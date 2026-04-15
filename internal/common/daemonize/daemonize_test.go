package daemonize

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestIsChild_EnvUnset(t *testing.T) {
	t.Setenv(EnvDaemonized, "")
	if IsChild() {
		t.Fatal("IsChild() = true with env unset; want false")
	}
}

func TestIsChild_EnvSet(t *testing.T) {
	t.Setenv(EnvDaemonized, "1")
	if !IsChild() {
		t.Fatal("IsChild() = false with env set; want true")
	}
}

func TestIsChild_EnvZero(t *testing.T) {
	t.Setenv(EnvDaemonized, "0")
	if IsChild() {
		t.Fatal("IsChild() = true with env=0; want false (only \"1\" means child)")
	}
}

func TestWritePIDFile_Basic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foo.pid")

	if err := WritePIDFile(path); err != nil {
		t.Fatalf("WritePIDFile: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pidfile: %v", err)
	}
	got, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parse pidfile contents %q: %v", data, err)
	}
	if got != os.Getpid() {
		t.Fatalf("pidfile contents = %d; want %d", got, os.Getpid())
	}
}

func TestWritePIDFile_AtomicOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "foo.pid")

	if err := os.WriteFile(path, []byte("99999999\nleftover-junk\n"), 0o644); err != nil {
		t.Fatalf("seed pidfile: %v", err)
	}

	if err := WritePIDFile(path); err != nil {
		t.Fatalf("WritePIDFile: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read pidfile: %v", err)
	}
	if strings.Contains(string(data), "leftover-junk") {
		t.Fatalf("pidfile still has old contents: %q", data)
	}
}

func TestCheckRunning_NoFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nope.pid")

	pid, alive, err := CheckRunning(path)
	if err != nil {
		t.Fatalf("CheckRunning: %v", err)
	}
	if alive {
		t.Fatal("alive=true with no pidfile; want false")
	}
	if pid != 0 {
		t.Fatalf("pid=%d; want 0", pid)
	}
}

func TestCheckRunning_LivePID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "live.pid")
	if err := WritePIDFile(path); err != nil {
		t.Fatalf("seed pidfile: %v", err)
	}

	pid, alive, err := CheckRunning(path)
	if err != nil {
		t.Fatalf("CheckRunning: %v", err)
	}
	if !alive {
		t.Fatal("alive=false for own pid; want true")
	}
	if pid != os.Getpid() {
		t.Fatalf("pid=%d; want %d", pid, os.Getpid())
	}
}

func TestCheckRunning_StalePID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stale.pid")

	// Find a PID that's definitely dead: spawn a short-lived child, wait
	// for it to exit, then write its PID.
	cmd := newDeadPIDCmd(t)
	stalePID := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		// exit error is expected; we just need the process to be gone.
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(stalePID)+"\n"), 0o644); err != nil {
		t.Fatalf("seed stale pidfile: %v", err)
	}

	pid, alive, err := CheckRunning(path)
	if err != nil {
		t.Fatalf("CheckRunning: %v", err)
	}
	if alive {
		t.Fatalf("alive=true for dead pid %d; want false", stalePID)
	}
	if pid != 0 {
		t.Fatalf("pid=%d; want 0 (stale)", pid)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stale pidfile still exists: %v", err)
	}
}

// newDeadPIDCmd starts /bin/sh that exits immediately. Caller must Wait()
// to reap it; the returned cmd's Process.Pid is then safe to use as a
// pid that no longer refers to a live process (in practice the kernel
// reuses PIDs only after a long cycle).
func newDeadPIDCmd(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	return cmd
}
