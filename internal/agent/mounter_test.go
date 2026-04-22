package agent

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentconfig "github.com/ykhdr/hubfuse/internal/agent/config"
)

// discardLogger returns a logger that discards all output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// newTestMounter creates a Mounter with test overrides.
// knownDevicesDir is where paired-peer *.pub files live; a sibling
// "known_hosts" directory is used for the per-mount known_hosts files.
// capturedArgs receives the args of the most recent sshfs invocation.
// unmountFn is called when Unmount is invoked; if nil, a no-op is used.
func newTestMounter(t *testing.T, knownDevicesDir, keyPath string, capturedArgs *[]string, unmountFn func(string) error) *Mounter {
	t.Helper()
	knownHostsDir := filepath.Join(filepath.Dir(knownDevicesDir), "known_hosts")
	m := NewMounter(keyPath, knownDevicesDir, knownHostsDir, discardLogger())

	m.execCommand = func(_ context.Context, name string, args ...string) *exec.Cmd {
		if capturedArgs != nil {
			*capturedArgs = append([]string{name}, args...)
		}
		// Return a command that succeeds immediately (true/cmd.exe).
		var cmd *exec.Cmd
		if runtime.GOOS == "windows" {
			cmd = exec.Command("cmd", "/C", "exit 0")
		} else {
			cmd = exec.Command("true")
		}
		return cmd
	}

	if unmountFn != nil {
		m.unmount = unmountFn
	} else {
		m.unmount = func(_ string) error { return nil }
	}

	return m
}

// writePubKeyFile writes a dummy .pub file for deviceID in dir, simulating a paired device.
func writePubKeyFile(t *testing.T, dir, deviceID string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0700), "MkdirAll(%q)", dir)
	path := filepath.Join(dir, deviceID+".pub")
	require.NoError(t, os.WriteFile(path, []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItest\n"), 0644), "WriteFile(%q)", path)
}

// ─── Mount ────────────────────────────────────────────────────────────────────

func TestMount_BuildsCorrectSSHFSArgs(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, "known_devices")
	keyPath := filepath.Join(dir, "id_ed25519")
	mountTo := filepath.Join(dir, "mnt", "docs")

	writePubKeyFile(t, knownDir, "device-a")

	var capturedArgs []string
	m := newTestMounter(t, knownDir, keyPath, &capturedArgs, nil)

	mc := agentconfig.MountConfig{
		Device: "device-a",
		Share:  "documents",
		To:     mountTo,
	}

	require.NoError(t, m.Mount(context.Background(), mc, "device-a", "192.168.1.10", 2222), "Mount()")

	knownHostsPath := filepath.Join(dir, "known_hosts", "device-a")
	want := []string{
		"sshfs",
		"-p", "2222",
		"-o", "IdentityFile=" + keyPath,
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile=" + knownHostsPath,
		"hubfuse@192.168.1.10:documents",
		mountTo,
	}

	assert.Equal(t, want, capturedArgs, "sshfs args")

	data, err := os.ReadFile(knownHostsPath)
	require.NoError(t, err, "read known_hosts file")
	assert.Equal(t,
		"[192.168.1.10]:2222 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItest\n",
		string(data),
		"known_hosts contents")
}

func TestMount_FailsWhenPeerPublicKeyMissing(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, "known_devices")
	keyPath := filepath.Join(dir, "id_ed25519")

	require.NoError(t, os.MkdirAll(knownDir, 0700), "MkdirAll(knownDir)")

	m := newTestMounter(t, knownDir, keyPath, nil, nil)

	mc := agentconfig.MountConfig{
		Device: "device-x",
		Share:  "docs",
		To:     filepath.Join(dir, "mnt"),
	}

	err := m.Mount(context.Background(), mc, "device-x", "10.0.0.1", 2222)
	assert.Error(t, err, "Mount() expected error when peer pubkey missing")
	assert.False(t, m.IsActive("device-x", "docs"), "must not record mount when pubkey missing")
}

func TestMount_KnownHostsUsesPlainHostForDefaultPort(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, "known_devices")
	keyPath := filepath.Join(dir, "id_ed25519")

	writePubKeyFile(t, knownDir, "device-a")

	m := newTestMounter(t, knownDir, keyPath, nil, nil)

	mc := agentconfig.MountConfig{
		Device: "device-a",
		Share:  "docs",
		To:     filepath.Join(dir, "mnt"),
	}

	require.NoError(t, m.Mount(context.Background(), mc, "device-a", "10.0.0.1", 22), "Mount()")

	knownHostsPath := filepath.Join(dir, "known_hosts", "device-a")
	data, err := os.ReadFile(knownHostsPath)
	require.NoError(t, err, "read known_hosts file")
	assert.Equal(t,
		"10.0.0.1 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItest\n",
		string(data),
		"known_hosts must use unbracketed host pattern for port 22")
}

func TestMount_CreatesMountPointDirectory(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, "known_devices")
	keyPath := filepath.Join(dir, "id_ed25519")
	mountTo := filepath.Join(dir, "deep", "nested", "mount")

	writePubKeyFile(t, knownDir, "device-a")

	m := newTestMounter(t, knownDir, keyPath, nil, nil)

	mc := agentconfig.MountConfig{
		Device: "device-a",
		Share:  "docs",
		To:     mountTo,
	}

	require.NoError(t, m.Mount(context.Background(), mc, "device-a", "10.0.0.1", 2222), "Mount()")

	_, err := os.Stat(mountTo)
	assert.NoError(t, err, "mount point directory not created")
}

func TestMount_RejectsDuplicateMount(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, "known_devices")
	keyPath := filepath.Join(dir, "id_ed25519")

	writePubKeyFile(t, knownDir, "device-a")

	m := newTestMounter(t, knownDir, keyPath, nil, nil)

	mc := agentconfig.MountConfig{
		Device: "device-a",
		Share:  "docs",
		To:     filepath.Join(dir, "mnt1"),
	}

	require.NoError(t, m.Mount(context.Background(), mc, "device-a", "10.0.0.1", 2222), "first Mount()")

	mc2 := agentconfig.MountConfig{
		Device: "device-a",
		Share:  "docs",
		To:     filepath.Join(dir, "mnt2"),
	}

	err := m.Mount(context.Background(), mc2, "device-a", "10.0.0.1", 2222)
	assert.Error(t, err, "second Mount() expected error for duplicate")
}

func TestMount_RecordsActiveMount(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, "known_devices")
	keyPath := filepath.Join(dir, "id_ed25519")
	mountTo := filepath.Join(dir, "mnt")

	writePubKeyFile(t, knownDir, "device-a")

	m := newTestMounter(t, knownDir, keyPath, nil, nil)

	mc := agentconfig.MountConfig{
		Device: "device-a",
		Share:  "docs",
		To:     mountTo,
	}

	require.NoError(t, m.Mount(context.Background(), mc, "device-a", "192.168.1.5", 2222), "Mount()")

	assert.True(t, m.IsActive("device-a", "docs"), "IsActive() = false, want true after Mount()")

	mounts := m.ActiveMounts()
	require.Len(t, mounts, 1)
	mnt := mounts[0]
	assert.Equal(t, "device-a", mnt.Device)
	assert.Equal(t, "docs", mnt.Share)
	assert.Equal(t, "192.168.1.5", mnt.IP)
	assert.Equal(t, 2222, mnt.SSHPort)
	assert.Equal(t, mountTo, mnt.LocalPath)
}

// ─── Unmount ──────────────────────────────────────────────────────────────────

func TestUnmount_RemovesActiveMount(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, "known_devices")
	keyPath := filepath.Join(dir, "id_ed25519")
	mountTo := filepath.Join(dir, "mnt")

	writePubKeyFile(t, knownDir, "device-a")

	m := newTestMounter(t, knownDir, keyPath, nil, nil)

	mc := agentconfig.MountConfig{Device: "device-a", Share: "docs", To: mountTo}
	require.NoError(t, m.Mount(context.Background(), mc, "device-a", "10.0.0.1", 2222), "Mount()")
	require.NoError(t, m.Unmount("device-a", "docs"), "Unmount()")

	assert.False(t, m.IsActive("device-a", "docs"), "IsActive() = true after Unmount(), want false")
}

func TestUnmount_ErrorForNonExistentMount(t *testing.T) {
	dir := t.TempDir()
	m := newTestMounter(t, dir, filepath.Join(dir, "key"), nil, nil)

	err := m.Unmount("no-device", "no-share")
	assert.Error(t, err, "Unmount() expected error for non-existent mount")
}

func TestUnmount_CallsUnmountFunction(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, "known_devices")
	keyPath := filepath.Join(dir, "id_ed25519")
	mountTo := filepath.Join(dir, "mnt")

	writePubKeyFile(t, knownDir, "device-a")

	var unmountedPath string
	unmountFn := func(path string) error {
		unmountedPath = path
		return nil
	}

	m := newTestMounter(t, knownDir, keyPath, nil, unmountFn)

	mc := agentconfig.MountConfig{Device: "device-a", Share: "docs", To: mountTo}
	require.NoError(t, m.Mount(context.Background(), mc, "device-a", "10.0.0.1", 2222), "Mount()")
	require.NoError(t, m.Unmount("device-a", "docs"), "Unmount()")

	assert.Equal(t, mountTo, unmountedPath, "unmount called with wrong path")
}

// ─── UnmountAll ───────────────────────────────────────────────────────────────

func TestUnmountAll_UnmountsAllActive(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, "known_devices")
	keyPath := filepath.Join(dir, "id_ed25519")

	writePubKeyFile(t, knownDir, "device-a")
	writePubKeyFile(t, knownDir, "device-b")

	m := newTestMounter(t, knownDir, keyPath, nil, nil)

	mounts := []agentconfig.MountConfig{
		{Device: "device-a", Share: "docs", To: filepath.Join(dir, "mnt1")},
		{Device: "device-a", Share: "photos", To: filepath.Join(dir, "mnt2")},
		{Device: "device-b", Share: "music", To: filepath.Join(dir, "mnt3")},
	}

	for _, mc := range mounts {
		require.NoError(t, m.Mount(context.Background(), mc, mc.Device, "10.0.0.1", 2222), "Mount(%q/%q)", mc.Device, mc.Share)
	}

	require.Len(t, m.ActiveMounts(), 3, "expected 3 active mounts")
	require.NoError(t, m.UnmountAll(), "UnmountAll()")

	assert.Empty(t, m.ActiveMounts(), "ActiveMounts() after UnmountAll should be empty")
}

func TestUnmountAll_EmptyIsNoOp(t *testing.T) {
	dir := t.TempDir()
	m := newTestMounter(t, dir, filepath.Join(dir, "key"), nil, nil)

	assert.NoError(t, m.UnmountAll(), "UnmountAll() on empty mounter")
}

// ─── UnmountDevice ────────────────────────────────────────────────────────────

func TestUnmountDevice_UnmountsOnlyTargetDevice(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, "known_devices")
	keyPath := filepath.Join(dir, "id_ed25519")

	writePubKeyFile(t, knownDir, "device-a")
	writePubKeyFile(t, knownDir, "device-b")

	m := newTestMounter(t, knownDir, keyPath, nil, nil)

	mounts := []agentconfig.MountConfig{
		{Device: "device-a", Share: "docs", To: filepath.Join(dir, "mnt1")},
		{Device: "device-a", Share: "photos", To: filepath.Join(dir, "mnt2")},
		{Device: "device-b", Share: "music", To: filepath.Join(dir, "mnt3")},
	}
	for _, mc := range mounts {
		require.NoError(t, m.Mount(context.Background(), mc, mc.Device, "10.0.0.1", 2222), "Mount(%q/%q)", mc.Device, mc.Share)
	}

	require.NoError(t, m.UnmountDevice("device-a"), "UnmountDevice()")

	assert.False(t, m.IsActive("device-a", "docs"), "device-a/docs still active after UnmountDevice(device-a)")
	assert.False(t, m.IsActive("device-a", "photos"), "device-a/photos still active after UnmountDevice(device-a)")
	assert.True(t, m.IsActive("device-b", "music"), "device-b/music should still be active")
}

// ─── IsActive ─────────────────────────────────────────────────────────────────

func TestIsActive_FalseWhenNotMounted(t *testing.T) {
	dir := t.TempDir()
	m := newTestMounter(t, dir, filepath.Join(dir, "key"), nil, nil)

	assert.False(t, m.IsActive("device-a", "docs"), "IsActive() = true for unmounted share, want false")
}

// ─── Platform-specific unmount command ───────────────────────────────────────

func TestUnmountPath_MacOS(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS-specific test")
	}

	// unmountPath internally calls "umount <path>" on macOS.
	// We can't easily test this without a real mount, so we verify the
	// function returns an error for a non-existent path (not a mount point).
	err := unmountPath("/tmp/not-a-real-mount-point-xyz")
	assert.Error(t, err, "unmountPath() expected error for non-mount path")
}

func TestUnmountPath_Linux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux-specific test")
	}

	// On Linux, unmountPath calls "fusermount -u <path>".
	// Verify it returns an error for a non-mount path.
	err := unmountPath("/tmp/not-a-real-mount-point-xyz")
	assert.Error(t, err, "unmountPath() expected error for non-mount path")
}

// ─── ActiveMounts ─────────────────────────────────────────────────────────────

func TestActiveMounts_ReturnsSnapshot(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, "known_devices")
	keyPath := filepath.Join(dir, "id_ed25519")

	writePubKeyFile(t, knownDir, "device-a")

	m := newTestMounter(t, knownDir, keyPath, nil, nil)

	mounts := m.ActiveMounts()
	assert.Empty(t, mounts, "ActiveMounts() on empty mounter")

	mc := agentconfig.MountConfig{
		Device: "device-a",
		Share:  "docs",
		To:     filepath.Join(dir, "mnt"),
	}
	require.NoError(t, m.Mount(context.Background(), mc, "device-a", "10.0.0.1", 2222), "Mount()")

	mounts = m.ActiveMounts()
	assert.Len(t, mounts, 1)
}
