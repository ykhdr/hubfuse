package agent

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeStubMarker writes a minimal stub-sshfs marker for mountPath recording
// pid, mirroring what tests/tools/stub-sshfs writes. Returns the marker path.
func writeStubMarker(t *testing.T, markerDir, mountPath string, pid int) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(markerDir, 0o755))
	p := stubMarkerPath(markerDir, mountPath)
	require.NoError(t, os.WriteFile(p, []byte(fmt.Sprintf(`{"dst":%q,"pid":%d}`, mountPath, pid)), 0o644))
	return p
}

// spawnLiveProcess starts a long-sleeping child and returns its PID. The child
// is killed and reaped on test cleanup, so it never outlives the test.
func spawnLiveProcess(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	require.NoError(t, cmd.Start(), "start sleep")
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd.Process.Pid
}

// deadPID returns the PID of a process that has already exited AND been
// reaped, so kill(pid, 0) yields ESRCH. The kernel allocates PIDs
// sequentially and does not reuse one immediately, so the stale PID stays
// reliably dead for the duration of the test.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	require.NoError(t, cmd.Run(), "run true")
	return cmd.Process.Pid
}

// startMarkerRemovingStub spawns a process that mimics stub-sshfs's marker
// contract end to end: it installs a SIGTERM trap that removes markerPath
// (the stub's defer), THEN writes the marker itself recording its own PID —
// so once the marker is visible the trap is guaranteed installed (signalling
// any earlier would race the shell's startup and default-kill it before the
// trap exists). The helper blocks until the marker appears. Killed and reaped
// on cleanup in case the test never signals it.
func startMarkerRemovingStub(t *testing.T, markerPath string) int {
	t.Helper()
	script := fmt.Sprintf(
		`trap 'rm -f %q; exit 0' TERM; printf '{"pid":%%d}' "$$" > %q; while :; do sleep 0.05; done`,
		markerPath, markerPath)
	cmd := exec.Command("sh", "-c", script)
	require.NoError(t, cmd.Start(), "start marker-removing stub")
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	require.Eventually(t, func() bool {
		_, err := os.Stat(markerPath)
		return err == nil
	}, 5*time.Second, 10*time.Millisecond, "stub must write its marker (readiness signal)")
	return cmd.Process.Pid
}

func TestStubSanitizePath(t *testing.T) {
	// Pins the transformation shared (by copy — the packages cannot import
	// each other) with sanitize() in tests/tools/stub-sshfs/main.go and
	// sanitizeForMarker() in tests/scenarios/helpers/agent.go. If this test
	// needs updating, all three copies must change together.
	tests := []struct {
		in   string
		want string
	}{
		{"/mnt/data", "mnt_data"},
		{"/mnt/my share", "mnt_my_share"},
		{`/a\b:c`, "a_b_c"},
		{"relative/path", "relative_path"},
		{"/", ""},
	}
	for _, tc := range tests {
		assert.Equal(t, tc.want, stubSanitizePath(tc.in), "stubSanitizePath(%q)", tc.in)
	}
}

// TestStubMountpointCheck_Classification pins the liveness semantics: a mount
// is "mounted" iff its marker exists and records a live PID; every failure
// shape is positively "not mounted" — (false, nil), never an error. (#67)
func TestStubMountpointCheck_Classification(t *testing.T) {
	mountPath := "/mnt/stub-target"

	t.Run("no marker reports not mounted", func(t *testing.T) {
		check := stubMountpointCheck(t.TempDir())
		ok, err := check(mountPath)
		require.NoError(t, err)
		assert.False(t, ok, "absent marker is the authoritative dead signal")
	})

	t.Run("live PID reports mounted", func(t *testing.T) {
		markerDir := t.TempDir()
		writeStubMarker(t, markerDir, mountPath, spawnLiveProcess(t))
		ok, err := stubMountpointCheck(markerDir)(mountPath)
		require.NoError(t, err)
		assert.True(t, ok, "marker + live PID = mounted")
	})

	t.Run("dead PID reports not mounted", func(t *testing.T) {
		markerDir := t.TempDir()
		writeStubMarker(t, markerDir, mountPath, deadPID(t))
		ok, err := stubMountpointCheck(markerDir)(mountPath)
		require.NoError(t, err)
		assert.False(t, ok, "marker whose PID is dead (ESRCH) = not mounted")
	})

	t.Run("corrupt JSON reports not mounted", func(t *testing.T) {
		markerDir := t.TempDir()
		p := stubMarkerPath(markerDir, mountPath)
		require.NoError(t, os.WriteFile(p, []byte("{not json"), 0o644))
		ok, err := stubMountpointCheck(markerDir)(mountPath)
		require.NoError(t, err)
		assert.False(t, ok, "unparsable marker cannot vouch for a process")
	})

	t.Run("marker without a positive pid reports not mounted", func(t *testing.T) {
		markerDir := t.TempDir()
		p := stubMarkerPath(markerDir, mountPath)
		// pid omitted → 0; kill(0, 0) would probe our own process GROUP, so
		// the harness must refuse to treat it as a liveness witness.
		require.NoError(t, os.WriteFile(p, []byte(`{"dst":"/mnt/stub-target"}`), 0o644))
		ok, err := stubMountpointCheck(markerDir)(mountPath)
		require.NoError(t, err)
		assert.False(t, ok, "pid <= 0 must not be probed")
	})
}

// TestStubPIDAlive_ZombieCaveat documents the caveat from the stubmount.go
// header: an exited-but-unreaped child (zombie) still reads as ALIVE, because
// kill(pid, 0) does not return ESRCH for zombies. This is why scenario tests
// must kill stubs with SIGTERM (marker removal via defer) and never SIGKILL.
func TestStubPIDAlive_ZombieCaveat(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	require.NoError(t, cmd.Start())
	pid := cmd.Process.Pid

	require.True(t, stubPIDAlive(pid), "running child must read alive")

	require.NoError(t, cmd.Process.Kill()) // SIGKILL: no defer would run in a real stub
	// Not reaped yet — the zombie still "exists" as far as kill(pid, 0) goes.
	// SIGKILL delivery makes the process a zombie; until Wait() reaps it,
	// kill(pid, 0) keeps succeeding.
	assert.True(t, stubPIDAlive(pid), "unreaped zombie must (unfortunately) read alive — the documented caveat")

	var exitErr *exec.ExitError
	require.ErrorAs(t, cmd.Wait(), &exitErr, "SIGKILLed child exits via signal")
	assert.False(t, stubPIDAlive(pid), "after reaping, the PID reads dead (ESRCH)")
}

// TestStubUnmount_TermsStubAndWaitsForMarker is the success path: stubUnmount
// SIGTERMs the process recorded in the marker and returns nil once the
// process's teardown (the stub's defer) has removed the marker. (#67)
func TestStubUnmount_TermsStubAndWaitsForMarker(t *testing.T) {
	markerDir := t.TempDir()
	mountPath := "/mnt/stub-target"
	markerPath := stubMarkerPath(markerDir, mountPath)

	// The stub writes its own marker (with its own PID), exactly like
	// stub-sshfs does; returns once the marker is on disk.
	pid := startMarkerRemovingStub(t, markerPath)

	err := stubUnmount(markerDir)(context.Background(), mountPath, false)
	require.NoError(t, err, "SIGTERM + marker removal is a successful unmount")

	_, statErr := os.Stat(markerPath)
	assert.True(t, os.IsNotExist(statErr), "marker must be gone after a successful stub unmount")
	_ = pid // reaped by the spawn helper's cleanup
}

// TestStubUnmount_NoMarkerIsSuccess: no marker means the stub already exited
// and cleaned up — unmounting an already-dead stub mount succeeds (mirrors
// unmountKey's reap-and-succeed semantics for an already-gone mount).
func TestStubUnmount_NoMarkerIsSuccess(t *testing.T) {
	err := stubUnmount(t.TempDir())(context.Background(), "/mnt/never-mounted", false)
	require.NoError(t, err)
}

// TestStubUnmount_CorruptMarkerErrors: a marker with no usable PID cannot be
// torn down and will never disappear on its own — stubUnmount fails fast
// (unmountKey's re-check then reaps the entry via stubMountpointCheck, which
// reads a corrupt marker as "not mounted").
func TestStubUnmount_CorruptMarkerErrors(t *testing.T) {
	markerDir := t.TempDir()
	mountPath := "/mnt/stub-target"
	require.NoError(t, os.WriteFile(stubMarkerPath(markerDir, mountPath), []byte("{not json"), 0o644))

	err := stubUnmount(markerDir)(context.Background(), mountPath, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no usable pid")
}

// TestStubUnmount_MarkerPersistsTimesOut: the signalled process dies without
// removing its marker (a contract violation a real stub never commits — its
// removal is a defer), so the wait loop must give up with an error instead of
// pretending the unmount succeeded.
func TestStubUnmount_MarkerPersistsTimesOut(t *testing.T) {
	markerDir := t.TempDir()
	mountPath := "/mnt/stub-target"

	// The sleeping child dies on SIGTERM but never removes the marker.
	pid := spawnLiveProcess(t)
	markerPath := writeStubMarker(t, markerDir, mountPath, pid)

	unmount := stubUnmountWithWait(markerDir, 150*time.Millisecond, 10*time.Millisecond)
	err := unmount(context.Background(), mountPath, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "still present")

	_, statErr := os.Stat(markerPath)
	assert.NoError(t, statErr, "stubUnmount must not remove the marker itself — that is the stub's job")
}

// TestStubUnmount_DeadPIDWithPersistentMarkerErrors: a marker whose PID is
// already dead (ESRCH) yet still on disk means the stub was killed without
// SIGTERM (its defer never ran). That stale harness state is reported as an
// error, not papered over — the zombie caveat in stubmount.go's header.
func TestStubUnmount_DeadPIDWithPersistentMarkerErrors(t *testing.T) {
	markerDir := t.TempDir()
	mountPath := "/mnt/stub-target"
	writeStubMarker(t, markerDir, mountPath, deadPID(t))

	err := stubUnmount(markerDir)(context.Background(), mountPath, false)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "persists")
}

// TestStubUnmount_CancelledCtxAborts: the marker-disappearance wait respects
// the caller's context (unmount callers pass bounded contexts — 3s
// interactive, 5s device-offline/shutdown), so a cancelled ctx aborts the
// wait promptly instead of burning the full stubUnmountWait.
func TestStubUnmount_CancelledCtxAborts(t *testing.T) {
	markerDir := t.TempDir()
	mountPath := "/mnt/stub-target"

	// The process dies on SIGTERM without removing its marker, so only the
	// ctx check can end the wait loop.
	pid := spawnLiveProcess(t)
	writeStubMarker(t, markerDir, mountPath, pid)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := stubUnmount(markerDir)(ctx, mountPath, false)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Less(t, time.Since(start), stubUnmountWait, "cancelled ctx must end the wait early")
}

// TestNewMounter_StubModeInstallsMarkerHarness verifies the NewMounter wiring:
// with HUBFUSE_STUB_MOUNT_DIR set, checkMountpoint consults the stub marker
// protocol (not the real filesystem) and unmount is stubUnmount (not the real
// fusermount ladder). (#67 truthful stub)
func TestNewMounter_StubModeInstallsMarkerHarness(t *testing.T) {
	markerDir := t.TempDir()
	t.Setenv("HUBFUSE_STUB_MOUNT_DIR", markerDir)

	dir := t.TempDir()
	m := NewMounter(filepath.Join(dir, "key"), dir, filepath.Join(dir, "known_hosts"), "", discardLogger())
	require.True(t, m.stub, "HUBFUSE_STUB_MOUNT_DIR must flip stub mode on")

	mountPath := filepath.Join(dir, "mnt")
	require.NoError(t, os.MkdirAll(mountPath, 0o755))

	// The old hardcode reported every path as mounted; the marker harness must
	// report "not mounted" for a real, existing directory with no marker.
	ok, err := m.checkMountpoint(mountPath)
	require.NoError(t, err)
	assert.False(t, ok, "no marker → not mounted, even though the directory exists")

	// A marker recording a live PID (our own) flips the verdict to mounted.
	markerPath := writeStubMarker(t, markerDir, mountPath, os.Getpid())
	ok, err = m.checkMountpoint(mountPath)
	require.NoError(t, err)
	assert.True(t, ok, "marker + live PID → mounted")

	// unmount is stubUnmount, not unmountPath: with no marker it succeeds
	// (already-dead semantics), whereas the real fusermount ladder would fail
	// against a path that was never a mountpoint. (Remove the marker first —
	// it records the test process's own PID, which must not be SIGTERMed.)
	require.NoError(t, os.Remove(markerPath))
	require.NoError(t, m.unmount(context.Background(), mountPath, false),
		"stub unmount of an already-gone mount must succeed")
}

// TestNewMounter_NoStubEnvKeepsRealHarness pins the inverse: without the env
// var, stub mode stays off (the real isMountpoint/unmountPath are installed).
func TestNewMounter_NoStubEnvKeepsRealHarness(t *testing.T) {
	t.Setenv("HUBFUSE_STUB_MOUNT_DIR", "")
	dir := t.TempDir()
	m := NewMounter(filepath.Join(dir, "key"), dir, filepath.Join(dir, "known_hosts"), "", discardLogger())
	assert.False(t, m.stub, "empty HUBFUSE_STUB_MOUNT_DIR must not enable stub mode")
}

