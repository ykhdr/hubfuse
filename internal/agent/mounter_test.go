package agent

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
// returns promptly (does not hang) when the checkMountpoint function blocks and
// the context is cancelled, and that it returns gone=FALSE — a timeout is not
// evidence the mount is gone, so it must NOT trigger a reap. This proves a wedged
// syscall.Stat cannot re-block a bounded teardown (#50) while never falsely
// dropping a possibly-live mount. (#50 bounded, no false reap)
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

	assert.False(t, gone, "mountpointGoneCtx must return gone=false on a deadline (timeout is not evidence the mount is gone)")
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
// block. The ctx-bounded force path must return PROMPTLY (no hang) — this is the
// #50.1 guarantee. Because neither the command nor the re-check could confirm the
// mount is gone within the budget, the entry is RETAINED and an error is
// reported (we never silently drop a possibly-live mount on a mere timeout).
// (#50 bounded, no false reap)
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

	// The ctx fires: the command returns ctx.Err() and the re-check times out
	// (gone=false), so the entry is RETAINED and an error is reported — but the
	// call still returns promptly (no hang), which is the #50.1 guarantee.
	require.Error(t, err, "UnmountAllForce must report an error when it could not confirm the mount is gone")
	assert.True(t, m.IsActive("device-a", "docs"), "a wedged mount must NOT be reaped on a mere timeout (no false reap)")
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

// ─── Guard-target (#49) ───────────────────────────────────────────────────────

// captureWarnLogger returns a logger that captures WARN+ output into buf.
func captureWarnLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
}

// newTestMounterWithLogger is like newTestMounter but uses a caller-supplied logger
// so tests can assert on logged warnings.
func newTestMounterWithLogger(t *testing.T, knownDevicesDir, keyPath string, logger *slog.Logger, capturedArgs *[]string, unmountFn func(context.Context, string, bool) error) *Mounter {
	t.Helper()
	knownHostsDir := filepath.Join(filepath.Dir(knownDevicesDir), common.KnownHostsDir)
	m := NewMounter(keyPath, knownDevicesDir, knownHostsDir, "", logger)

	m.execCommand = func(_ context.Context, name string, args ...string) *exec.Cmd {
		if capturedArgs != nil {
			*capturedArgs = append([]string{name}, args...)
		}
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

	m.checkMountpoint = func(string) (bool, error) { return true, nil }
	m.mountVerifyTimeout = 500 * time.Millisecond
	m.mountVerifyInterval = 10 * time.Millisecond
	return m
}

// TestGuardTarget_MountRestrictsOnCreation verifies that Mount creates the target
// and makes it mountable (mountableMode) just before attaching the backend — a
// real FUSE mount then masks this mode, and unmount re-applies guardMode (covered
// by TestGuardTarget_Reguard*). The test mounter never creates a real mount, so
// the underlying mountableMode is directly observable. fusermount3 (Linux)
// requires the write bit at mount time, which is why this is not guardMode. (#49)
func TestGuardTarget_MountRestrictsOnCreation(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, common.KnownDevicesDir)
	keyPath := filepath.Join(dir, "id_ed25519")
	mountTo := filepath.Join(dir, "mnt", "docs")

	writePubKeyFile(t, knownDir, "device-a")
	m := newTestMounter(t, knownDir, keyPath, nil, nil)

	// Restore dir perms before t.TempDir cleanup.
	t.Cleanup(func() { _ = os.Chmod(mountTo, 0o755) })

	mc := agentconfig.MountConfig{Device: "device-a", Share: "docs", To: mountTo}
	require.NoError(t, m.Mount(context.Background(), mc, "device-a", "10.0.0.1", 2222))

	info, err := os.Stat(mountTo)
	require.NoError(t, err, "mount target must exist")
	assert.Equal(t, mountableMode, info.Mode().Perm(), "mount target must be made mountable (owner-writable) for the backend to attach")
}

// TestGuardTarget_RemountActiveKeyDoesNotChmod verifies that Mount for a key that
// is already active is rejected as "already mounted" WITHOUT running guardTarget.
// guardTarget runs only after the already-mounted check, so it can never chmod a
// path that is currently a live mount of ours. (#49 guard ordering)
func TestGuardTarget_RemountActiveKeyDoesNotChmod(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, common.KnownDevicesDir)
	keyPath := filepath.Join(dir, "id_ed25519")
	mountTo := filepath.Join(dir, "mnt", "docs")

	writePubKeyFile(t, knownDir, "device-a")
	m := newTestMounter(t, knownDir, keyPath, nil, nil)
	t.Cleanup(func() { _ = os.Chmod(mountTo, 0o755) })

	mc := agentconfig.MountConfig{Device: "device-a", Share: "docs", To: mountTo}
	require.NoError(t, m.Mount(context.Background(), mc, "device-a", "10.0.0.1", 2222))

	// Simulate a live FUSE mount root exposing a distinct mode that a re-mount
	// must not clobber.
	const sentinel os.FileMode = 0o755
	require.NoError(t, os.Chmod(mountTo, sentinel))

	err := m.Mount(context.Background(), mc, "device-a", "10.0.0.1", 2222)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "already mounted")

	info, statErr := os.Stat(mountTo)
	require.NoError(t, statErr)
	assert.Equal(t, sentinel, info.Mode().Perm(), "re-mount of an active key must not chmod the (live) mount target")
}

// TestGuardTarget_FailedMountLeavesTargetRestricted verifies that when Mount
// fails the verify loop (mountpoint never appears), the target dir is at
// guardMode — not a writable local dir. This is the #49 core regression test.
// (#49 test 2)
func TestGuardTarget_FailedMountLeavesTargetRestricted(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, common.KnownDevicesDir)
	keyPath := filepath.Join(dir, "id_ed25519")
	mountTo := filepath.Join(dir, "mnt", "docs")

	writePubKeyFile(t, knownDir, "device-a")
	m := newTestMounter(t, knownDir, keyPath, nil, nil)
	// checkMountpoint always returns false → verify loop times out.
	m.checkMountpoint = func(string) (bool, error) { return false, nil }
	m.mountVerifyTimeout = 30 * time.Millisecond
	m.mountVerifyInterval = 5 * time.Millisecond

	// Restore dir perms before t.TempDir cleanup.
	t.Cleanup(func() { _ = os.Chmod(mountTo, 0o755) })

	mc := agentconfig.MountConfig{Device: "device-a", Share: "docs", To: mountTo}
	err := m.Mount(context.Background(), mc, "device-a", "10.0.0.1", 2222)
	require.Error(t, err, "Mount() must fail when mountpoint never appears")
	assert.False(t, m.IsActive("device-a", "docs"), "mount must not be recorded on failure")

	info, statErr := os.Stat(mountTo)
	require.NoError(t, statErr, "target dir must still exist after failed mount")
	assert.Equal(t, guardMode, info.Mode().Perm(), "target must be restricted to guardMode after failed mount (#49 core)")

	// Bonus: touch into the dir must be denied for non-root users.
	if os.Geteuid() == 0 {
		t.Skip("running as root — mode bits are not enforced, skipping write-denied assertion")
	}
	f, touchErr := os.Create(filepath.Join(mountTo, "canary.txt"))
	if f != nil {
		_ = f.Close()
	}
	assert.Error(t, touchErr, "write into a 0o500 dir must be denied (EACCES)")
}

// TestGuardTarget_NonEmptyTargetRefused verifies that Mount refuses to mount
// over a non-empty local directory, with an actionable WARN naming the path.
// The unmount/exec seam must never be invoked. (#49 test 3)
func TestGuardTarget_NonEmptyTargetRefused(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, common.KnownDevicesDir)
	keyPath := filepath.Join(dir, "id_ed25519")
	mountTo := filepath.Join(dir, "mnt", "docs")

	writePubKeyFile(t, knownDir, "device-a")

	// Pre-create the target with a file inside (chmod to writable first so we
	// can plant the file, then we leave it writable — the guard logic must detect
	// the file and refuse regardless of dir mode).
	require.NoError(t, os.MkdirAll(mountTo, 0o755))
	plantedFile := filepath.Join(mountTo, "planted.txt")
	require.NoError(t, os.WriteFile(plantedFile, []byte("local content"), 0644))

	// Restore the dir to writable before test cleanup so t.TempDir can remove it.
	t.Cleanup(func() {
		_ = os.Chmod(mountTo, 0o755)
	})

	var logBuf bytes.Buffer
	logger := captureWarnLogger(&logBuf)

	var execCalled atomic.Bool
	m := newTestMounterWithLogger(t, knownDir, keyPath, logger, nil, nil)
	m.execCommand = func(_ context.Context, _ string, _ ...string) *exec.Cmd {
		execCalled.Store(true)
		return exec.Command("true")
	}
	// checkMountpoint returns false so the dir is NOT seen as a mountpoint.
	m.checkMountpoint = func(string) (bool, error) { return false, nil }
	m.mountVerifyTimeout = 30 * time.Millisecond
	m.mountVerifyInterval = 5 * time.Millisecond

	mc := agentconfig.MountConfig{Device: "device-a", Share: "docs", To: mountTo}
	err := m.Mount(context.Background(), mc, "device-a", "10.0.0.1", 2222)
	require.Error(t, err, "Mount() must refuse to mount over a non-empty target")

	// The error message should mention the local content.
	assert.Contains(t, err.Error(), "local file", "error must mention local files")

	// The WARN must name the path.
	logOutput := logBuf.String()
	assert.True(t, strings.Contains(logOutput, mountTo), "WARN must name the target path")

	// The exec seam must never be invoked — no sshfs process was started.
	assert.False(t, execCalled.Load(), "exec seam must not be invoked when refusing non-empty target")

	// The planted file must be untouched.
	data, readErr := os.ReadFile(plantedFile)
	require.NoError(t, readErr, "planted file must still exist")
	assert.Equal(t, "local content", string(data), "planted file must be untouched")
}

// TestGuardTarget_EmptyTargetProceeds verifies that Mount accepts an empty
// pre-existing target directory (no non-empty refusal). (#49 test 4)
func TestGuardTarget_EmptyTargetProceeds(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, common.KnownDevicesDir)
	keyPath := filepath.Join(dir, "id_ed25519")
	mountTo := filepath.Join(dir, "mnt", "docs")

	writePubKeyFile(t, knownDir, "device-a")
	require.NoError(t, os.MkdirAll(mountTo, 0o755))

	m := newTestMounter(t, knownDir, keyPath, nil, nil)

	mc := agentconfig.MountConfig{Device: "device-a", Share: "docs", To: mountTo}
	require.NoError(t, m.Mount(context.Background(), mc, "device-a", "10.0.0.1", 2222), "Mount() must accept an empty target dir")
	assert.True(t, m.IsActive("device-a", "docs"), "mount must be active after empty-target Mount")
}

// TestGuardTarget_ReguardAfterSuccessfulUnmount verifies that after a clean
// Unmount the target dir is re-restricted to guardMode. (#49 test 5)
func TestGuardTarget_ReguardAfterSuccessfulUnmount(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, common.KnownDevicesDir)
	keyPath := filepath.Join(dir, "id_ed25519")
	mountTo := filepath.Join(dir, "mnt", "docs")

	writePubKeyFile(t, knownDir, "device-a")
	m := newTestMounter(t, knownDir, keyPath, nil, nil)

	// Restore dir perms before t.TempDir cleanup.
	t.Cleanup(func() { _ = os.Chmod(mountTo, 0o755) })

	mc := agentconfig.MountConfig{Device: "device-a", Share: "docs", To: mountTo}
	require.NoError(t, m.Mount(context.Background(), mc, "device-a", "10.0.0.1", 2222))
	require.NoError(t, m.Unmount("device-a", "docs"))

	assert.False(t, m.IsActive("device-a", "docs"), "mount must be inactive after Unmount")

	info, err := os.Stat(mountTo)
	require.NoError(t, err, "target must still exist after Unmount")
	assert.Equal(t, guardMode, info.Mode().Perm(), "target must be re-restricted to guardMode after unmount (#49 test 5)")
}

// TestGuardTarget_ReguardAfterReap verifies that after a reap (unmount cmd
// fails but the mount is gone), the target is re-restricted to guardMode.
// (#49 test 6)
func TestGuardTarget_ReguardAfterReap(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, common.KnownDevicesDir)
	keyPath := filepath.Join(dir, "id_ed25519")
	mountTo := filepath.Join(dir, "mnt", "docs")

	writePubKeyFile(t, knownDir, "device-a")

	unmountFn := func(_ context.Context, _ string, _ bool) error {
		return errors.New("not in mtab")
	}
	m := newTestMounter(t, knownDir, keyPath, nil, unmountFn)

	// Restore dir perms before t.TempDir cleanup.
	t.Cleanup(func() { _ = os.Chmod(mountTo, 0o755) })

	mc := agentconfig.MountConfig{Device: "device-a", Share: "docs", To: mountTo}
	require.NoError(t, m.Mount(context.Background(), mc, "device-a", "10.0.0.1", 2222))

	// Switch checkMountpoint to false → triggers the reap path.
	m.checkMountpoint = func(string) (bool, error) { return false, nil }

	require.NoError(t, m.Unmount("device-a", "docs"), "reap path must return nil")
	assert.False(t, m.IsActive("device-a", "docs"), "entry must be reaped")

	info, err := os.Stat(mountTo)
	require.NoError(t, err, "target must still exist after reap")
	assert.Equal(t, guardMode, info.Mode().Perm(), "target must be re-restricted to guardMode after reap (#49 test 6)")
}

// TestGuardTarget_ShutdownDoesNotReguard verifies that UnmountAllForce (shutdown)
// does NOT re-guard the target (reguard=false). (#49 test 7)
func TestGuardTarget_ShutdownDoesNotReguard(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, common.KnownDevicesDir)
	keyPath := filepath.Join(dir, "id_ed25519")
	mountTo := filepath.Join(dir, "mnt", "docs")

	writePubKeyFile(t, knownDir, "device-a")
	m := newTestMounter(t, knownDir, keyPath, nil, nil)

	mc := agentconfig.MountConfig{Device: "device-a", Share: "docs", To: mountTo}
	require.NoError(t, m.Mount(context.Background(), mc, "device-a", "10.0.0.1", 2222))

	// Chmod the target to a sentinel mode (0o700) to detect any re-guard.
	require.NoError(t, os.Chmod(mountTo, 0o700), "set sentinel mode")

	ctx := context.Background()
	require.NoError(t, m.UnmountAllForce(ctx))

	assert.False(t, m.IsActive("device-a", "docs"), "entry must be gone after UnmountAllForce")

	info, err := os.Stat(mountTo)
	require.NoError(t, err, "target must still exist")
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(), "shutdown must NOT re-guard (sentinel mode unchanged, reguard=false)")
}

// TestTargetHasLocalContents_Table is a unit test covering the four cases
// described in test 11 of the plan. (#49 test 11)
func TestTargetHasLocalContents_Table(t *testing.T) {
	dir := t.TempDir()
	m := newTestMounter(t, dir, filepath.Join(dir, "key"), nil, nil)

	t.Run("absent dir returns 0", func(t *testing.T) {
		absent := filepath.Join(dir, "nonexistent")
		count, err := m.targetHasLocalContents(absent)
		require.NoError(t, err)
		assert.Equal(t, 0, count)
	})

	t.Run("empty dir returns 0", func(t *testing.T) {
		empty := filepath.Join(dir, "empty")
		require.NoError(t, os.MkdirAll(empty, 0o755))
		count, err := m.targetHasLocalContents(empty)
		require.NoError(t, err)
		assert.Equal(t, 0, count)
	})

	t.Run("dir with two files returns 2", func(t *testing.T) {
		withFiles := filepath.Join(dir, "nonempty")
		require.NoError(t, os.MkdirAll(withFiles, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(withFiles, "x.txt"), []byte("hi"), 0644))
		require.NoError(t, os.WriteFile(filepath.Join(withFiles, "y.txt"), []byte("yo"), 0644))
		count, err := m.targetHasLocalContents(withFiles)
		require.NoError(t, err)
		assert.Equal(t, 2, count)
	})

	// Note: the plan mentions "checkMountpoint says is a mountpoint → (false, nil)"
	// but per the final plan-review fix (item 1), targetHasLocalContents does NOT
	// call checkMountpoint. Enumerating a pre-existing foreign mountpoint and
	// refusing to mount over it is the correct, desired behavior. So we do not add
	// a mountpoint sub-case here.
}

// TestGuardTarget_StubNoOp verifies that guardTarget and targetHasLocalContents
// are no-ops when the Mounter's stub field is true. (#49 test — stub no-op)
func TestGuardTarget_StubNoOp(t *testing.T) {
	dir := t.TempDir()
	m := newTestMounter(t, dir, filepath.Join(dir, "key"), nil, nil)
	m.stub = true // simulate HUBFUSE_STUB_MOUNT_DIR

	target := filepath.Join(dir, "stubdir")
	require.NoError(t, os.MkdirAll(target, 0o755))

	// guardTarget must leave the mode unchanged.
	before, err := os.Stat(target)
	require.NoError(t, err)
	require.NoError(t, m.guardTarget(target), "guardTarget must not error under stub")

	after, err := os.Stat(target)
	require.NoError(t, err)
	assert.Equal(t, before.Mode().Perm(), after.Mode().Perm(), "guardTarget must be a no-op under stub")

	// targetHasLocalContents must return 0 even when the dir has a file.
	require.NoError(t, os.WriteFile(filepath.Join(target, "x.txt"), []byte("hi"), 0644))
	count, err := m.targetHasLocalContents(target)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "targetHasLocalContents must return 0 under stub")
}
