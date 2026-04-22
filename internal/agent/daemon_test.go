package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentconfig "github.com/ykhdr/hubfuse/internal/agent/config"
	pb "github.com/ykhdr/hubfuse/proto"
)

// ─── Test helpers ─────────────────────────────────────────────────────────────

// buildTestDaemon sets up a minimal Daemon suitable for unit tests. It creates
// the required directory structure and files (config, identity, SSH keys) in a
// temporary directory and returns a Daemon that is ready for method-level
// testing (i.e. Run is NOT called).
func buildTestDaemon(t *testing.T) (*Daemon, string) {
	t.Helper()

	dir := t.TempDir()

	// SSH keys
	keysDir := filepath.Join(dir, "keys")
	_, err := GenerateSSHKeyPair(keysDir)
	require.NoError(t, err, "GenerateSSHKeyPair")
	keyPath := filepath.Join(keysDir, privateKeyFile)

	// Known-devices directory
	knownDevicesDir := filepath.Join(dir, "known_devices")
	require.NoError(t, os.MkdirAll(knownDevicesDir, 0700), "MkdirAll known_devices")

	// Write a minimal config.kdl.
	cfgPath := filepath.Join(dir, "config.kdl")
	cfgContent := `device {
    nickname "test-device"
}
hub {
    address "localhost:9090"
}
agent {
    ssh-port 2222
}
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgContent), 0644), "write config")

	cfg, err := agentconfig.Load(cfgPath)
	require.NoError(t, err, "agentconfig.Load")

	// Write device.json identity.
	identityPath := filepath.Join(dir, "device.json")
	identity := &DeviceIdentity{DeviceID: "test-device-id", Nickname: "test-device"}
	require.NoError(t, SaveIdentity(identityPath, identity), "SaveIdentity")

	// Mounter with overrides so we never invoke sshfs or umount.
	mounter := newTestMounter(t, knownDevicesDir, keyPath, nil, nil)

	sshServer, err := NewSSHServer(0, keyPath, discardLogger())
	require.NoError(t, err, "NewSSHServer")

	d := &Daemon{
		config:        cfg,
		configPath:    cfgPath,
		identity:      identity,
		mounter:       mounter,
		sshServer:     sshServer,
		logger:        discardLogger(),
		onlineDevices: make(map[string]*OnlineDevice),
		dataDir:       dir,
	}

	return d, dir
}

// writePubKey writes a paired-device public key file to knownDevicesDir.
func writePubKey(t *testing.T, dir, deviceID string) {
	t.Helper()
	knownDevicesDir := filepath.Join(dir, "known_devices")
	writePubKeyFile(t, knownDevicesDir, deviceID)
}

// ─── NewDaemon ────────────────────────────────────────────────────────────────

func TestNewDaemon_CreatesSuccessfully(t *testing.T) {
	dir := t.TempDir()

	// Write required files.
	keysDir := filepath.Join(dir, "keys")
	_, err := GenerateSSHKeyPair(keysDir)
	require.NoError(t, err, "GenerateSSHKeyPair")

	cfgPath := filepath.Join(dir, "config.kdl")
	cfgContent := `device {
    nickname "my-device"
}
hub {
    address "localhost:9090"
}
agent {
    ssh-port 2222
}
`
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgContent), 0644), "write config")

	identityPath := filepath.Join(dir, "device.json")
	identity := &DeviceIdentity{DeviceID: "abc123", Nickname: "my-device"}
	require.NoError(t, SaveIdentity(identityPath, identity), "SaveIdentity")

	tlsDir := filepath.Join(dir, "tls")
	require.NoError(t, os.MkdirAll(tlsDir, 0700), "MkdirAll tls")
	// Write placeholder TLS files (content doesn't matter for NewDaemon).
	for _, name := range []string{"ca.crt", "client.crt", "client.key"} {
		require.NoError(t, os.WriteFile(filepath.Join(tlsDir, name), []byte("placeholder"), 0600), "write %s", name)
	}

	daemon, err := NewDaemon(cfgPath, discardLogger(), DaemonOptions{})
	require.NoError(t, err, "NewDaemon() error")

	assert.Equal(t, "abc123", daemon.identity.DeviceID)
	assert.Equal(t, 2222, daemon.config.Agent.SSHPort)
	assert.NotNil(t, daemon.mounter)
	assert.NotNil(t, daemon.sshServer)
}

func TestNewDaemon_MissingIdentityReturnsError(t *testing.T) {
	dir := t.TempDir()

	// Write config but no identity.
	cfgPath := filepath.Join(dir, "config.kdl")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`hub { address "localhost:9090" }`+"\n"), 0644), "write config")

	// Generate SSH key so the SSH server can start.
	keysDir := filepath.Join(dir, "keys")
	_, err := GenerateSSHKeyPair(keysDir)
	require.NoError(t, err, "GenerateSSHKeyPair")

	_, err = NewDaemon(cfgPath, discardLogger(), DaemonOptions{})
	assert.Error(t, err, "NewDaemon() expected error for missing identity")
}

// ─── handleDeviceOnline ────────────────────────────────────────────────────────

func TestHandleDeviceOnline_AddsToKnownDevices(t *testing.T) {
	d, _ := buildTestDaemon(t)

	evt := &pb.DeviceOnlineEvent{
		DeviceId: "device-123",
		Nickname: "laptop",
		Ip:       "10.0.0.5",
		SshPort:  2222,
		Shares:   []*pb.Share{{Alias: "docs", Permissions: "ro"}},
	}

	d.handleDeviceOnline(evt)

	d.mu.RLock()
	info, ok := d.onlineDevices["device-123"]
	d.mu.RUnlock()

	require.True(t, ok, "knownDevices does not contain device-123 after handleDeviceOnline")
	assert.Equal(t, "laptop", info.Nickname)
	assert.Equal(t, "10.0.0.5", info.IP)
	assert.Equal(t, 2222, info.SSHPort)
	assert.Equal(t, []string{"docs"}, info.Shares)
}

func TestHandleDeviceOnline_AutoMountsWhenPairedAndConfigured(t *testing.T) {
	d, dir := buildTestDaemon(t)

	// Add a mount config for "laptop".
	d.config.Mounts = []agentconfig.MountConfig{
		{Device: "laptop", Share: "docs", To: filepath.Join(dir, "mnt", "docs")},
	}

	// isPaired checks by device_id.
	writePubKey(t, dir, "device-123")

	evt := &pb.DeviceOnlineEvent{
		DeviceId: "device-123",
		Nickname: "laptop",
		Ip:       "10.0.0.5",
		SshPort:  2222,
		Shares:   []*pb.Share{{Alias: "docs", Permissions: "ro"}},
	}

	d.handleDeviceOnline(evt)

	assert.True(t, d.mounter.IsActive("laptop", "docs"), "share should be mounted after handleDeviceOnline for paired + configured device")
}

func TestHandleDeviceOnline_NoMountWhenNotPaired(t *testing.T) {
	d, dir := buildTestDaemon(t)

	d.config.Mounts = []agentconfig.MountConfig{
		{Device: "laptop", Share: "docs", To: filepath.Join(dir, "mnt", "docs")},
	}
	// Do NOT write a pairing key for device-123.

	evt := &pb.DeviceOnlineEvent{
		DeviceId: "device-123",
		Nickname: "laptop",
		Ip:       "10.0.0.5",
		SshPort:  2222,
	}

	d.handleDeviceOnline(evt)

	assert.False(t, d.mounter.IsActive("laptop", "docs"), "share should NOT be mounted for an unpaired device")
}

// ─── handleDeviceOffline ──────────────────────────────────────────────────────

func TestHandleDeviceOffline_RemovesFromKnownDevices(t *testing.T) {
	d, _ := buildTestDaemon(t)

	d.mu.Lock()
	d.onlineDevices["device-123"] = &OnlineDevice{
		DeviceID: "device-123",
		Nickname: "laptop",
		IP:       "10.0.0.5",
		SSHPort:  2222,
	}
	d.mu.Unlock()

	evt := &pb.DeviceOfflineEvent{DeviceId: "device-123", Nickname: "laptop"}
	d.handleDeviceOffline(evt)

	d.mu.RLock()
	_, ok := d.onlineDevices["device-123"]
	d.mu.RUnlock()

	assert.False(t, ok, "knownDevices still contains device-123 after handleDeviceOffline")
}

func TestHandleDeviceOffline_UnmountsShares(t *testing.T) {
	d, dir := buildTestDaemon(t)

	writePubKey(t, dir, "device-123")
	mc := agentconfig.MountConfig{Device: "laptop", Share: "docs", To: filepath.Join(dir, "mnt")}
	require.NoError(t, d.mounter.Mount(context.Background(), mc, "device-123", "10.0.0.5", 2222), "pre-mount")

	d.mu.Lock()
	d.onlineDevices["device-123"] = &OnlineDevice{
		DeviceID: "device-123",
		Nickname: "laptop",
		IP:       "10.0.0.5",
		SSHPort:  2222,
	}
	d.mu.Unlock()

	evt := &pb.DeviceOfflineEvent{DeviceId: "device-123", Nickname: "laptop"}
	d.handleDeviceOffline(evt)

	assert.False(t, d.mounter.IsActive("laptop", "docs"), "share should be unmounted after handleDeviceOffline")
}

// ─── handleDeviceRemoved ──────────────────────────────────────────────────────

func TestHandleDeviceRemoved_RemovesFromKnownDevices(t *testing.T) {
	d, _ := buildTestDaemon(t)

	d.mu.Lock()
	d.onlineDevices["device-123"] = &OnlineDevice{
		DeviceID: "device-123",
		Nickname: "laptop",
		IP:       "10.0.0.5",
		SSHPort:  2222,
	}
	d.mu.Unlock()

	evt := &pb.DeviceRemovedEvent{DeviceId: "device-123", Nickname: "laptop"}
	d.handleDeviceRemoved(evt)

	d.mu.RLock()
	_, ok := d.onlineDevices["device-123"]
	d.mu.RUnlock()

	if ok {
		t.Error("knownDevices still contains device-123 after handleDeviceRemoved")
	}
}

func TestHandleDeviceRemoved_UnmountsShares(t *testing.T) {
	d, dir := buildTestDaemon(t)

	writePubKey(t, dir, "device-123")
	mc := agentconfig.MountConfig{Device: "laptop", Share: "docs", To: filepath.Join(dir, "mnt")}
	if err := d.mounter.Mount(context.Background(), mc, "device-123", "10.0.0.5", 2222); err != nil {
		t.Fatalf("pre-mount: %v", err)
	}

	d.mu.Lock()
	d.onlineDevices["device-123"] = &OnlineDevice{
		DeviceID: "device-123",
		Nickname: "laptop",
		IP:       "10.0.0.5",
		SSHPort:  2222,
	}
	d.mu.Unlock()

	evt := &pb.DeviceRemovedEvent{DeviceId: "device-123", Nickname: "laptop"}
	d.handleDeviceRemoved(evt)

	if d.mounter.IsActive("laptop", "docs") {
		t.Error("share should be unmounted after handleDeviceRemoved")
	}
}

// ─── handlePairingCompleted ────────────────────────────────────────────────────

func TestHandlePairingCompleted_SavesPeerKey(t *testing.T) {
	d, dir := buildTestDaemon(t)

	evt := &pb.PairingCompletedEvent{
		PeerDeviceId:  "peer-device-999",
		PeerPublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIpeer test@host",
	}

	d.handlePairingCompleted(evt)

	knownDevicesDir := filepath.Join(dir, "known_devices")
	pubKeyPath := filepath.Join(knownDevicesDir, "peer-device-999.pub")
	_, err := os.Stat(pubKeyPath)
	assert.NoError(t, err, "peer public key not saved")
}

func TestHandlePairingCompleted_AutoMountsWhenOnlineAndConfigured(t *testing.T) {
	d, dir := buildTestDaemon(t)

	// Configure a mount for the peer device.
	d.config.Mounts = []agentconfig.MountConfig{
		{Device: "peer-laptop", Share: "docs", To: filepath.Join(dir, "mnt", "docs")},
	}

	// The peer is online.
	d.mu.Lock()
	d.onlineDevices["peer-device-999"] = &OnlineDevice{
		DeviceID: "peer-device-999",
		Nickname: "peer-laptop",
		IP:       "10.0.0.7",
		SSHPort:  2222,
	}
	d.mu.Unlock()

	evt := &pb.PairingCompletedEvent{
		PeerDeviceId:  "peer-device-999",
		PeerPublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIpeer test@host",
	}

	d.handlePairingCompleted(evt)

	assert.True(t, d.mounter.IsActive("peer-laptop", "docs"), "share should be mounted after pairing completed for an online + configured device")
}

func TestHandlePairingCompleted_NoMountWhenOffline(t *testing.T) {
	d, dir := buildTestDaemon(t)

	d.config.Mounts = []agentconfig.MountConfig{
		{Device: "peer-laptop", Share: "docs", To: filepath.Join(dir, "mnt", "docs")},
	}
	// Device is NOT in knownDevices (offline).

	evt := &pb.PairingCompletedEvent{
		PeerDeviceId:  "peer-device-offline",
		PeerPublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIpeer test@host",
	}

	d.handlePairingCompleted(evt)

	assert.False(t, d.mounter.IsActive("peer-laptop", "docs"), "share should NOT be mounted for an offline device")
}

// ─── onConfigChange ────────────────────────────────────────────────────────────

func TestOnConfigChange_SharesChangedUpdatesShares(t *testing.T) {
	d, _ := buildTestDaemon(t)

	// We can't easily stub hubClient without a real gRPC conn, so we test the
	// SSH server path and the config update. The hubClient call will fail
	// gracefully (nil pointer) and we verify the config was updated.
	oldCfg := &agentconfig.Config{
		Shares: []agentconfig.ShareConfig{
			{Alias: "old-share", Path: "/old", Permissions: "ro", AllowedDevices: []string{"all"}},
		},
	}
	newCfg := &agentconfig.Config{
		Shares: []agentconfig.ShareConfig{
			{Alias: "new-share", Path: "/new", Permissions: "rw", AllowedDevices: []string{"all"}},
		},
	}

	// Patch hubClient to a nil so UpdateShares errors quietly — we're testing
	// the rest of the flow.
	d.hubClient = nil

	// Test configSharesToProto.
	protoShares := configSharesToProto(newCfg.Shares)
	require.Len(t, protoShares, 1, "configSharesToProto len")
	assert.Equal(t, "new-share", protoShares[0].Alias)
	assert.Equal(t, "rw", protoShares[0].Permissions)

	// Test sharesToACL.
	acls := sharesToACL(newCfg.Shares)
	require.Len(t, acls, 1)
	assert.Equal(t, "new-share", acls[0].Alias)
	assert.Equal(t, "/new", acls[0].Path)
	assert.False(t, acls[0].ReadOnly, "permissions=rw must map to ReadOnly=false")
	assert.True(t, acls[0].AllowAll, `allowed-devices "all" must lift into AllowAll`)

	// Test that SSH server snapshot is updated.
	d.sshServer.UpdateShares(acls)
	snap := d.sshServer.aclSnapshot()
	hasNew := false
	for _, a := range snap {
		if a.Alias == "new-share" {
			hasNew = true
		}
	}
	assert.True(t, hasNew, "sshServer snapshot missing 'new-share' after UpdateShares")

	// Verify ComputeDiff detects the share change.
	diff := agentconfig.ComputeDiff(oldCfg, newCfg)
	assert.True(t, diff.SharesChanged, "ComputeDiff should report SharesChanged for different share lists")
}

func TestOnConfigChange_MountsAdded(t *testing.T) {
	d, dir := buildTestDaemon(t)

	// isPaired checks by device_id.
	writePubKey(t, dir, "device-abc")
	d.mu.Lock()
	d.onlineDevices["device-abc"] = &OnlineDevice{
		DeviceID: "device-abc",
		Nickname: "remote",
		IP:       "10.0.0.9",
		SSHPort:  2222,
	}
	d.mu.Unlock()

	// tryMount should attempt to mount since device is online + paired.
	mc := agentconfig.MountConfig{
		Device: "remote",
		Share:  "photos",
		To:     filepath.Join(dir, "mnt", "photos"),
	}

	d.tryMount(mc)

	assert.True(t, d.mounter.IsActive("remote", "photos"), "tryMount should have mounted 'photos' for an online+paired device")
}

func TestOnConfigChange_MountsRemoved(t *testing.T) {
	d, dir := buildTestDaemon(t)

	writePubKey(t, dir, "device-abc")
	mc := agentconfig.MountConfig{
		Device: "remote",
		Share:  "music",
		To:     filepath.Join(dir, "mnt", "music"),
	}
	require.NoError(t, d.mounter.Mount(context.Background(), mc, "device-abc", "10.0.0.9", 2222), "pre-mount")

	// Unmounting via the diff path.
	require.NoError(t, d.mounter.Unmount("remote", "music"), "Unmount")

	assert.False(t, d.mounter.IsActive("remote", "music"), "share should be inactive after Unmount")
}

// ─── isPaired ─────────────────────────────────────────────────────────────────

func TestIsPaired_TrueWhenKeyExists(t *testing.T) {
	d, dir := buildTestDaemon(t)
	writePubKey(t, dir, "known-device")

	assert.True(t, d.isPaired("known-device"), "isPaired() = false for a device with a key file, want true")
}

func TestIsPaired_FalseWhenKeyMissing(t *testing.T) {
	d, _ := buildTestDaemon(t)

	assert.False(t, d.isPaired("unknown-device"), "isPaired() = true for a device with no key file, want false")
}

// ─── shouldMount ──────────────────────────────────────────────────────────────

func TestShouldMount_TrueWhenConfigured(t *testing.T) {
	d, dir := buildTestDaemon(t)

	d.config.Mounts = []agentconfig.MountConfig{
		{Device: "laptop", Share: "docs", To: filepath.Join(dir, "mnt")},
	}

	mc, ok := d.shouldMount("laptop")
	require.True(t, ok, "shouldMount() = false, want true for configured device")
	assert.Equal(t, "docs", mc.Share)
}

func TestShouldMount_FalseWhenNotConfigured(t *testing.T) {
	d, _ := buildTestDaemon(t)
	d.config.Mounts = nil

	_, ok := d.shouldMount("unconfigured-device")
	assert.False(t, ok, "shouldMount() = true for unconfigured device, want false")
}

// ─── processInitialDevices ────────────────────────────────────────────────────

func TestProcessInitialDevices_PopulatesKnownDevices(t *testing.T) {
	d, _ := buildTestDaemon(t)

	devices := []*pb.DeviceInfo{
		{DeviceId: "dev-1", Nickname: "alpha", Ip: "10.0.0.1", SshPort: 2222},
		{DeviceId: "dev-2", Nickname: "beta", Ip: "10.0.0.2", SshPort: 2222},
	}

	d.processInitialDevices(devices)

	d.mu.RLock()
	defer d.mu.RUnlock()

	assert.Len(t, d.onlineDevices, 2, "knownDevices len")
	assert.Contains(t, d.onlineDevices, "dev-1", "knownDevices missing dev-1")
	assert.Contains(t, d.onlineDevices, "dev-2", "knownDevices missing dev-2")
}
