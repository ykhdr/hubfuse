//go:build unix

package daemonize

import (
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMain is the self-reexec trampoline. When the test binary is run
// by Spawn with HUBFUSE_TEST_ROLE set, we switch into one of three
// pretend-service behaviours and exit. Otherwise m.Run() runs tests as
// usual.
func TestMain(m *testing.M) {
	switch os.Getenv("HUBFUSE_TEST_ROLE") {
	case "ready":
		// Optionally record the argv this child was launched with so a
		// test can assert SpawnOpts.ChildArgs was honored. Written BEFORE
		// the PID file so it is guaranteed present once Spawn returns
		// (Spawn returns as soon as the PID file appears).
		if argsFile := os.Getenv("HUBFUSE_TEST_ARGSFILE"); argsFile != "" {
			if err := os.WriteFile(argsFile, []byte(strings.Join(os.Args[1:], "\n")), 0o644); err != nil {
				_, _ = os.Stderr.WriteString("child: write argsfile failed: " + err.Error() + "\n")
				os.Exit(4)
			}
		}
		// Write the PID file at the path passed in HUBFUSE_TEST_PIDFILE,
		// then block on SIGTERM/SIGINT.
		if err := WritePIDFile(os.Getenv("HUBFUSE_TEST_PIDFILE")); err != nil {
			_, _ = os.Stderr.WriteString("child: WritePIDFile failed: " + err.Error() + "\n")
			os.Exit(3)
		}
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		<-sigCh
		os.Exit(0)
	case "die":
		_, _ = os.Stderr.WriteString("boom\n")
		os.Exit(2)
	case "slow":
		// Never write PID file; just sleep. Parent must time out.
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "retrying":
		// The shape the agent now has: it logs why it cannot come up and keeps
		// trying instead of exiting, so it is still alive when the parent's
		// readiness deadline passes. The parent kills it, and the log is the
		// only account of what went wrong. (#74)
		_, _ = os.Stderr.WriteString("cannot reach hub at 192.168.0.9:9090, retrying\n")
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "ignore":
		// Writes pidfile, then ignores SIGTERM/SIGINT and only exits on
		// SIGKILL (or after a long fallback so a runaway test cleans up).
		if err := WritePIDFile(os.Getenv("HUBFUSE_TEST_PIDFILE")); err != nil {
			os.Exit(3)
		}
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		// Drain incoming signals so they don't terminate the process,
		// but stay alive on a deadline as a safety net for a runaway test.
		go func() {
			for range sigCh {
			}
		}()
		time.Sleep(60 * time.Second)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestSpawn_Success(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "child.log")
	pidPath := filepath.Join(dir, "child.pid")

	t.Setenv("HUBFUSE_TEST_ROLE", "ready")
	t.Setenv("HUBFUSE_TEST_PIDFILE", pidPath)

	done := captureStdout(t, func() {
		require.NoError(t, Spawn(SpawnOpts{
			LogPath:      logPath,
			PIDFilePath:  pidPath,
			ReadyTimeout: 3 * time.Second,
		}))
	})

	out := <-done
	assert.Contains(t, out, "started (pid ", "Spawn stdout expected started line")

	pid, alive, err := CheckRunning(pidPath)
	require.NoError(t, err)
	require.True(t, alive, "child not alive after Spawn returned")

	proc, err := os.FindProcess(pid)
	require.NoError(t, err)
	require.NoError(t, proc.Signal(syscall.SIGTERM), "SIGTERM")
	waitForDeath(t, pid, 5*time.Second)
}

func TestSpawn_ChildArgsOverride(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "child.log")
	pidPath := filepath.Join(dir, "child.pid")
	argsPath := filepath.Join(dir, "child.args")

	t.Setenv("HUBFUSE_TEST_ROLE", "ready")
	t.Setenv("HUBFUSE_TEST_PIDFILE", pidPath)
	t.Setenv("HUBFUSE_TEST_ARGSFILE", argsPath)

	childArgs := []string{"start", "--marker"}

	done := captureStdout(t, func() {
		require.NoError(t, Spawn(SpawnOpts{
			LogPath:      logPath,
			PIDFilePath:  pidPath,
			ReadyTimeout: 3 * time.Second,
			ChildArgs:    childArgs,
		}))
	})
	<-done

	// The child must have been launched with exactly ChildArgs, not the
	// test binary's own os.Args — that is what stops `restart` recursing.
	got, err := os.ReadFile(argsPath)
	require.NoError(t, err, "argsfile should exist once Spawn returned")
	assert.Equal(t, strings.Join(childArgs, "\n"), string(got), "child argv must equal SpawnOpts.ChildArgs")

	pid, alive, err := CheckRunning(pidPath)
	require.NoError(t, err)
	require.True(t, alive, "child not alive after Spawn returned")

	proc, err := os.FindProcess(pid)
	require.NoError(t, err)
	require.NoError(t, proc.Signal(syscall.SIGTERM), "SIGTERM")
	waitForDeath(t, pid, 5*time.Second)
}

func TestSpawn_ChildDiesEarly(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "child.log")
	pidPath := filepath.Join(dir, "child.pid")

	t.Setenv("HUBFUSE_TEST_ROLE", "die")

	err := Spawn(SpawnOpts{
		LogPath:      logPath,
		PIDFilePath:  pidPath,
		ReadyTimeout: 3 * time.Second,
	})
	require.Error(t, err, "Spawn: got nil error; want child-exited error")
	assert.Contains(t, err.Error(), "exited", `want substring "exited"`)
	assert.Contains(t, err.Error(), "boom", `want substring "boom" from child stderr`)
}

func TestSpawn_Timeout(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "child.log")
	pidPath := filepath.Join(dir, "child.pid")

	t.Setenv("HUBFUSE_TEST_ROLE", "slow")

	err := Spawn(SpawnOpts{
		LogPath:      logPath,
		PIDFilePath:  pidPath,
		ReadyTimeout: 300 * time.Millisecond,
	})
	require.Error(t, err, "Spawn: got nil error; want timeout error")
	assert.Contains(t, err.Error(), "did not become ready", `want substring "did not become ready"`)
}

// TestSpawn_TimeoutCarriesTheLogTail pins the half of the readiness failure that
// used to be silent.
//
// Both failure branches kill the same expectation — the daemon never came up —
// but only one of them used to say why. A child that EXITS is reported with 20
// lines of its log; a child that is merely still trying was reported as "did not
// become ready within 5s" and nothing else, and then killed, so its reason died
// with it.
//
// That branch was harmless while the agent exited on a failed first
// registration, because then it was the exit branch that ran. Now the agent
// retries instead (#74), so an unreachable hub reaches the operator through
// THIS branch and only this one. Without the tail, `hubfuse start -d` against a
// down hub would report a timeout and hide the "cannot reach hub" line the
// daemon had already written.
func TestSpawn_TimeoutCarriesTheLogTail(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "child.log")
	pidPath := filepath.Join(dir, "child.pid")

	t.Setenv("HUBFUSE_TEST_ROLE", "retrying")

	err := Spawn(SpawnOpts{
		LogPath:      logPath,
		PIDFilePath:  pidPath,
		ReadyTimeout: 500 * time.Millisecond,
	})
	require.Error(t, err, "Spawn: got nil error; want timeout error")
	assert.Contains(t, err.Error(), "did not become ready",
		"the timeout itself must still be reported")
	assert.Contains(t, err.Error(), "cannot reach hub",
		"the daemon's own account of the failure must survive being killed — "+
			"without it the operator is told only that nothing happened")
}

func TestSpawn_RemovesDaemonFlag(t *testing.T) {
	cases := [][]string{
		{"--daemon"},
		{"-d"},
		{"--daemon=true"},
		{"--daemon=false"},
		{"start", "--daemon", "--other"},
		{"start", "-d", "--other"},
		{"start", "--daemon=true", "--other"},
	}
	for _, argv := range cases {
		got := stripDaemonFlag(argv)
		for _, a := range got {
			isDaemonFlag := a == "--daemon" || a == "-d" || strings.HasPrefix(a, "--daemon=")
			assert.False(t, isDaemonFlag, "stripDaemonFlag(%v) left %q in %v", argv, a, got)
		}
	}
}

// captureStdout replaces os.Stdout for the duration of fn, captures
// everything written, and returns the captured string via the channel.
func captureStdout(t *testing.T, fn func()) <-chan string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err, "pipe")
	orig := os.Stdout
	os.Stdout = w

	out := make(chan string, 1)
	go func() {
		defer close(out)
		buf := make([]byte, 4096)
		var sb strings.Builder
		for {
			n, err := r.Read(buf)
			if n > 0 {
				sb.Write(buf[:n])
			}
			if err != nil {
				break
			}
		}
		out <- sb.String()
	}()

	fn()

	os.Stdout = orig
	_ = w.Close()
	return out
}

// waitForDeath polls until the PID is no longer signalable or the
// deadline is reached.
func waitForDeath(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		proc, err := os.FindProcess(pid)
		if err != nil {
			return
		}
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("process %d still alive after %s", pid, timeout)
}
