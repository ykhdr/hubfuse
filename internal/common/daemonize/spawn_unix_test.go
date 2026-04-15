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
)

// TestMain is the self-reexec trampoline. When the test binary is run
// by Spawn with HUBFUSE_TEST_ROLE set, we switch into one of three
// pretend-service behaviours and exit. Otherwise m.Run() runs tests as
// usual.
func TestMain(m *testing.M) {
	switch os.Getenv("HUBFUSE_TEST_ROLE") {
	case "ready":
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
		if err := Spawn(SpawnOpts{
			LogPath:      logPath,
			PIDFilePath:  pidPath,
			ReadyTimeout: 3 * time.Second,
		}); err != nil {
			t.Fatalf("Spawn: %v", err)
		}
	})

	out := <-done
	if !strings.Contains(out, "started (pid ") {
		t.Fatalf("Spawn stdout = %q; expected started line", out)
	}

	pid, alive, err := CheckRunning(pidPath)
	if err != nil {
		t.Fatalf("CheckRunning: %v", err)
	}
	if !alive {
		t.Fatal("child not alive after Spawn returned")
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		t.Fatalf("FindProcess: %v", err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
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
	if err == nil {
		t.Fatal("Spawn: got nil error; want child-exited error")
	}
	if !strings.Contains(err.Error(), "exited") {
		t.Fatalf("Spawn err = %q; want substring \"exited\"", err)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Fatalf("Spawn err = %q; want substring \"boom\" from child stderr", err)
	}
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
	if err == nil {
		t.Fatal("Spawn: got nil error; want timeout error")
	}
	if !strings.Contains(err.Error(), "did not become ready") {
		t.Fatalf("Spawn err = %q; want substring \"did not become ready\"", err)
	}
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
			if a == "--daemon" || a == "-d" ||
				strings.HasPrefix(a, "--daemon=") {
				t.Fatalf("stripDaemonFlag(%v) left %q in %v", argv, a, got)
			}
		}
	}
}

// captureStdout replaces os.Stdout for the duration of fn, captures
// everything written, and returns the captured string via the channel.
func captureStdout(t *testing.T, fn func()) <-chan string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
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
