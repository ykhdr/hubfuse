//go:build unix

package daemonize

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A tiny helper child that waits for SIGTERM and exits 0. The test
// binary's TestMain already branches on HUBFUSE_TEST_ROLE=ready (see
// spawn_unix_test.go) — reuse that infrastructure.

// startHelper starts the test binary as a child with the given role and
// pidfile path. It returns the started *exec.Cmd.
//
// The child is started with Setsid=true so that it becomes its own session
// leader. A background goroutine calls cmd.Wait() so the test binary reaps
// the zombie promptly, allowing syscall.Kill(pid,0) to return ESRCH as soon
// as the child exits — which is the same behavior SignalStop sees in
// production (where it is never the parent of the daemon).
func startHelper(t *testing.T, role, pidPath string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(),
		"HUBFUSE_TEST_ROLE="+role,
		"HUBFUSE_TEST_PIDFILE="+pidPath,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	require.NoError(t, cmd.Start(), "start helper")
	// Reap the child when it exits so kill(0) returns ESRCH immediately.
	go func() { _ = cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	return cmd
}

// waitLive polls until the pidfile is live or the deadline expires.
func waitLive(t *testing.T, pidPath string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, alive, _ := CheckRunning(pidPath); alive {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("child did not start within %s", timeout)
}

func TestSignalStop_SendsSIGTERM(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "x.pid")

	startHelper(t, "ready", pidPath)
	waitLive(t, pidPath, 2*time.Second)

	require.NoError(t, SignalStop(pidPath, "testproc"))

	// SignalStop must wait for the process to be gone before returning.
	_, alive, err := CheckRunning(pidPath)
	require.NoError(t, err)
	assert.False(t, alive, "process still alive after SignalStop returned")
}

func TestSignalStop_NoPIDFile(t *testing.T) {
	err := SignalStop(filepath.Join(t.TempDir(), "nope.pid"), "x")
	assert.Error(t, err, "SignalStop with missing pidfile should error")
}

// TestSignalStop_EscalatesToSIGKILL verifies that a process ignoring SIGTERM
// is escalated to SIGKILL and that SignalStop returns nil once it is gone.
func TestSignalStop_EscalatesToSIGKILL(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "ignore.pid")

	startHelper(t, "ignore", pidPath)
	waitLive(t, pidPath, 3*time.Second)

	start := time.Now()
	maxWait := stopGracefulTimeout + stopKillTimeout + 2*time.Second
	done := make(chan error, 1)
	go func() { done <- SignalStop(pidPath, "ignore-proc") }()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(maxWait):
		t.Fatalf("SignalStop did not return within %s", maxWait)
	}

	elapsed := time.Since(start)
	// SIGKILL was needed, so we must have waited at least the graceful window.
	assert.GreaterOrEqual(t, elapsed, 9*time.Second,
		"expected elapsed >= 9s (graceful timeout), got %s", elapsed)

	_, alive, err := CheckRunning(pidPath)
	require.NoError(t, err)
	assert.False(t, alive, "process still alive after SignalStop returned")
}

// TestSignalStop_ProcessAlreadyDead verifies that SignalStop returns nil when
// the pidfile points at a PID that has already exited.
func TestSignalStop_ProcessAlreadyDead(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "dead.pid")

	// Spawn a process and wait for it to exit.
	cmd := exec.Command("true")
	require.NoError(t, cmd.Start())
	require.NoError(t, cmd.Wait())
	deadPID := cmd.Process.Pid

	require.NoError(t, os.WriteFile(pidPath, []byte(strconv.Itoa(deadPID)+"\n"), 0o644))

	err := SignalStop(pidPath, "dead-proc")
	assert.NoError(t, err, "SignalStop should return nil for already-dead process")
}

// TestSignalStop_RejectsNonPositivePID verifies SignalStop refuses to act on
// a pidfile containing 0 or a negative value. On Unix, signalling pid 0 hits
// the entire process group and pid <0 the whole reachable system — a corrupt
// pidfile must never escalate into either.
func TestSignalStop_RejectsNonPositivePID(t *testing.T) {
	for _, raw := range []string{"0", "-1"} {
		t.Run("pid_"+raw, func(t *testing.T) {
			dir := t.TempDir()
			pidPath := filepath.Join(dir, "bad.pid")
			require.NoError(t, os.WriteFile(pidPath, []byte(raw+"\n"), 0o644))

			err := SignalStop(pidPath, "bad-pid")
			require.Error(t, err)
			assert.Contains(t, err.Error(), "non-positive pid",
				"error should mention non-positive pid; got: %v", err)
		})
	}
}

func TestReportStatus_Running(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "live.pid")
	require.NoError(t, os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644), "seed pidfile")
	assert.NoError(t, ReportStatus(pidPath, "self"))
}

func TestReportStatus_NotRunning(t *testing.T) {
	assert.NoError(t, ReportStatus(filepath.Join(t.TempDir(), "nope.pid"), "absent"))
}

// TestReadPID_TrimsNewline: WritePIDFile writes "pid\n"; ReadPID
// must round-trip that correctly.
func TestReadPID_TrimsNewline(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "x.pid")
	require.NoError(t, os.WriteFile(p, []byte("12345\n"), 0o644), "seed")
	pid, err := ReadPID(p)
	require.NoError(t, err)
	assert.Equal(t, 12345, pid)
}
