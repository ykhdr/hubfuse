package agent

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentconfig "github.com/ykhdr/hubfuse/internal/agent/config"
	"github.com/ykhdr/hubfuse/internal/common"
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
func newTestMounter(t *testing.T, knownDevicesDir, keyPath string, capturedArgs *[]string, unmountFn func(context.Context, string, bool) error) *Mounter {
	t.Helper()
	return newTestMounterWithTool(t, knownDevicesDir, keyPath, "", capturedArgs, unmountFn)
}

// newTestMounterWithTool is like newTestMounter but lets a test select the
// mount tool (e.g. "fuse-t") so the resolved backend can be exercised.
func newTestMounterWithTool(t *testing.T, knownDevicesDir, keyPath, mountTool string, capturedArgs *[]string, unmountFn func(context.Context, string, bool) error) *Mounter {
	t.Helper()
	knownHostsDir := filepath.Join(filepath.Dir(knownDevicesDir), common.KnownHostsDir)
	m := NewMounter(keyPath, knownDevicesDir, knownHostsDir, mountTool, discardLogger())

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
		m.unmount = func(_ context.Context, _ string, _ bool) error { return nil }
	}

	// Stub checkMountpoint so existing Mount tests do not wait for a real
	// filesystem mountpoint that the stub command ("true") never creates.
	m.checkMountpoint = func(string) (bool, error) { return true, nil }
	// Use tiny timeouts so any test that exercises the verification path completes quickly.
	m.mountVerifyTimeout = 500 * time.Millisecond
	m.mountVerifyInterval = 10 * time.Millisecond

	return m
}

// writePubKeyFile writes a dummy .pub file for deviceID in dir, simulating a paired device.
func writePubKeyFile(t *testing.T, dir, deviceID string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0700), "MkdirAll(%q)", dir)
	path := filepath.Join(dir, deviceID+".pub")
	require.NoError(t, os.WriteFile(path, []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItest\n"), 0644), "WriteFile(%q)", path)
}

// ─── Backend profiles & pure helpers ────────────────────────────────────────

func TestResolveBackend(t *testing.T) {
	tests := []struct {
		name       string
		tool       string
		wantBinary string
	}{
		{name: "sshfs", tool: "sshfs", wantBinary: "sshfs"},
		{name: "fuse-t", tool: "fuse-t", wantBinary: "sshfs"},
		{name: "empty defaults to sshfs", tool: "", wantBinary: "sshfs"},
		{name: "unknown defaults to sshfs", tool: "bogus", wantBinary: "sshfs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := resolveBackend(tt.tool)
			assert.Equal(t, tt.wantBinary, b.binary, "backend binary")
		})
	}
}

func TestBuildMountArgs_BaseArgs(t *testing.T) {
	b := resolveBackend("sshfs")
	args := buildMountArgs(b, 2222, "/key/path", "/known/hosts", "192.168.1.10", "documents", "/mnt/docs")

	want := []string{
		"-p", "2222",
		"-o", "IdentityFile=/key/path",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile=/known/hosts",
		"hubfuse@192.168.1.10:documents",
		"/mnt/docs",
	}
	assert.Equal(t, want, args, "base mount args")
}

func TestBuildMountArgs_ExtraOptsInjectedBeforeOperands(t *testing.T) {
	// Construct a backend with non-empty extraOpts to verify ordering: the
	// extra -o pairs must appear after the base options and before the
	// user@host:share / target operands.
	b := mountBackend{binary: "sshfs", extraOpts: []string{"volname=share", "noappledouble"}}
	args := buildMountArgs(b, 22, "/key", "/kh", "10.0.0.1", "photos", "/mnt/photos")

	want := []string{
		"-p", "22",
		"-o", "IdentityFile=/key",
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile=/kh",
		"-o", "volname=share",
		"-o", "noappledouble",
		"hubfuse@10.0.0.1:photos",
		"/mnt/photos",
	}
	assert.Equal(t, want, args, "mount args with extraOpts")
}

func TestValidateMountTool(t *testing.T) {
	tests := []struct {
		name            string
		tool            string
		goos            string
		wantErr         bool
		wantErrContains string
	}{
		{name: "fuse-t on linux is rejected", tool: "fuse-t", goos: "linux", wantErr: true, wantErrContains: "only supported on macOS"},
		{name: "fuse-t on darwin is ok", tool: "fuse-t", goos: "darwin", wantErr: false},
		{name: "sshfs on linux is ok", tool: "sshfs", goos: "linux", wantErr: false},
		{name: "sshfs on darwin is ok", tool: "sshfs", goos: "darwin", wantErr: false},
		{name: "empty is ok", tool: "", goos: "linux", wantErr: false},
		{name: "bad value on darwin is rejected", tool: "bogus", goos: "darwin", wantErr: true, wantErrContains: `must be "sshfs" or "fuse-t"`},
		{name: "bad value on linux is rejected", tool: "bogus", goos: "linux", wantErr: true, wantErrContains: `must be "sshfs" or "fuse-t"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateMountTool(tt.tool, tt.goos)
			if tt.wantErr {
				require.Error(t, err, "validateMountTool(%q, %q)", tt.tool, tt.goos)
				assert.Contains(t, err.Error(), tt.wantErrContains, "validateMountTool(%q, %q) error text", tt.tool, tt.goos)
			} else {
				assert.NoError(t, err, "validateMountTool(%q, %q)", tt.tool, tt.goos)
			}
		})
	}
}

// ─── unmountLadder table tests ────────────────────────────────────────────────

// TestUnmountLadder verifies that the pure helper returns the expected ordered
// argv sequences for each OS × force combination. (#50 bounded/force)
func TestUnmountLadder(t *testing.T) {
	tests := []struct {
		name  string
		goos  string
		force bool
		want  [][]string
	}{
		{
			name:  "linux non-force",
			goos:  "linux",
			force: false,
			want: [][]string{
				{"fusermount", "-u"},
				{"fusermount", "-uz"},
			},
		},
		{
			name:  "linux force",
			goos:  "linux",
			force: true,
			want: [][]string{
				{"fusermount", "-u"},
				{"fusermount", "-uz"},
				{"umount", "-l"},
			},
		},
		{
			name:  "darwin non-force",
			goos:  "darwin",
			force: false,
			want: [][]string{
				{"umount"},
			},
		},
		{
			name:  "darwin force",
			goos:  "darwin",
			force: true,
			want: [][]string{
				{"umount"},
				{"diskutil", "unmount", "force"},
				{"umount", "-f"},
			},
		},
		{
			name:  "unknown OS non-force (defaults to linux path)",
			goos:  "freebsd",
			force: false,
			want: [][]string{
				{"fusermount", "-u"},
				{"fusermount", "-uz"},
			},
		},
		{
			name:  "unknown OS force (defaults to linux path)",
			goos:  "freebsd",
			force: true,
			want: [][]string{
				{"fusermount", "-u"},
				{"fusermount", "-uz"},
				{"umount", "-l"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := unmountLadder(tt.goos, tt.force)
			assert.Equal(t, tt.want, got, "unmountLadder(%q, %v)", tt.goos, tt.force)
		})
	}
}

// ─── Mount ────────────────────────────────────────────────────────────────────

func TestMount_BuildsCorrectSSHFSArgs(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, common.KnownDevicesDir)
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

	knownHostsPath := filepath.Join(dir, common.KnownHostsDir, "device-a")
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

func TestMount_FuseTUsesSSHFSBinaryWithSameArgs(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, common.KnownDevicesDir)
	keyPath := filepath.Join(dir, "id_ed25519")
	mountTo := filepath.Join(dir, "mnt", "docs")

	writePubKeyFile(t, knownDir, "device-a")

	var capturedArgs []string
	m := newTestMounterWithTool(t, knownDir, keyPath, "fuse-t", &capturedArgs, nil)

	mc := agentconfig.MountConfig{
		Device: "device-a",
		Share:  "documents",
		To:     mountTo,
	}

	require.NoError(t, m.Mount(context.Background(), mc, "device-a", "192.168.1.10", 2222), "Mount()")

	knownHostsPath := filepath.Join(dir, common.KnownHostsDir, "device-a")
	// fuse-t ships a drop-in sshfs binary, so the invocation is byte-identical
	// to the default sshfs backend (extraOpts is empty for both today).
	want := []string{
		"sshfs",
		"-p", "2222",
		"-o", "IdentityFile=" + keyPath,
		"-o", "StrictHostKeyChecking=yes",
		"-o", "UserKnownHostsFile=" + knownHostsPath,
		"hubfuse@192.168.1.10:documents",
		mountTo,
	}

	require.NotEmpty(t, capturedArgs, "captured args")
	assert.Equal(t, "sshfs", capturedArgs[0], "fuse-t backend binary")
	assert.Equal(t, want, capturedArgs, "fuse-t mount args")
}

func TestMount_FailsWhenPeerPublicKeyMissing(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, common.KnownDevicesDir)
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

func TestMount_RejectsUnsafeDeviceID(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, common.KnownDevicesDir)
	keyPath := filepath.Join(dir, "id_ed25519")

	// Plant a decoy file outside knownHostsDir that a traversal would clobber.
	outside := filepath.Join(dir, "outside.pwned")
	require.NoError(t, os.WriteFile(outside, []byte("original"), 0644), "plant decoy")

	m := newTestMounter(t, knownDir, keyPath, nil, nil)

	mc := agentconfig.MountConfig{
		Device: "device-evil",
		Share:  "docs",
		To:     filepath.Join(dir, "mnt"),
	}

	err := m.Mount(context.Background(), mc, "../outside.pwned", "10.0.0.1", 2222)
	assert.Error(t, err, "Mount() must reject deviceID with path traversal")

	data, readErr := os.ReadFile(outside)
	require.NoError(t, readErr, "decoy disappeared")
	assert.Equal(t, "original", string(data), "decoy must not be overwritten via deviceID traversal")
}

func TestMount_KnownHostsUsesPlainHostForDefaultPort(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, common.KnownDevicesDir)
	keyPath := filepath.Join(dir, "id_ed25519")

	writePubKeyFile(t, knownDir, "device-a")

	m := newTestMounter(t, knownDir, keyPath, nil, nil)

	mc := agentconfig.MountConfig{
		Device: "device-a",
		Share:  "docs",
		To:     filepath.Join(dir, "mnt"),
	}

	require.NoError(t, m.Mount(context.Background(), mc, "device-a", "10.0.0.1", 22), "Mount()")

	knownHostsPath := filepath.Join(dir, common.KnownHostsDir, "device-a")
	data, err := os.ReadFile(knownHostsPath)
	require.NoError(t, err, "read known_hosts file")
	assert.Equal(t,
		"10.0.0.1 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItest\n",
		string(data),
		"known_hosts must use unbracketed host pattern for port 22")
}

func TestMount_CreatesMountPointDirectory(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, common.KnownDevicesDir)
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
	knownDir := filepath.Join(dir, common.KnownDevicesDir)
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
	knownDir := filepath.Join(dir, common.KnownDevicesDir)
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
	knownDir := filepath.Join(dir, common.KnownDevicesDir)
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
	knownDir := filepath.Join(dir, common.KnownDevicesDir)
	keyPath := filepath.Join(dir, "id_ed25519")
	mountTo := filepath.Join(dir, "mnt")

	writePubKeyFile(t, knownDir, "device-a")

	var unmountedPath string
	unmountFn := func(_ context.Context, path string, _ bool) error {
		unmountedPath = path
		return nil
	}

	m := newTestMounter(t, knownDir, keyPath, nil, unmountFn)

	mc := agentconfig.MountConfig{Device: "device-a", Share: "docs", To: mountTo}
	require.NoError(t, m.Mount(context.Background(), mc, "device-a", "10.0.0.1", 2222), "Mount()")
	require.NoError(t, m.Unmount("device-a", "docs"), "Unmount()")

	assert.Equal(t, mountTo, unmountedPath, "unmount called with wrong path")
}

// TestUnmount_DeadMountReapReturnsNilAndDropsEntry verifies #47: when the
// unmount command fails but checkMountpoint reports the path is no longer a
// mountpoint, Unmount returns nil and the entry is removed. (#47 reap)
func TestUnmount_DeadMountReapReturnsNilAndDropsEntry(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, common.KnownDevicesDir)
	keyPath := filepath.Join(dir, "id_ed25519")
	mountTo := filepath.Join(dir, "mnt")

	writePubKeyFile(t, knownDir, "device-a")

	unmountErr := errors.New("fusermount: entry for /mnt not found in /etc/mtab")
	unmountFn := func(_ context.Context, _ string, _ bool) error {
		return unmountErr
	}

	m := newTestMounter(t, knownDir, keyPath, nil, unmountFn)
	// checkMountpoint returns true during Mount's verify loop (newTestMounter default),
	// then is overridden to false for the Unmount re-check (simulating a reaped mount).
	mc := agentconfig.MountConfig{Device: "device-a", Share: "docs", To: mountTo}
	require.NoError(t, m.Mount(context.Background(), mc, "device-a", "10.0.0.1", 2222), "Mount()")

	// Now override: path is gone (st_dev == parent — already reaped).
	m.checkMountpoint = func(string) (bool, error) { return false, nil }

	err := m.Unmount("device-a", "docs")
	assert.NoError(t, err, "Unmount() must return nil when mount is already gone (#47 reap)")
	assert.False(t, m.IsActive("device-a", "docs"), "entry must be reaped after dead-mount path")
}

// TestUnmount_RealFailureKeepsEntryAndReturnsError verifies that when the
// unmount command fails AND the path is still a mountpoint, Unmount returns
// an error and retains the entry so a retry is possible.
func TestUnmount_RealFailureKeepsEntryAndReturnsError(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, common.KnownDevicesDir)
	keyPath := filepath.Join(dir, "id_ed25519")
	mountTo := filepath.Join(dir, "mnt")

	writePubKeyFile(t, knownDir, "device-a")

	unmountFn := func(_ context.Context, _ string, _ bool) error {
		return errors.New("device is busy")
	}

	// newTestMounter already sets checkMountpoint to return true, matching the
	// "still a mountpoint" scenario we want for the re-check after unmount fails.
	m := newTestMounter(t, knownDir, keyPath, nil, unmountFn)

	mc := agentconfig.MountConfig{Device: "device-a", Share: "docs", To: mountTo}
	require.NoError(t, m.Mount(context.Background(), mc, "device-a", "10.0.0.1", 2222), "Mount()")

	// checkMountpoint still returns true (default from newTestMounter): still a mountpoint.
	err := m.Unmount("device-a", "docs")
	assert.Error(t, err, "Unmount() must return error for a real failure")
	assert.True(t, m.IsActive("device-a", "docs"), "entry must be retained when unmount truly failed")
}

// TestUnmount_ReapWhenCheckMountpointErrors verifies that when the unmount
// command fails and checkMountpoint itself returns an error (e.g. ENOENT),
// the mount is reaped (treated as gone). (#47 reap)
func TestUnmount_ReapWhenCheckMountpointErrors(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, common.KnownDevicesDir)
	keyPath := filepath.Join(dir, "id_ed25519")
	mountTo := filepath.Join(dir, "mnt")

	writePubKeyFile(t, knownDir, "device-a")

	unmountFn := func(_ context.Context, _ string, _ bool) error {
		return errors.New("umount: not mounted")
	}

	m := newTestMounter(t, knownDir, keyPath, nil, unmountFn)

	mc := agentconfig.MountConfig{Device: "device-a", Share: "docs", To: mountTo}
	require.NoError(t, m.Mount(context.Background(), mc, "device-a", "10.0.0.1", 2222), "Mount()")

	// After mount is recorded, override to simulate ENOENT-style error during re-check.
	m.checkMountpoint = func(string) (bool, error) {
		return false, errors.New("no such file or directory")
	}

	err := m.Unmount("device-a", "docs")
	assert.NoError(t, err, "Unmount() must return nil when checkMountpoint errors (treat as gone, #47 reap)")
	assert.False(t, m.IsActive("device-a", "docs"), "entry must be reaped when checkMountpoint errors")
}

// TestUnmount_RemountAfterReapSucceeds verifies the #47 end-to-end mounter-level
// scenario: after a dead-mount reap, the same device/share can be remounted.
func TestUnmount_RemountAfterReapSucceeds(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, common.KnownDevicesDir)
	keyPath := filepath.Join(dir, "id_ed25519")

	writePubKeyFile(t, knownDir, "device-a")

	unmountFn := func(_ context.Context, _ string, _ bool) error {
		return errors.New("not in mtab")
	}

	m := newTestMounter(t, knownDir, keyPath, nil, unmountFn)
	// checkMountpoint starts as true so Mount's verify loop passes.

	// First mount.
	mc1 := agentconfig.MountConfig{Device: "device-a", Share: "docs", To: filepath.Join(dir, "mnt1")}
	require.NoError(t, m.Mount(context.Background(), mc1, "device-a", "10.0.0.1", 2222), "first Mount()")

	// Switch to false so Unmount's reap path activates.
	m.checkMountpoint = func(string) (bool, error) { return false, nil }

	// Unmount via reap path.
	require.NoError(t, m.Unmount("device-a", "docs"), "Unmount() reap")

	// Re-mount to a new path — must succeed (no "already mounted" error). (#47 reap)
	m.checkMountpoint = func(string) (bool, error) { return true, nil } // Mount verify OK
	mc2 := agentconfig.MountConfig{Device: "device-a", Share: "docs", To: filepath.Join(dir, "mnt2")}
	err := m.Mount(context.Background(), mc2, "device-a", "10.0.0.1", 2222)
	assert.NoError(t, err, "second Mount() after reap must succeed — no 'already mounted' error")
}

// ─── UnmountAll ───────────────────────────────────────────────────────────────

func TestUnmountAll_UnmountsAllActive(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, common.KnownDevicesDir)
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

// TestUnmountAllForce_PassesForceTrueAndCtx verifies that UnmountAllForce calls
// the unmount seam with force=true, while UnmountAll calls it with force=false.
func TestUnmountAllForce_PassesForceTrueAndCtx(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, common.KnownDevicesDir)
	keyPath := filepath.Join(dir, "id_ed25519")

	writePubKeyFile(t, knownDir, "device-a")

	type call struct {
		force bool
	}
	var calls []call

	m := newTestMounter(t, knownDir, keyPath, nil, func(_ context.Context, _ string, force bool) error {
		calls = append(calls, call{force: force})
		return nil
	})

	mc := agentconfig.MountConfig{Device: "device-a", Share: "docs", To: filepath.Join(dir, "mnt")}
	require.NoError(t, m.Mount(context.Background(), mc, "device-a", "10.0.0.1", 2222), "Mount()")

	calls = nil
	require.NoError(t, m.UnmountAll(), "UnmountAll()")
	require.Len(t, calls, 1, "expected 1 unmount call")
	assert.False(t, calls[0].force, "UnmountAll must use force=false")

	// Re-mount and test UnmountAllForce.
	m.checkMountpoint = func(string) (bool, error) { return true, nil }
	require.NoError(t, m.Mount(context.Background(), mc, "device-a", "10.0.0.1", 2222), "re-Mount()")

	calls = nil
	ctx := context.Background()
	require.NoError(t, m.UnmountAllForce(ctx), "UnmountAllForce()")
	require.Len(t, calls, 1, "expected 1 unmount call")
	assert.True(t, calls[0].force, "UnmountAllForce must use force=true")
}

// ─── UnmountDevice ────────────────────────────────────────────────────────────

func TestUnmountDevice_UnmountsOnlyTargetDevice(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, common.KnownDevicesDir)
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
	err := unmountPath(context.Background(), "/tmp/not-a-real-mount-point-xyz", false)
	assert.Error(t, err, "unmountPath() expected error for non-mount path")
}

func TestUnmountPath_Linux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux-specific test")
	}

	// On Linux, unmountPath calls "fusermount -u <path>".
	// Verify it returns an error for a non-mount path.
	err := unmountPath(context.Background(), "/tmp/not-a-real-mount-point-xyz", false)
	assert.Error(t, err, "unmountPath() expected error for non-mount path")
}

// ─── ActiveMounts ─────────────────────────────────────────────────────────────

func TestActiveMounts_ReturnsSnapshot(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, common.KnownDevicesDir)
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

// ─── Mountpoint verification ──────────────────────────────────────────────────

// TestMount_VerifySuccess ensures Mount records an active entry and logs
// success when checkMountpoint returns true.
func TestMount_VerifySuccess(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, common.KnownDevicesDir)
	keyPath := filepath.Join(dir, "id_ed25519")
	mountTo := filepath.Join(dir, "mnt")

	writePubKeyFile(t, knownDir, "device-a")

	m := newTestMounter(t, knownDir, keyPath, nil, nil)
	// checkMountpoint already returns true from newTestMounter.

	mc := agentconfig.MountConfig{Device: "device-a", Share: "docs", To: mountTo}
	require.NoError(t, m.Mount(context.Background(), mc, "device-a", "10.0.0.1", 2222), "Mount() must succeed when mountpoint check passes")
	assert.True(t, m.IsActive("device-a", "docs"), "IsActive() must be true after successful Mount()")
}

// TestMount_VerifyTimeout ensures Mount returns an error and does not record
// the mount when checkMountpoint never returns true within the timeout.
func TestMount_VerifyTimeout(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, common.KnownDevicesDir)
	keyPath := filepath.Join(dir, "id_ed25519")
	mountTo := filepath.Join(dir, "mnt")

	writePubKeyFile(t, knownDir, "device-a")

	m := newTestMounter(t, knownDir, keyPath, nil, nil)
	// Override check to always fail.
	m.checkMountpoint = func(string) (bool, error) { return false, nil }
	m.mountVerifyTimeout = 50 * time.Millisecond
	m.mountVerifyInterval = 10 * time.Millisecond

	mc := agentconfig.MountConfig{Device: "device-a", Share: "docs", To: mountTo}
	err := m.Mount(context.Background(), mc, "device-a", "10.0.0.1", 2222)
	require.Error(t, err, "Mount() must return an error when mountpoint never appears")
	assert.Contains(t, err.Error(), "did not appear", "error should mention that mountpoint did not appear")
	assert.False(t, m.IsActive("device-a", "docs"), "IsActive() must be false when mountpoint verification failed")
}

// TestMount_VerifyCtxCancelled ensures Mount returns a context error and does
// not record the mount when the context is cancelled before the mountpoint appears.
func TestMount_VerifyCtxCancelled(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, common.KnownDevicesDir)
	keyPath := filepath.Join(dir, "id_ed25519")
	mountTo := filepath.Join(dir, "mnt")

	writePubKeyFile(t, knownDir, "device-a")

	m := newTestMounter(t, knownDir, keyPath, nil, nil)
	// Never returns true; context will be cancelled first.
	m.checkMountpoint = func(string) (bool, error) { return false, nil }
	m.mountVerifyTimeout = 10 * time.Second // long timeout, ctx cancels first
	m.mountVerifyInterval = 5 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after a brief moment so the poll loop sees it.
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	mc := agentconfig.MountConfig{Device: "device-a", Share: "docs", To: mountTo}
	err := m.Mount(ctx, mc, "device-a", "10.0.0.1", 2222)
	require.Error(t, err, "Mount() must return an error when context is cancelled")
	assert.False(t, m.IsActive("device-a", "docs"), "IsActive() must be false after ctx cancel")
}

// TestIsMountpoint_TempDirIsNotMountpoint verifies that a plain temp directory
// (same device as its parent) is NOT reported as a mountpoint.
func TestIsMountpoint_TempDirIsNotMountpoint(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	require.NoError(t, os.MkdirAll(sub, 0755), "MkdirAll")

	ok, err := isMountpoint(sub)
	require.NoError(t, err, "isMountpoint() must not error on an accessible directory")
	assert.False(t, ok, "a plain subdirectory must not be reported as a mountpoint")
}

// ─── mountpointGoneCtx: wedged-check guard ────────────────────────────────────

// TestMountpointGoneCtx_BlockingCheckCtxCancelled verifies that mountpointGoneCtx
// returns "gone=true" promptly when the checkMountpoint function blocks and the
// context is cancelled — proving a wedged syscall.Stat cannot re-block a bounded
// shutdown. This is the unit-level proof for #50.1 worst-case. (#50 bounded)
func TestMountpointGoneCtx_BlockingCheckCtxCancelled(t *testing.T) {
	dir := t.TempDir()
	m := newTestMounter(t, dir, filepath.Join(dir, "key"), nil, nil)

	// checkMountpoint blocks forever (wedged Stat simulation).
	blocked := make(chan struct{}) // never closed
	m.checkMountpoint = func(string) (bool, error) {
		<-blocked
		return true, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	gone := m.mountpointGoneCtx(ctx, "/fake/path")
	elapsed := time.Since(start)

	assert.True(t, gone, "mountpointGoneCtx must return gone=true when ctx fires before check returns")
	assert.Less(t, elapsed, 500*time.Millisecond, "mountpointGoneCtx must return promptly, not hang")
}

// TestMountpointGoneCtx_CheckReturnsGone verifies the normal reap path.
func TestMountpointGoneCtx_CheckReturnsGone(t *testing.T) {
	dir := t.TempDir()
	m := newTestMounter(t, dir, filepath.Join(dir, "key"), nil, nil)
	m.checkMountpoint = func(string) (bool, error) { return false, nil }

	gone := m.mountpointGoneCtx(context.Background(), "/fake/path")
	assert.True(t, gone, "mountpointGoneCtx must return gone=true when checkMountpoint returns false")
}

// TestMountpointGoneCtx_CheckReturnsPresent verifies the still-mounted path.
func TestMountpointGoneCtx_CheckReturnsPresent(t *testing.T) {
	dir := t.TempDir()
	m := newTestMounter(t, dir, filepath.Join(dir, "key"), nil, nil)
	m.checkMountpoint = func(string) (bool, error) { return true, nil }

	gone := m.mountpointGoneCtx(context.Background(), "/fake/path")
	assert.False(t, gone, "mountpointGoneCtx must return gone=false when checkMountpoint reports still-mounted")
}

// ─── Wedged-command + wedged-recheck: worst-case #50.1 test ──────────────────

// TestUnmountAllForce_WedgedCommandAndRecheckBoundedByCtx is the worst-case
// scenario test: BOTH the unmount command AND the checkMountpoint re-check
// block. The ctx-bounded force path must return promptly, reap the entry, and
// not hang. This proves #50.1 even under the absolute worst case. (#50 bounded)
func TestUnmountAllForce_WedgedCommandAndRecheckBoundedByCtx(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, common.KnownDevicesDir)
	keyPath := filepath.Join(dir, "id_ed25519")
	mountTo := filepath.Join(dir, "mnt")

	writePubKeyFile(t, knownDir, "device-a")

	// unmount blocks until its ctx is cancelled.
	blocked := make(chan struct{}) // never closed in this test
	unmountFn := func(ctx context.Context, _ string, _ bool) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-blocked:
			return nil
		}
	}

	m := newTestMounter(t, knownDir, keyPath, nil, unmountFn)

	// Mount so there is an entry to unmount. newTestMounter's checkMountpoint
	// already returns true, which lets Mount's verify loop pass.
	mc := agentconfig.MountConfig{Device: "device-a", Share: "docs", To: mountTo}
	require.NoError(t, m.Mount(context.Background(), mc, "device-a", "10.0.0.1", 2222), "Mount()")

	// Now install a blocking checkMountpoint (wedged Stat) for the unmount path:
	// both the command and the re-check are wedged, so only the ctx can unblock.
	m.checkMountpoint = func(string) (bool, error) {
		<-blocked // never unblocked
		return true, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	// UnmountAllForce should return well within our test guard (the budget is 100ms above).
	err := m.UnmountAllForce(ctx)
	elapsed := time.Since(start)

	// The ctx fires: unmount returns ctx.Err(), recheck also hits ctx.Done(),
	// so the entry is reaped and nil is returned.
	assert.NoError(t, err, "UnmountAllForce must return nil (entry reaped via ctx-guard)")
	assert.False(t, m.IsActive("device-a", "docs"), "entry must be reaped after ctx-bounded wedge")
	assert.Less(t, elapsed, 1*time.Second, "UnmountAllForce must not hang when both command and recheck block")
}

// ─── Force-flag propagation ───────────────────────────────────────────────────

// TestUnmountDevice_PassesForceTrueToSeam verifies that UnmountDevice always
// calls the unmount seam with force=true (device-offline teardown should never
// leave wedged mounts). (#50 force)
func TestUnmountDevice_PassesForceTrueToSeam(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, common.KnownDevicesDir)
	keyPath := filepath.Join(dir, "id_ed25519")

	writePubKeyFile(t, knownDir, "device-a")

	var capturedForce atomic.Bool
	unmountFn := func(_ context.Context, _ string, force bool) error {
		capturedForce.Store(force)
		return nil
	}

	m := newTestMounter(t, knownDir, keyPath, nil, unmountFn)

	mc := agentconfig.MountConfig{Device: "device-a", Share: "docs", To: filepath.Join(dir, "mnt")}
	require.NoError(t, m.Mount(context.Background(), mc, "device-a", "10.0.0.1", 2222), "Mount()")

	require.NoError(t, m.UnmountDevice("device-a"), "UnmountDevice()")
	assert.True(t, capturedForce.Load(), "UnmountDevice must call unmount with force=true")
}
