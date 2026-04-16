package agent

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	agentconfig "github.com/ykhdr/hubfuse/internal/agent/config"
)

// discardLogger returns a logger that discards all output.
func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

// newTestMounter creates a Mounter with test overrides.
// capturedArgs receives the args of the most recent sshfs invocation.
// unmountFn is called when Unmount is invoked; if nil, a no-op is used.
func newTestMounter(t *testing.T, _, keyPath string, capturedArgs *[]string, unmountFn func(string) error) *Mounter {
	t.Helper()
	m := NewMounter(keyPath, discardLogger())

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
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatalf("MkdirAll(%q): %v", dir, err)
	}
	path := filepath.Join(dir, deviceID+".pub")
	if err := os.WriteFile(path, []byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItest\n"), 0644); err != nil {
		t.Fatalf("WriteFile(%q): %v", path, err)
	}
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

	if err := m.Mount(context.Background(), mc, "192.168.1.10", 2222); err != nil {
		t.Fatalf("Mount(): %v", err)
	}

	// Expect: sshfs -p 2222 -o IdentityFile=<key> -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null hubfuse@192.168.1.10:documents <mountTo>
	want := []string{
		"sshfs",
		"-p", "2222",
		"-o", "IdentityFile=" + keyPath,
		"-o", "StrictHostKeyChecking=no",
		"-o", "UserKnownHostsFile=/dev/null",
		"hubfuse@192.168.1.10:documents",
		mountTo,
	}

	if len(capturedArgs) != len(want) {
		t.Fatalf("sshfs args = %v, want %v", capturedArgs, want)
	}
	for i, arg := range want {
		if capturedArgs[i] != arg {
			t.Errorf("sshfs arg[%d] = %q, want %q", i, capturedArgs[i], arg)
		}
	}
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

	if err := m.Mount(context.Background(), mc, "10.0.0.1", 2222); err != nil {
		t.Fatalf("Mount(): %v", err)
	}

	if _, err := os.Stat(mountTo); err != nil {
		t.Errorf("mount point directory not created: %v", err)
	}
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

	if err := m.Mount(context.Background(), mc, "10.0.0.1", 2222); err != nil {
		t.Fatalf("first Mount(): %v", err)
	}

	mc2 := agentconfig.MountConfig{
		Device: "device-a",
		Share:  "docs",
		To:     filepath.Join(dir, "mnt2"),
	}

	err := m.Mount(context.Background(), mc2, "10.0.0.1", 2222)
	if err == nil {
		t.Fatal("second Mount() expected error for duplicate, got nil")
	}
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

	if err := m.Mount(context.Background(), mc, "192.168.1.5", 2222); err != nil {
		t.Fatalf("Mount(): %v", err)
	}

	if !m.IsActive("device-a", "docs") {
		t.Error("IsActive() = false, want true after Mount()")
	}

	mounts := m.ActiveMounts()
	if len(mounts) != 1 {
		t.Fatalf("ActiveMounts() length = %d, want 1", len(mounts))
	}
	mnt := mounts[0]
	if mnt.Device != "device-a" {
		t.Errorf("Mount.Device = %q, want \"device-a\"", mnt.Device)
	}
	if mnt.Share != "docs" {
		t.Errorf("Mount.Share = %q, want \"docs\"", mnt.Share)
	}
	if mnt.IP != "192.168.1.5" {
		t.Errorf("Mount.IP = %q, want \"192.168.1.5\"", mnt.IP)
	}
	if mnt.SSHPort != 2222 {
		t.Errorf("Mount.SSHPort = %d, want 2222", mnt.SSHPort)
	}
	if mnt.LocalPath != mountTo {
		t.Errorf("Mount.LocalPath = %q, want %q", mnt.LocalPath, mountTo)
	}
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
	if err := m.Mount(context.Background(), mc, "10.0.0.1", 2222); err != nil {
		t.Fatalf("Mount(): %v", err)
	}

	if err := m.Unmount("device-a", "docs"); err != nil {
		t.Fatalf("Unmount(): %v", err)
	}

	if m.IsActive("device-a", "docs") {
		t.Error("IsActive() = true after Unmount(), want false")
	}
}

func TestUnmount_ErrorForNonExistentMount(t *testing.T) {
	dir := t.TempDir()
	m := newTestMounter(t, dir, filepath.Join(dir, "key"), nil, nil)

	err := m.Unmount("no-device", "no-share")
	if err == nil {
		t.Fatal("Unmount() expected error for non-existent mount, got nil")
	}
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
	if err := m.Mount(context.Background(), mc, "10.0.0.1", 2222); err != nil {
		t.Fatalf("Mount(): %v", err)
	}

	if err := m.Unmount("device-a", "docs"); err != nil {
		t.Fatalf("Unmount(): %v", err)
	}

	if unmountedPath != mountTo {
		t.Errorf("unmount called with path %q, want %q", unmountedPath, mountTo)
	}
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
		if err := m.Mount(context.Background(), mc, "10.0.0.1", 2222); err != nil {
			t.Fatalf("Mount(%q/%q): %v", mc.Device, mc.Share, err)
		}
	}

	if len(m.ActiveMounts()) != 3 {
		t.Fatalf("expected 3 active mounts, got %d", len(m.ActiveMounts()))
	}

	if err := m.UnmountAll(); err != nil {
		t.Fatalf("UnmountAll(): %v", err)
	}

	if len(m.ActiveMounts()) != 0 {
		t.Errorf("ActiveMounts() after UnmountAll = %d, want 0", len(m.ActiveMounts()))
	}
}

func TestUnmountAll_EmptyIsNoOp(t *testing.T) {
	dir := t.TempDir()
	m := newTestMounter(t, dir, filepath.Join(dir, "key"), nil, nil)

	if err := m.UnmountAll(); err != nil {
		t.Fatalf("UnmountAll() on empty mounter: %v", err)
	}
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
		if err := m.Mount(context.Background(), mc, "10.0.0.1", 2222); err != nil {
			t.Fatalf("Mount(%q/%q): %v", mc.Device, mc.Share, err)
		}
	}

	if err := m.UnmountDevice("device-a"); err != nil {
		t.Fatalf("UnmountDevice(): %v", err)
	}

	if m.IsActive("device-a", "docs") {
		t.Error("device-a/docs still active after UnmountDevice(device-a)")
	}
	if m.IsActive("device-a", "photos") {
		t.Error("device-a/photos still active after UnmountDevice(device-a)")
	}
	if !m.IsActive("device-b", "music") {
		t.Error("device-b/music should still be active")
	}
}

// ─── IsActive ─────────────────────────────────────────────────────────────────

func TestIsActive_FalseWhenNotMounted(t *testing.T) {
	dir := t.TempDir()
	m := newTestMounter(t, dir, filepath.Join(dir, "key"), nil, nil)

	if m.IsActive("device-a", "docs") {
		t.Error("IsActive() = true for unmounted share, want false")
	}
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
	if err == nil {
		t.Error("unmountPath() expected error for non-mount path, got nil")
	}
}

func TestUnmountPath_Linux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux-specific test")
	}

	// On Linux, unmountPath calls "fusermount -u <path>".
	// Verify it returns an error for a non-mount path.
	err := unmountPath("/tmp/not-a-real-mount-point-xyz")
	if err == nil {
		t.Error("unmountPath() expected error for non-mount path, got nil")
	}
}

// ─── ActiveMounts ─────────────────────────────────────────────────────────────

func TestActiveMounts_ReturnsSnapshot(t *testing.T) {
	dir := t.TempDir()
	knownDir := filepath.Join(dir, "known_devices")
	keyPath := filepath.Join(dir, "id_ed25519")

	writePubKeyFile(t, knownDir, "device-a")

	m := newTestMounter(t, knownDir, keyPath, nil, nil)

	mounts := m.ActiveMounts()
	if len(mounts) != 0 {
		t.Fatalf("ActiveMounts() on empty mounter = %d, want 0", len(mounts))
	}

	mc := agentconfig.MountConfig{
		Device: "device-a",
		Share:  "docs",
		To:     filepath.Join(dir, "mnt"),
	}
	if err := m.Mount(context.Background(), mc, "10.0.0.1", 2222); err != nil {
		t.Fatalf("Mount(): %v", err)
	}

	mounts = m.ActiveMounts()
	if len(mounts) != 1 {
		t.Fatalf("ActiveMounts() = %d, want 1", len(mounts))
	}
}
