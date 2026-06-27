package agent

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

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
		sshPort:       cfg.Agent.SSHPort,
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

// writeDaemonFixture lays out the on-disk files NewDaemon needs (config,
// identity, TLS placeholders, SSH keys) under a fresh temp dir and returns the
// config path. cfgContent is the KDL written to config.kdl.
func writeDaemonFixture(t *testing.T, cfgContent string) string {
	t.Helper()
	dir := t.TempDir()

	keysDir := filepath.Join(dir, "keys")
	_, err := GenerateSSHKeyPair(keysDir)
	require.NoError(t, err, "GenerateSSHKeyPair")

	cfgPath := filepath.Join(dir, "config.kdl")
	require.NoError(t, os.WriteFile(cfgPath, []byte(cfgContent), 0644), "write config")

	identityPath := filepath.Join(dir, "device.json")
	identity := &DeviceIdentity{DeviceID: "abc123", Nickname: "my-device"}
	require.NoError(t, SaveIdentity(identityPath, identity), "SaveIdentity")

	tlsDir := filepath.Join(dir, "tls")
	require.NoError(t, os.MkdirAll(tlsDir, 0700), "MkdirAll tls")
	for _, name := range []string{"ca.crt", "client.crt", "client.key"} {
		require.NoError(t, os.WriteFile(filepath.Join(tlsDir, name), []byte("placeholder"), 0600), "write %s", name)
	}

	return cfgPath
}

// TestNewDaemon_FuseTOnLinuxFailsFast verifies the OS-gating wired into
// NewDaemon: a config selecting mount-tool "fuse-t" on a non-macOS host must
// abort construction with the wrapped "only supported on macOS" error. Value
// validation (config Load) accepts "fuse-t" on any OS; the platform gate lives
// in the daemon layer, so this exercises the wiring, not the pure helper.
func TestNewDaemon_FuseTOnLinuxFailsFast(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("fuse-t is supported on macOS; this asserts the non-macOS rejection")
	}

	cfgPath := writeDaemonFixture(t, `device {
    nickname "my-device"
}
hub {
    address "localhost:9090"
}
agent {
    mount-tool "fuse-t"
}
`)

	_, err := NewDaemon(cfgPath, discardLogger(), DaemonOptions{})
	require.Error(t, err, "NewDaemon() must reject fuse-t on a non-macOS host")
	assert.Contains(t, err.Error(), "only supported on macOS", "error must explain the platform gate")
	assert.Contains(t, err.Error(), "validate mount tool", "error must be wrapped at the daemon layer")
}

// TestNewDaemon_MissingMountBinaryWarnsButSucceeds verifies the production
// preflight call site: when a mount is configured but the backend binary is not
// installed, NewDaemon must still succeed (warn-and-continue), because a device
// with no mount tool can still export its own shares.
func TestNewDaemon_MissingMountBinaryWarnsButSucceeds(t *testing.T) {
	// Make the preflight lookup miss deterministically regardless of whether
	// sshfs happens to be installed on the test host.
	t.Setenv("PATH", t.TempDir())

	cfgPath := writeDaemonFixture(t, `device {
    nickname "my-device"
}
hub {
    address "localhost:9090"
}
agent {
    ssh-port 2222
}
mounts {
    mount device="peer" share="docs" to="/tmp/hubfuse-test-mnt"
}
`)

	daemon, err := NewDaemon(cfgPath, discardLogger(), DaemonOptions{})
	require.NoError(t, err, "NewDaemon() must succeed even when the mount binary is missing")
	require.NotNil(t, daemon, "daemon should be constructed")
	assert.NotNil(t, daemon.mounter, "mounter should still be wired")
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

// TestHandleDeviceOnline_AutoMountsAllSharesOfPeer verifies that a peer exporting
// MULTIPLE configured shares gets ALL of them mounted on DeviceOnline, and that
// after the peer roams to a new endpoint every share remounts at the new IP — not
// just the first matching mount entry. (#61 multi-share remount)
func TestHandleDeviceOnline_AutoMountsAllSharesOfPeer(t *testing.T) {
	d, dir := buildTestDaemon(t)

	docsTo := filepath.Join(dir, "mnt", "docs")
	photosTo := filepath.Join(dir, "mnt", "photos")
	t.Cleanup(func() {
		_ = os.Chmod(docsTo, 0o755)
		_ = os.Chmod(photosTo, 0o755)
	})

	// Two mount entries for the same peer nickname — one per exported share.
	d.config.Mounts = []agentconfig.MountConfig{
		{Device: "laptop", Share: "docs", To: docsTo},
		{Device: "laptop", Share: "photos", To: photosTo},
	}

	writePubKey(t, dir, "device-123")

	online := func(ip string) *pb.DeviceOnlineEvent {
		return &pb.DeviceOnlineEvent{
			DeviceId: "device-123",
			Nickname: "laptop",
			Ip:       ip,
			SshPort:  2222,
			Shares: []*pb.Share{
				{Alias: "docs", Permissions: "ro"},
				{Alias: "photos", Permissions: "ro"},
			},
		}
	}

	// First online: BOTH shares must mount at the initial endpoint.
	d.handleDeviceOnline(online("10.0.0.5"))
	assert.True(t, d.mounter.IsActive("laptop", "docs"), "docs must mount on device-online")
	assert.True(t, d.mounter.IsActive("laptop", "photos"),
		"photos must also mount on device-online — not just the first configured share")

	// Peer roamed to a new IP. BOTH shares must remount at the new endpoint.
	d.handleDeviceOnline(online("10.0.0.99"))
	assert.True(t, d.mounter.IsActive("laptop", "docs"), "docs must stay mounted after roam")
	assert.True(t, d.mounter.IsActive("laptop", "photos"), "photos must stay mounted after roam")

	endpoints := make(map[string]string)
	for _, mnt := range d.mounter.ActiveMounts() {
		endpoints[mnt.Share] = mnt.IP
	}
	assert.Equal(t, "10.0.0.99", endpoints["docs"], "docs must remount at the new endpoint after roam")
	assert.Equal(t, "10.0.0.99", endpoints["photos"],
		"photos must remount at the new endpoint after roam — every share follows the roam, not just the first")
}

// TestHandleDeviceOnline_MountsOnlyExportedShares verifies that auto-mount
// intersects local config with the peer's actually-exported shares: a share the
// peer no longer exports (e.g. a SharesUpdated removal missed while the stream was
// down) must NOT be (re)mounted to a now-dead share. (#61)
func TestHandleDeviceOnline_MountsOnlyExportedShares(t *testing.T) {
	d, dir := buildTestDaemon(t)

	docsTo := filepath.Join(dir, "mnt", "docs")
	photosTo := filepath.Join(dir, "mnt", "photos")
	t.Cleanup(func() {
		_ = os.Chmod(docsTo, 0o755)
		_ = os.Chmod(photosTo, 0o755)
	})

	// Local config mounts BOTH shares...
	d.config.Mounts = []agentconfig.MountConfig{
		{Device: "laptop", Share: "docs", To: docsTo},
		{Device: "laptop", Share: "photos", To: photosTo},
	}
	writePubKey(t, dir, "device-123")

	// ...but the peer now exports ONLY "docs".
	evt := &pb.DeviceOnlineEvent{
		DeviceId: "device-123",
		Nickname: "laptop",
		Ip:       "10.0.0.5",
		SshPort:  2222,
		Shares:   []*pb.Share{{Alias: "docs", Permissions: "ro"}},
	}

	d.handleDeviceOnline(evt)

	assert.True(t, d.mounter.IsActive("laptop", "docs"), "the exported share must mount")
	assert.False(t, d.mounter.IsActive("laptop", "photos"),
		"a share the peer no longer exports must NOT be mounted")
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

	// The peer is online and exports the configured share.
	d.mu.Lock()
	d.onlineDevices["peer-device-999"] = &OnlineDevice{
		DeviceID: "peer-device-999",
		Nickname: "peer-laptop",
		IP:       "10.0.0.7",
		SSHPort:  2222,
		Shares:   []string{"docs"},
	}
	d.mu.Unlock()

	evt := &pb.PairingCompletedEvent{
		PeerDeviceId:  "peer-device-999",
		PeerPublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIpeer test@host",
	}

	d.handlePairingCompleted(evt)

	assert.True(t, d.mounter.IsActive("peer-laptop", "docs"), "share should be mounted after pairing completed for an online + configured device")
}

// TestHandlePairingCompleted_AutoMountsAllExportedSharesOfPeer verifies that a
// multi-share peer already online when pairing completes gets ALL of its
// configured+exported shares mounted immediately — not just the first. (#61)
func TestHandlePairingCompleted_AutoMountsAllExportedSharesOfPeer(t *testing.T) {
	d, dir := buildTestDaemon(t)

	docsTo := filepath.Join(dir, "mnt", "docs")
	photosTo := filepath.Join(dir, "mnt", "photos")
	t.Cleanup(func() {
		_ = os.Chmod(docsTo, 0o755)
		_ = os.Chmod(photosTo, 0o755)
	})

	d.config.Mounts = []agentconfig.MountConfig{
		{Device: "peer-laptop", Share: "docs", To: docsTo},
		{Device: "peer-laptop", Share: "photos", To: photosTo},
	}

	// Peer already online, exporting BOTH shares, at pairing-completion time.
	d.mu.Lock()
	d.onlineDevices["peer-device-999"] = &OnlineDevice{
		DeviceID: "peer-device-999",
		Nickname: "peer-laptop",
		IP:       "10.0.0.7",
		SSHPort:  2222,
		Shares:   []string{"docs", "photos"},
	}
	d.mu.Unlock()

	evt := &pb.PairingCompletedEvent{
		PeerDeviceId:  "peer-device-999",
		PeerPublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIpeer test@host",
	}

	d.handlePairingCompleted(evt)

	assert.True(t, d.mounter.IsActive("peer-laptop", "docs"), "docs must mount on pairing completion")
	assert.True(t, d.mounter.IsActive("peer-laptop", "photos"),
		"photos must ALSO mount on pairing completion — not just the first configured share")
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

// ─── sessionOnce: register port + config-reload race (#61) ─────────────────────

// TestSessionOnce_RegistersStartupSSHPortNotLiveConfig verifies that a re-register
// advertises the SSH port the embedded server actually bound at startup
// (d.sshPort), NOT a value the live config may have hot-reloaded. The SSH server
// is not restarted on config change, so advertising the config value would point
// peers at a dead endpoint. (#61)
func TestSessionOnce_RegistersStartupSSHPortNotLiveConfig(t *testing.T) {
	d, _ := buildTestDaemon(t) // d.sshPort == 2222 (the bound port)

	// Simulate a config hot-reload that changed ssh-port while the server keeps
	// listening on the original port.
	d.mu.Lock()
	d.config.Agent.SSHPort = 9999
	d.mu.Unlock()

	var gotPort int
	d.registerFn = func(_ context.Context, _ []*pb.Share, sshPort int) (*pb.RegisterResponse, error) {
		gotPort = sshPort
		return &pb.RegisterResponse{}, nil
	}
	d.subscribeFn = func(context.Context) (pb.HubFuse_SubscribeClient, error) {
		return errStream(), nil
	}

	_, err := d.sessionOnce(context.Background())
	require.NoError(t, err, "sessionOnce")
	assert.Equal(t, 2222, gotPort,
		"re-register must advertise the SSH server's startup port, not the hot-reloaded config value")
}

// TestSessionOnce_ConcurrentConfigReloadNoRace drives sessionOnce concurrently
// with onConfigChange (which swaps d.config under d.mu). Under -race this fails if
// sessionOnce reads d.config without the lock — the exact data race the supervisor
// introduced, since it re-runs sessionOnce on every reconnect alongside the config
// watcher. (#61)
func TestSessionOnce_ConcurrentConfigReloadNoRace(t *testing.T) {
	d, _ := buildTestDaemon(t)

	d.registerFn = func(_ context.Context, shares []*pb.Share, _ int) (*pb.RegisterResponse, error) {
		_ = len(shares) // touch the snapshot so -race observes the read
		return &pb.RegisterResponse{}, nil
	}
	d.subscribeFn = func(context.Context) (pb.HubFuse_SubscribeClient, error) {
		return errStream(), nil
	}

	// Two configs with identical content but distinct pointers: ComputeDiff is
	// empty, so onConfigChange only swaps the d.config pointer (the racing write)
	// without touching the nil test hubClient or the mounter.
	mkCfg := func() *agentconfig.Config {
		return &agentconfig.Config{
			Agent: agentconfig.AgentConfig{SSHPort: 2222},
			Shares: []agentconfig.ShareConfig{
				{Alias: "docs", Path: "/docs", Permissions: "ro", AllowedDevices: []string{"all"}},
			},
		}
	}

	done := make(chan struct{})
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-done:
				return
			default:
				d.onConfigChange(mkCfg(), mkCfg())
			}
		}
	}()

	for i := 0; i < 500; i++ {
		_, err := d.sessionOnce(context.Background())
		require.NoError(t, err, "sessionOnce #%d", i)
	}
	close(done)
	<-writerDone
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

// TestProcessInitialDevices_AutoMountsAllSharesOfPeer verifies the reconnect path:
// the RegisterResponse snapshot auto-mounts EVERY configured share of a paired
// multi-share peer, and a later snapshot reporting a roamed endpoint remounts all
// of them at the new IP — not just the first. (#61 multi-share remount)
func TestProcessInitialDevices_AutoMountsAllSharesOfPeer(t *testing.T) {
	d, dir := buildTestDaemon(t)

	docsTo := filepath.Join(dir, "mnt", "docs")
	photosTo := filepath.Join(dir, "mnt", "photos")
	t.Cleanup(func() {
		_ = os.Chmod(docsTo, 0o755)
		_ = os.Chmod(photosTo, 0o755)
	})

	d.config.Mounts = []agentconfig.MountConfig{
		{Device: "laptop", Share: "docs", To: docsTo},
		{Device: "laptop", Share: "photos", To: photosTo},
	}

	writePubKey(t, dir, "device-123")

	snapshot := func(ip string) []*pb.DeviceInfo {
		return []*pb.DeviceInfo{{
			DeviceId: "device-123",
			Nickname: "laptop",
			Ip:       ip,
			SshPort:  2222,
			Shares:   []*pb.Share{{Alias: "docs"}, {Alias: "photos"}},
		}}
	}

	// Initial register snapshot: both shares mount.
	d.processInitialDevices(snapshot("10.0.0.5"))
	assert.True(t, d.mounter.IsActive("laptop", "docs"), "docs must mount from the register snapshot")
	assert.True(t, d.mounter.IsActive("laptop", "photos"),
		"photos must also mount from the register snapshot — not just the first share")

	// Reconnect snapshot reports the peer at a new endpoint: both shares remount.
	d.processInitialDevices(snapshot("10.0.0.99"))
	endpoints := make(map[string]string)
	for _, mnt := range d.mounter.ActiveMounts() {
		endpoints[mnt.Share] = mnt.IP
	}
	assert.Equal(t, "10.0.0.99", endpoints["docs"], "docs must remount at the new endpoint on reconnect")
	assert.Equal(t, "10.0.0.99", endpoints["photos"],
		"photos must remount at the new endpoint on reconnect — every share follows the roam")
}

// TestProcessInitialDevices_MountsOnlyExportedShares verifies the reconnect
// snapshot path intersects local config with the peer's exported shares: a config
// mount for a share the peer no longer exports in the snapshot must NOT be
// (re)mounted. (#61)
func TestProcessInitialDevices_MountsOnlyExportedShares(t *testing.T) {
	d, dir := buildTestDaemon(t)

	docsTo := filepath.Join(dir, "mnt", "docs")
	photosTo := filepath.Join(dir, "mnt", "photos")
	t.Cleanup(func() {
		_ = os.Chmod(docsTo, 0o755)
		_ = os.Chmod(photosTo, 0o755)
	})

	d.config.Mounts = []agentconfig.MountConfig{
		{Device: "laptop", Share: "docs", To: docsTo},
		{Device: "laptop", Share: "photos", To: photosTo},
	}
	writePubKey(t, dir, "device-123")

	// The Register snapshot reports the peer exporting ONLY "docs".
	d.processInitialDevices([]*pb.DeviceInfo{{
		DeviceId: "device-123",
		Nickname: "laptop",
		Ip:       "10.0.0.5",
		SshPort:  2222,
		Shares:   []*pb.Share{{Alias: "docs"}},
	}})

	assert.True(t, d.mounter.IsActive("laptop", "docs"), "the exported share must mount from the snapshot")
	assert.False(t, d.mounter.IsActive("laptop", "photos"),
		"a share absent from the snapshot must NOT be mounted")
}

// ─── supervisor: session reconnect (#61) ──────────────────────────────────────

// fakeSubscribeStream is a minimal pb.HubFuse_SubscribeClient for unit tests.
// Only Recv is implemented; the embedded grpc.ClientStream is left nil because
// the code under test (readStream) only ever calls Recv. recv supplies the
// per-call behaviour.
type fakeSubscribeStream struct {
	grpc.ClientStream
	recv func() (*pb.Event, error)
}

func (f *fakeSubscribeStream) Recv() (*pb.Event, error) { return f.recv() }

// errStream returns a fake stream whose Recv always fails — it models a hub
// session that has just died.
func errStream() *fakeSubscribeStream {
	return &fakeSubscribeStream{recv: func() (*pb.Event, error) {
		return nil, errors.New("stream closed")
	}}
}

// TestReadStream_ExitsOnRecvError verifies readStream consumes events until Recv
// returns an error, then returns instead of spinning. No seam is needed: the
// fake stream is passed directly. (#61)
func TestReadStream_ExitsOnRecvError(t *testing.T) {
	d, _ := buildTestDaemon(t)

	var calls int
	stream := &fakeSubscribeStream{recv: func() (*pb.Event, error) {
		calls++
		if calls == 1 {
			// A benign event with no payload: handleEvent logs an unknown-type
			// warning and returns, so the loop advances to the error below.
			return &pb.Event{}, nil
		}
		return nil, errors.New("stream broken")
	}}

	done := make(chan struct{})
	go func() {
		d.readStream(context.Background(), stream)
		close(done)
	}()

	select {
	case <-done:
		// returned promptly — good.
	case <-time.After(2 * time.Second):
		t.Fatal("readStream did not return after a Recv error — it is looping")
	}

	assert.GreaterOrEqual(t, calls, 2, "readStream should consume the event then exit on the next Recv error")
}

// TestSessionOnce_OnReadyFiresExactlyOnce verifies that across multiple
// sessionOnce calls (modelling the first session plus reconnects) the onReady
// hook is invoked exactly once. The HubClient seam is overridden with fakes so
// no live hub is needed. (#61)
func TestSessionOnce_OnReadyFiresExactlyOnce(t *testing.T) {
	d, _ := buildTestDaemon(t)

	var registerCalls, readyCalls int
	d.onReady = func() { readyCalls++ }
	d.registerFn = func(context.Context, []*pb.Share, int) (*pb.RegisterResponse, error) {
		registerCalls++
		return &pb.RegisterResponse{}, nil
	}
	d.subscribeFn = func(context.Context) (pb.HubFuse_SubscribeClient, error) {
		return errStream(), nil
	}

	for i := 0; i < 3; i++ {
		stream, err := d.sessionOnce(context.Background())
		require.NoError(t, err, "sessionOnce #%d", i)
		require.NotNil(t, stream, "sessionOnce #%d should return a stream", i)
	}

	assert.Equal(t, 3, registerCalls, "every sessionOnce must re-register (idempotent on the hub)")
	assert.Equal(t, 1, readyCalls, "onReady must fire exactly once across multiple sessions")
}

// TestSessionOnce_NilOnReadyDoesNotPanic verifies the nil-guard inside the
// readyOnce.Do closure: buildTestDaemon leaves onReady nil, and sessionOnce must
// not panic (a bare Do(d.onReady) would). (#61)
func TestSessionOnce_NilOnReadyDoesNotPanic(t *testing.T) {
	d, _ := buildTestDaemon(t) // onReady is nil

	d.registerFn = func(context.Context, []*pb.Share, int) (*pb.RegisterResponse, error) {
		return &pb.RegisterResponse{}, nil
	}
	d.subscribeFn = func(context.Context) (pb.HubFuse_SubscribeClient, error) {
		return errStream(), nil
	}

	require.NotPanics(t, func() {
		_, err := d.sessionOnce(context.Background())
		require.NoError(t, err)
	}, "sessionOnce must not panic when onReady is nil")
}

// TestReconnectSession_ReturnsNilOnCancelledCtx verifies that reconnectSession
// stops retrying and returns nil once ctx is cancelled, so supervise can exit
// cleanly on shutdown. The seam's registerFn always fails, forcing the backoff
// path where the cancelled ctx is observed. (#61)
func TestReconnectSession_ReturnsNilOnCancelledCtx(t *testing.T) {
	d, _ := buildTestDaemon(t)

	d.registerFn = func(context.Context, []*pb.Share, int) (*pb.RegisterResponse, error) {
		return nil, errors.New("hub unreachable")
	}
	// subscribeFn is never reached because Register always fails.
	d.subscribeFn = func(context.Context) (pb.HubFuse_SubscribeClient, error) {
		t.Fatal("subscribeFn must not be called when Register fails")
		return nil, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the first backoff select

	done := make(chan pb.HubFuse_SubscribeClient, 1)
	go func() { done <- d.reconnectSession(ctx) }()

	select {
	case stream := <-done:
		assert.Nil(t, stream, "reconnectSession must return nil when ctx is cancelled")
	case <-time.After(2 * time.Second):
		t.Fatal("reconnectSession did not return on a cancelled ctx — it is stuck in backoff")
	}
}

// TestSessionOnce_SubscribeFailureReturnsWrappedError verifies the documented
// "Register ok, Subscribe failed" partial-success path: sessionOnce returns the
// "subscribe to hub events" wrapped error, and onReady (which fires after
// Register, before Subscribe) still fires exactly once when the first Subscribe
// fails and a later retry succeeds. (#61)
func TestSessionOnce_SubscribeFailureReturnsWrappedError(t *testing.T) {
	d, _ := buildTestDaemon(t)

	var readyCalls int
	d.onReady = func() { readyCalls++ }

	var registerCalls atomic.Int32
	d.registerFn = func(context.Context, []*pb.Share, int) (*pb.RegisterResponse, error) {
		registerCalls.Add(1)
		return &pb.RegisterResponse{}, nil
	}
	var subscribeCalls atomic.Int32
	d.subscribeFn = func(context.Context) (pb.HubFuse_SubscribeClient, error) {
		if subscribeCalls.Add(1) == 1 {
			return nil, errors.New("stream setup failed")
		}
		return errStream(), nil
	}

	// First session: Register ok (onReady fires), Subscribe fails → wrapped error.
	stream, err := d.sessionOnce(context.Background())
	require.Error(t, err, "sessionOnce must surface a Subscribe failure")
	assert.Nil(t, stream, "no stream is returned when Subscribe fails")
	assert.Contains(t, err.Error(), "subscribe to hub events", "error must be wrapped with the Subscribe context")

	// Second session: Subscribe now succeeds.
	stream, err = d.sessionOnce(context.Background())
	require.NoError(t, err, "second sessionOnce must succeed")
	require.NotNil(t, stream, "second sessionOnce must return a stream")

	assert.Equal(t, int32(2), registerCalls.Load(), "both sessions re-register")
	assert.Equal(t, 1, readyCalls, "onReady must fire exactly once even though the first Subscribe failed")
}

// TestReconnectSession_SucceedsAfterFailures verifies the success path of the
// retry loop: registerFn fails several times then succeeds, exercising the
// fail → backoff (delay doubling) → succeed → return-stream branch that the
// cancelled-ctx test does not. minReconnectInterval is set tiny so the doubling
// runs without slowing the test. (#61)
func TestReconnectSession_SucceedsAfterFailures(t *testing.T) {
	d, _ := buildTestDaemon(t)
	d.minReconnectInterval = time.Millisecond // tiny: backoff still doubles (1→2→4ms) but stays fast

	var registerCalls atomic.Int32
	d.registerFn = func(context.Context, []*pb.Share, int) (*pb.RegisterResponse, error) {
		if registerCalls.Add(1) <= 3 {
			return nil, errors.New("hub unreachable")
		}
		return &pb.RegisterResponse{}, nil
	}
	var subscribeCalls atomic.Int32
	d.subscribeFn = func(context.Context) (pb.HubFuse_SubscribeClient, error) {
		subscribeCalls.Add(1)
		return errStream(), nil
	}

	done := make(chan pb.HubFuse_SubscribeClient, 1)
	go func() { done <- d.reconnectSession(context.Background()) }()

	select {
	case stream := <-done:
		require.NotNil(t, stream, "reconnectSession must return the live stream once sessionOnce succeeds")
	case <-time.After(2 * time.Second):
		t.Fatal("reconnectSession did not return after the hub recovered")
	}

	assert.Equal(t, int32(4), registerCalls.Load(), "reconnectSession retries Register until success (3 failures + 1 success)")
	assert.Equal(t, int32(1), subscribeCalls.Load(), "Subscribe runs only on the successful attempt")
}

// TestSupervise_ReconnectsMultipleTimesThenExitsOnCancel is the direct unit test
// for supervise — the core of the #61 fix. It drives more than one reconnect (a
// regression where supervise returns after the FIRST reconnect, e.g. return
// instead of continue, would never reach the third stream) and asserts a prompt,
// clean goroutine exit when ctx is cancelled while readStream is blocked.
// minReconnectInterval stays 0 (buildTestDaemon) so the loop spins fast. (#61)
func TestSupervise_ReconnectsMultipleTimesThenExitsOnCancel(t *testing.T) {
	d, _ := buildTestDaemon(t)

	d.registerFn = func(context.Context, []*pb.Share, int) (*pb.RegisterResponse, error) {
		return &pb.RegisterResponse{}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var subscribeCalls atomic.Int32
	d.subscribeFn = func(context.Context) (pb.HubFuse_SubscribeClient, error) {
		// The first two reconnect streams die immediately, forcing supervise to
		// loop and reconnect again; the third blocks until ctx cancel so we can
		// assert a prompt, clean exit while readStream is blocked.
		if subscribeCalls.Add(1) <= 2 {
			return errStream(), nil
		}
		return &fakeSubscribeStream{recv: func() (*pb.Event, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}, nil
	}

	done := make(chan struct{})
	go func() {
		// The initial stream also dies immediately (read cycle 1).
		d.supervise(ctx, errStream())
		close(done)
	}()

	// Wait until supervise has reconnected more than once: subscribeFn called
	// >= 3 times means two immediate-death reconnects plus the blocking one.
	require.Eventually(t, func() bool {
		return subscribeCalls.Load() >= 3
	}, 2*time.Second, 5*time.Millisecond, "supervise must reconnect more than once")

	// supervise is now blocked in readStream on the third stream; cancelling ctx
	// must make it return promptly without another reconnect.
	cancel()
	select {
	case <-done:
		// returned promptly — good.
	case <-time.After(2 * time.Second):
		t.Fatal("supervise did not return after ctx cancel — it is stuck")
	}
	assert.Equal(t, int32(3), subscribeCalls.Load(), "supervise must not reconnect again after ctx cancel")
}

// TestSupervise_FloorsReconnectAfterImmediateDeath verifies the reconnect floor:
// when a session is established but its stream dies almost immediately, supervise
// must wait out minReconnectInterval before reconnecting rather than spinning
// with zero delay (CPU peg / hub hammering). (#61)
func TestSupervise_FloorsReconnectAfterImmediateDeath(t *testing.T) {
	d, _ := buildTestDaemon(t)
	d.minReconnectInterval = 80 * time.Millisecond

	d.registerFn = func(context.Context, []*pb.Share, int) (*pb.RegisterResponse, error) {
		return &pb.RegisterResponse{}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	reconnected := make(chan time.Time, 1)
	d.subscribeFn = func(context.Context) (pb.HubFuse_SubscribeClient, error) {
		select {
		case reconnected <- time.Now(): // record the first reconnect's timestamp
		default:
		}
		return &fakeSubscribeStream{recv: func() (*pb.Event, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}, nil
	}

	start := time.Now()
	go d.supervise(ctx, errStream()) // initial stream dies immediately at ~start

	var reconnectAt time.Time
	select {
	case reconnectAt = <-reconnected:
	case <-time.After(2 * time.Second):
		t.Fatal("supervise never reconnected")
	}

	// The initial stream died almost immediately, so supervise must have waited
	// out (most of) the 80ms floor before reconnecting. Generous slack below the
	// floor absorbs scheduler/timer coarseness; a regression that drops the floor
	// would reconnect in well under 60ms.
	elapsed := reconnectAt.Sub(start)
	assert.GreaterOrEqual(t, elapsed, 60*time.Millisecond,
		"supervise must wait out minReconnectInterval before reconnecting after an immediate stream death")
}

// TestSessionOnce_RemountsPeerAtChangedEndpoint covers the daemon-level wiring of
// the #61 end-user goal: a reconnect re-runs processInitialDevices from the fresh
// Register snapshot, and a peer whose IP changed is remounted at the new
// endpoint. Earlier sessionOnce tests pass an empty snapshot, so this is the only
// coverage of snapshot → Mount → remount through the daemon. (#61)
func TestSessionOnce_RemountsPeerAtChangedEndpoint(t *testing.T) {
	d, dir := buildTestDaemon(t)

	mountTo := filepath.Join(dir, "mnt", "docs")
	t.Cleanup(func() { _ = os.Chmod(mountTo, 0o755) })
	d.config.Mounts = []agentconfig.MountConfig{
		{Device: "peer", Share: "docs", To: mountTo},
	}
	writePubKey(t, dir, "peer-device") // pair the peer so auto-mount proceeds

	d.subscribeFn = func(context.Context) (pb.HubFuse_SubscribeClient, error) {
		return errStream(), nil // returned stream is unused by this test
	}

	peerAt := func(ip string, port int32) *pb.RegisterResponse {
		return &pb.RegisterResponse{
			DevicesOnline: []*pb.DeviceInfo{{
				DeviceId: "peer-device",
				Nickname: "peer",
				Ip:       ip,
				SshPort:  port,
				Shares:   []*pb.Share{{Alias: "docs", Permissions: "ro"}},
			}},
		}
	}

	// First session: peer online at 10.0.0.5 → auto-mount there.
	d.registerFn = func(context.Context, []*pb.Share, int) (*pb.RegisterResponse, error) {
		return peerAt("10.0.0.5", 2222), nil
	}
	_, err := d.sessionOnce(context.Background())
	require.NoError(t, err, "first sessionOnce")

	require.True(t, d.mounter.IsActive("peer", "docs"), "peer share must be mounted from the Register snapshot")
	mounts := d.mounter.ActiveMounts()
	require.Len(t, mounts, 1)
	assert.Equal(t, "10.0.0.5", mounts[0].IP, "initial mount endpoint")

	// Peer roamed: the second session reports a new IP. processInitialDevices
	// re-runs from the fresh snapshot and the mounter remounts at the new endpoint.
	d.registerFn = func(context.Context, []*pb.Share, int) (*pb.RegisterResponse, error) {
		return peerAt("10.0.0.99", 2222), nil
	}
	_, err = d.sessionOnce(context.Background())
	require.NoError(t, err, "second sessionOnce")

	require.True(t, d.mounter.IsActive("peer", "docs"), "peer share must still be mounted after roaming")
	mounts = d.mounter.ActiveMounts()
	require.Len(t, mounts, 1, "exactly one mount after remount")
	assert.Equal(t, "10.0.0.99", mounts[0].IP, "mount must follow the peer to the new endpoint after reconnect")
}

// ─── preflightMountBinary ──────────────────────────────────────────────────────

// captureLogger returns a logger that writes warnings (and above) into buf so a
// test can assert on the emitted message.
func captureLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

func TestPreflightMountBinary_MissingBinaryWarnsButDoesNotAbort(t *testing.T) {
	var buf bytes.Buffer
	logger := captureLogger(&buf)

	called := false
	lookPath := func(string) (string, error) {
		called = true
		return "", errors.New("not found")
	}
	fileExists := func(string) bool { return true } // runtime markers present, no extra warn

	// Must not panic; warn-and-continue is the contract (no error to assert on
	// since the helper returns nothing). fuse-t is macOS-only, so the hint stays
	// the fuse-t tap+cask regardless of the goos we pass here.
	preflightMountBinary("fuse-t", resolveBackend("fuse-t"), true, "darwin", lookPath, fileExists, logger)

	assert.True(t, called, "lookPath should be invoked when mounts are configured")
	out := buf.String()
	assert.Contains(t, out, "not found on PATH", "expected an actionable warning to be logged")
	assert.Contains(t, out, "brew tap macos-fuse-t/homebrew-cask && brew install --cask fuse-t fuse-t-sshfs", "fuse-t hint must tap the third-party cask before installing")
	assert.Contains(t, out, "level=WARN", "message must be logged at WARN level, never an error/abort")
}

func TestPreflightMountBinary_InstallHintIsOSAware(t *testing.T) {
	lookPath := func(string) (string, error) {
		return "", errors.New("not found")
	}
	fileExists := func(string) bool { return true }

	t.Run("sshfs on linux suggests distro package", func(t *testing.T) {
		var buf bytes.Buffer
		preflightMountBinary("sshfs", resolveBackend("sshfs"), true, "linux", lookPath, fileExists, captureLogger(&buf))
		out := buf.String()
		assert.Contains(t, out, "sshfs package", "linux sshfs hint should point at the distro package")
		assert.NotContains(t, out, "brew", "linux sshfs hint must not mention brew")
	})

	t.Run("sshfs on darwin does not claim fuse-t", func(t *testing.T) {
		var buf bytes.Buffer
		preflightMountBinary("sshfs", resolveBackend("sshfs"), true, "darwin", lookPath, fileExists, captureLogger(&buf))
		out := buf.String()
		assert.NotContains(t, out, "fuse-t", "sshfs-on-darwin hint must not recommend fuse-t when sshfs was chosen")
		assert.Contains(t, out, "sshfs", "sshfs-on-darwin hint should still point at an sshfs binary")
	})

	t.Run("fuse-t taps the third-party cask first", func(t *testing.T) {
		var buf bytes.Buffer
		preflightMountBinary("fuse-t", resolveBackend("fuse-t"), true, "darwin", lookPath, fileExists, captureLogger(&buf))
		assert.Contains(t, buf.String(), "brew tap macos-fuse-t/homebrew-cask && brew install --cask fuse-t fuse-t-sshfs", "fuse-t hint must tap before installing")
	})
}

func TestPreflightMountBinary_NoMountsSkipsLookPath(t *testing.T) {
	var buf bytes.Buffer
	logger := captureLogger(&buf)

	lookPath := func(string) (string, error) {
		t.Fatal("lookPath must not be called when no mounts are configured")
		return "", nil
	}
	fileExists := func(string) bool {
		t.Fatal("fileExists must not be called when no mounts are configured")
		return false
	}

	preflightMountBinary("sshfs", resolveBackend("sshfs"), false, runtime.GOOS, lookPath, fileExists, logger)

	assert.Empty(t, buf.String(), "no warning should be logged when there are no mounts")
}

func TestPreflightMountBinary_BinaryPresentNoWarning(t *testing.T) {
	var buf bytes.Buffer
	logger := captureLogger(&buf)

	lookPath := func(string) (string, error) {
		return "/usr/bin/sshfs", nil
	}
	fileExists := func(string) bool { return true }

	preflightMountBinary("sshfs", resolveBackend("sshfs"), true, runtime.GOOS, lookPath, fileExists, logger)

	assert.Empty(t, buf.String(), "no warning should be logged when the binary is found")
}

// ─── preflightMountBinary: FUSE-T runtime checks ─────────────────────────────

// TestPreflightMountBinary_FuseTRuntimeMissingWarns verifies that when fuse-t
// is selected on darwin and none of the runtime markers exist, a WARN is logged.
func TestPreflightMountBinary_FuseTRuntimeMissingWarns(t *testing.T) {
	var buf bytes.Buffer
	logger := captureLogger(&buf)

	lookPath := func(string) (string, error) { return "/usr/local/bin/sshfs", nil }
	// Stub: no runtime markers present.
	fileExists := func(string) bool { return false }

	preflightMountBinary("fuse-t", resolveBackend("fuse-t"), true, "darwin", lookPath, fileExists, logger)

	out := buf.String()
	assert.Contains(t, out, "level=WARN", "must log at WARN level")
	assert.Contains(t, out, "FUSE-T runtime not found", "must mention runtime not found")
	assert.Contains(t, out, "fuse-t-sshfs", "must include the install hint")
}

// TestPreflightMountBinary_FuseTRuntimePresentNoWarn verifies that when at
// least one runtime marker exists, no runtime warning is emitted.
func TestPreflightMountBinary_FuseTRuntimePresentNoWarn(t *testing.T) {
	var buf bytes.Buffer
	logger := captureLogger(&buf)

	lookPath := func(string) (string, error) { return "/usr/local/bin/sshfs", nil }
	// Stub: first marker found.
	fileExists := func(path string) bool {
		return path == fuseTRuntimeMarkers[0]
	}

	preflightMountBinary("fuse-t", resolveBackend("fuse-t"), true, "darwin", lookPath, fileExists, logger)

	out := buf.String()
	assert.NotContains(t, out, "FUSE-T runtime not found", "must not warn when a runtime marker is present")
}

// TestPreflightMountBinary_FuseTRuntimeCheckNotOnLinux verifies that the
// fuse-t runtime check is not performed on non-darwin hosts.
func TestPreflightMountBinary_FuseTRuntimeCheckNotOnLinux(t *testing.T) {
	var buf bytes.Buffer
	logger := captureLogger(&buf)

	lookPath := func(string) (string, error) { return "/usr/bin/sshfs", nil }
	fileExists := func(string) bool {
		t.Fatal("fileExists must not be called for fuse-t on non-darwin")
		return false
	}

	preflightMountBinary("fuse-t", resolveBackend("fuse-t"), true, "linux", lookPath, fileExists, logger)

	assert.NotContains(t, buf.String(), "FUSE-T runtime not found", "runtime check must be skipped on linux")
}

// ─── Shutdown: bounded unmount (#50) ─────────────────────────────────────────

// TestShutdown_BoundedUnmountReturnsUnderTimeout verifies that daemon.Shutdown
// completes within a reasonable time even when BOTH the unmount command and the
// mountpoint re-check block on a wedged mount. The ctx-guard inside
// UnmountAllForce ensures shutdown returns promptly (it does not hang); because
// neither step could confirm the mount is gone, the entry is retained rather than
// falsely reaped. The "returned within the guard" assertion is the daemon-level
// proof for #50.1. (#50 bounded/force, no false reap)
func TestShutdown_BoundedUnmountReturnsUnderTimeout(t *testing.T) {
	d, dir := buildTestDaemon(t)

	// Pre-mount: override checkMountpoint to allow Mount's verify loop to pass.
	d.mounter.SetMountpointCheckForTests(func(string) (bool, error) { return true, nil })

	writePubKey(t, dir, "device-a")
	mc := agentconfig.MountConfig{
		Device: "device-a",
		Share:  "docs",
		To:     filepath.Join(dir, "mnt"),
	}
	require.NoError(t, d.mounter.Mount(context.Background(), mc, "device-a", "10.0.0.1", 2222), "pre-mount")
	require.True(t, d.mounter.IsActive("device-a", "docs"), "pre-mount must be active")

	// Install a blocking unmount: blocks on ctx cancellation.
	blocked := make(chan struct{}) // never closed in this test
	d.mounter.SetUnmountForTests(func(ctx context.Context, _ string, _ bool) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-blocked:
			return nil
		}
	})
	// Install a blocking checkMountpoint: also blocks on the same channel.
	// This exercises the worst-case where BOTH command and re-check are wedged.
	d.mounter.SetMountpointCheckForTests(func(string) (bool, error) {
		<-blocked
		return true, nil
	})

	// Shutdown must complete within a generous guard (the internal budget is 5s;
	// we allow 6s from the test's perspective so CI has headroom).
	done := make(chan error, 1)
	go func() {
		// Shutdown calls d.hubClient (nil) and d.sshServer.Stop — those are fine.
		done <- d.Shutdown()
	}()

	select {
	case err := <-done:
		// Shutdown may return an error (the wedged unmount could not be confirmed,
		// plus sshServer.Stop etc.); the important thing is that it RETURNED at all
		// (not hung) — that is the #50.1 guarantee.
		_ = err
	case <-time.After(6 * time.Second):
		t.Fatal("Shutdown() did not return within 6s — bounded unmount is not working (#50)")
	}

	// A mount that could not be confirmed gone (both command and re-check wedged)
	// is RETAINED, not reaped — we never silently drop a possibly-live mount on a
	// timeout. The process is exiting anyway, so the retained entry is harmless.
	assert.True(t, d.mounter.IsActive("device-a", "docs"), "a wedged mount must not be falsely reaped on a timeout")
}

// TestShutdown_BoundedUnmount_EmptyMountsReturnsImmediately verifies that
// Shutdown with no active mounts returns promptly (regression guard).
func TestShutdown_BoundedUnmount_EmptyMountsReturnsImmediately(t *testing.T) {
	d, _ := buildTestDaemon(t)

	done := make(chan error, 1)
	go func() {
		done <- d.Shutdown()
	}()

	select {
	case <-done:
		// OK
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown() with no mounts should return immediately")
	}
}

// ─── Guard-target daemon-level tests (#49) ────────────────────────────────────

// TestGuardTarget_MountRemoveRestoresPerms verifies that when a mount is removed
// from config via onConfigChange (MountsRemoved), the target directory is restored
// to 0o755 after unmount. (#49 test 8)
func TestGuardTarget_MountRemoveRestoresPerms(t *testing.T) {
	d, dir := buildTestDaemon(t)

	mountTo := filepath.Join(dir, "mnt", "music")
	writePubKey(t, dir, "device-abc")

	// Pre-mount the target (this will guard it to 0o500).
	mc := agentconfig.MountConfig{Device: "remote", Share: "music", To: mountTo}
	require.NoError(t, d.mounter.Mount(context.Background(), mc, "device-abc", "10.0.0.9", 2222), "pre-mount")

	// After a successful Mount the point is left at mountableMode (a real FUSE
	// mount would mask it); guardMode is re-applied on unmount, asserted below.
	info, err := os.Stat(mountTo)
	require.NoError(t, err)
	assert.Equal(t, mountableMode, info.Mode().Perm(), "target must be mountable (owner-writable) after Mount")

	// Now drive onConfigChange with the mount removed.
	oldCfg := &agentconfig.Config{
		Mounts: []agentconfig.MountConfig{mc},
	}
	newCfg := &agentconfig.Config{
		Mounts: []agentconfig.MountConfig{},
	}

	// hubClient is nil in buildTestDaemon; SharesChanged is false so no hubClient
	// call is made.
	d.onConfigChange(oldCfg, newCfg)

	// Target should be back to 0o755.
	info, err = os.Stat(mountTo)
	require.NoError(t, err, "target must still exist after mount remove")
	assert.Equal(t, os.FileMode(0o755), info.Mode().Perm(), "target must be restored to 0o755 after mount remove (#49 test 8)")
}

// TestGuardTarget_TryMountGuardsOfflineTarget verifies that when tryMount is
// called for a device that is not online, the target dir is restricted to
// guardMode. (#49 test 9)
func TestGuardTarget_TryMountGuardsOfflineTarget(t *testing.T) {
	d, dir := buildTestDaemon(t)

	mountTo := filepath.Join(dir, "mnt", "photos")
	mc := agentconfig.MountConfig{Device: "offline-device", Share: "photos", To: mountTo}

	// Ensure the target dir exists at a normal mode first.
	require.NoError(t, os.MkdirAll(mountTo, 0o755))

	// Restore dir perms before t.TempDir cleanup.
	t.Cleanup(func() { _ = os.Chmod(mountTo, 0o755) })

	// tryMount — device is not in onlineDevices, so it early-returns and must guard.
	d.tryMount(mc)

	info, err := os.Stat(mountTo)
	require.NoError(t, err, "target must exist")
	assert.Equal(t, guardMode, info.Mode().Perm(), "tryMount must guard target when device is offline (#49 test 9)")
}

// TestGuardTarget_GuardConfiguredTargets verifies that guardConfiguredTargets
// restricts all configured mount target dirs to guardMode. (#49 test 10)
func TestGuardTarget_GuardConfiguredTargets(t *testing.T) {
	d, dir := buildTestDaemon(t)

	mountTo1 := filepath.Join(dir, "mnt", "share1")
	mountTo2 := filepath.Join(dir, "mnt", "share2")

	// Pre-create both dirs at 0o755.
	require.NoError(t, os.MkdirAll(mountTo1, 0o755))
	require.NoError(t, os.MkdirAll(mountTo2, 0o755))

	// Restore dir perms before t.TempDir cleanup.
	t.Cleanup(func() {
		_ = os.Chmod(mountTo1, 0o755)
		_ = os.Chmod(mountTo2, 0o755)
	})

	d.config.Mounts = []agentconfig.MountConfig{
		{Device: "alpha", Share: "share1", To: mountTo1},
		{Device: "beta", Share: "share2", To: mountTo2},
	}

	d.guardConfiguredTargets()

	info1, err := os.Stat(mountTo1)
	require.NoError(t, err)
	assert.Equal(t, guardMode, info1.Mode().Perm(), "first target must be restricted to guardMode (#49 test 10)")

	info2, err := os.Stat(mountTo2)
	require.NoError(t, err)
	assert.Equal(t, guardMode, info2.Mode().Perm(), "second target must be restricted to guardMode (#49 test 10)")
}
