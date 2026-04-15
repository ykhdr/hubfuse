# Daemon Mode for `hubfuse-agent` and `hubfuse-hub` — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a `--daemon` / `-d` flag to `hubfuse-agent start` and `hubfuse-hub start` that detaches the process from the terminal, redirects logs to a file, and returns immediately, while preserving the existing foreground default.

**Architecture:** Introduce a new reusable package `internal/common/daemonize` that handles self-respawn via `exec` + `Setsid`, PID-file lifecycle, and log-output resolution. Wire an `OnReady func()` callback into `agent.Daemon` and `hub.Hub` so the PID file is written exactly when the service is accepting work — the parent uses the PID file's appearance as its readiness signal.

**Tech Stack:** Go 1.25, `log/slog`, `spf13/cobra`. Unix-only daemon path via `syscall.SysProcAttr.Setsid`; Windows gets a stub that returns a clear error.

**Spec:** [`docs/superpowers/specs/2026-04-15-agent-hub-daemon-mode-design.md`](../specs/2026-04-15-agent-hub-daemon-mode-design.md). Issue: [#2](https://github.com/ykhdr/hubfuse/issues/2).

---

## File map

**New:**
- `internal/common/daemonize/daemonize.go` — package-level helpers (`IsChild`, `CheckRunning`, `WritePIDFile`, `ResolveLogOutput`).
- `internal/common/daemonize/spawn_unix.go` — `Spawn` implementation, build-tagged `//go:build unix`.
- `internal/common/daemonize/spawn_windows.go` — stub returning `errors.New("--daemon is not supported on windows")`, build-tagged `//go:build windows`.
- `internal/common/daemonize/daemonize_test.go` — unit tests for helpers (OS-portable).
- `internal/common/daemonize/spawn_unix_test.go` — integration test for `Spawn`, build-tagged `//go:build unix`.

**Modified:**
- `internal/hub/hub.go` — add `OnReady func()` field to `Hub`; remove inline PID-file write; call `OnReady` after listener bound.
- `internal/agent/daemon.go` — add `OnReady func()` field to `Daemon`; call it once after Register.
- `cmd/hubfuse-hub/main.go` — add `--daemon` flag; use `daemonize.Spawn` / `CheckRunning` / `WritePIDFile` / `ResolveLogOutput`.
- `cmd/hubfuse-agent/main.go` — add `--daemon`, `--log-output`, `--log-level` flags; use the same helpers; move PID-file write out of `RunE` into an `OnReady` callback.

---

## Task 1: Create `daemonize` package with `IsChild`

**Files:**
- Create: `internal/common/daemonize/daemonize.go`
- Test: `internal/common/daemonize/daemonize_test.go`

- [ ] **Step 1.1: Write failing tests for `IsChild`**

Create `internal/common/daemonize/daemonize_test.go`:

```go
package daemonize

import (
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
```

- [ ] **Step 1.2: Run tests to confirm they fail**

```
go test ./internal/common/daemonize/ -run TestIsChild -v
```
Expected: build fails — package does not exist.

- [ ] **Step 1.3: Create the package with `IsChild`**

Create `internal/common/daemonize/daemonize.go`:

```go
// Package daemonize provides primitives for turning a foreground process
// into a detached Unix daemon: self-respawn with Setsid, PID-file
// lifecycle, and log-output redirection. Unix-only for the Spawn path;
// helpers here are OS-portable.
package daemonize

import "os"

// EnvDaemonized is the environment variable the parent sets on the
// detached child before re-execing. A non-empty value "1" marks the
// process as the child so it does not try to re-daemonize.
const EnvDaemonized = "HUBFUSE_DAEMONIZED"

// IsChild reports whether the current process is the detached child
// spawned by Spawn. Only the exact value "1" is treated as truthy.
func IsChild() bool {
	return os.Getenv(EnvDaemonized) == "1"
}
```

- [ ] **Step 1.4: Run tests to confirm they pass**

```
go test ./internal/common/daemonize/ -run TestIsChild -v
```
Expected: PASS.

- [ ] **Step 1.5: Run go vet**

```
go vet ./internal/common/daemonize/
```
Expected: no output.

- [ ] **Step 1.6: Commit**

```bash
git add internal/common/daemonize/daemonize.go internal/common/daemonize/daemonize_test.go
git commit -m "Add daemonize package with IsChild helper"
```

---

## Task 2: Add `WritePIDFile`

**Files:**
- Modify: `internal/common/daemonize/daemonize.go`
- Modify: `internal/common/daemonize/daemonize_test.go`

- [ ] **Step 2.1: Write failing tests for `WritePIDFile`**

Append to `internal/common/daemonize/daemonize_test.go`:

```go
import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

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
```

Merge the new `import` block with the existing one at the top (single consolidated import; `"testing"` was already there).

- [ ] **Step 2.2: Run to confirm failure**

```
go test ./internal/common/daemonize/ -run TestWritePIDFile -v
```
Expected: FAIL — `WritePIDFile` undefined.

- [ ] **Step 2.3: Implement `WritePIDFile`**

Append to `internal/common/daemonize/daemonize.go`:

```go
import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// WritePIDFile atomically writes os.Getpid() to path. It writes to a
// sibling tmp file first and renames on success, so readers never see a
// half-written PID. The file mode is 0644 and parent directories must
// already exist.
func WritePIDFile(path string) error {
	pid := strconv.Itoa(os.Getpid())

	tmp, err := os.CreateTemp(filepath.Dir(path), ".pid-*")
	if err != nil {
		return fmt.Errorf("create tmp pidfile: %w", err)
	}
	tmpName := tmp.Name()

	cleanup := func() { _ = os.Remove(tmpName) }

	if _, err := tmp.WriteString(pid + "\n"); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write tmp pidfile: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("chmod tmp pidfile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close tmp pidfile: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("rename tmp pidfile to %q: %w", path, err)
	}
	return nil
}
```

Merge the two `import` blocks at the top of `daemonize.go` — there must be only one, containing all of `"fmt"`, `"os"`, `"path/filepath"`, `"strconv"`.

- [ ] **Step 2.4: Run to confirm pass**

```
go test ./internal/common/daemonize/ -run TestWritePIDFile -v
```
Expected: PASS.

- [ ] **Step 2.5: Commit**

```bash
git add internal/common/daemonize/daemonize.go internal/common/daemonize/daemonize_test.go
git commit -m "Add WritePIDFile with atomic tmp+rename"
```

---

## Task 3: Add `CheckRunning`

**Files:**
- Modify: `internal/common/daemonize/daemonize.go`
- Modify: `internal/common/daemonize/daemonize_test.go`

- [ ] **Step 3.1: Write failing tests**

Append to `internal/common/daemonize/daemonize_test.go`:

```go
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
```

Add `"os/exec"` to the test file's import block.

- [ ] **Step 3.2: Run to confirm failure**

```
go test ./internal/common/daemonize/ -run TestCheckRunning -v
```
Expected: FAIL — `CheckRunning` undefined.

- [ ] **Step 3.3: Implement `CheckRunning`**

Append to `internal/common/daemonize/daemonize.go`:

```go
import "syscall"

// CheckRunning inspects path and reports whether it contains a live PID.
//
//   - If path does not exist → returns (0, false, nil).
//   - If path exists and the PID is alive → (pid, true, nil).
//   - If path exists but the PID is gone (stale) → the file is removed
//     and (0, false, nil) is returned.
//   - I/O problems (permission errors, malformed contents) surface as
//     non-nil err with (0, false, err).
//
// Liveness is probed with os.FindProcess followed by proc.Signal(0),
// which is the POSIX idiom for "does this process exist and can I send
// it signals?". On Unix, FindProcess never fails, so the Signal call is
// authoritative.
func CheckRunning(path string) (int, bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read pidfile %q: %w", path, err)
	}

	trimmed := strings.TrimSpace(string(data))
	pid, err := strconv.Atoi(trimmed)
	if err != nil {
		return 0, false, fmt.Errorf("parse pidfile %q (contents %q): %w", path, trimmed, err)
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		// Not reachable on Unix (FindProcess always succeeds) but we
		// handle it defensively.
		_ = os.Remove(path)
		return 0, false, nil
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		// Process is gone (ESRCH) or we can't signal it (EPERM). In
		// either case treat the PID file as stale and remove it so the
		// next start can proceed.
		_ = os.Remove(path)
		return 0, false, nil
	}
	return pid, true, nil
}
```

Merge the `import` at the top: final import block is `"fmt"`, `"os"`, `"path/filepath"`, `"strconv"`, `"strings"`, `"syscall"`.

- [ ] **Step 3.4: Run to confirm pass**

```
go test ./internal/common/daemonize/ -run TestCheckRunning -v
```
Expected: PASS.

- [ ] **Step 3.5: Commit**

```bash
git add internal/common/daemonize/daemonize.go internal/common/daemonize/daemonize_test.go
git commit -m "Add CheckRunning with stale-pidfile cleanup"
```

---

## Task 4: Add `ResolveLogOutput`

**Files:**
- Modify: `internal/common/daemonize/daemonize.go`
- Modify: `internal/common/daemonize/daemonize_test.go`

- [ ] **Step 4.1: Write failing tests**

Append:

```go
func TestResolveLogOutput(t *testing.T) {
	cases := []struct {
		name        string
		userFlag    string
		daemon      bool
		defaultPath string
		want        string
	}{
		{"foreground default stderr", "stderr", false, "/tmp/x.log", "stderr"},
		{"daemon default upgrades to path", "stderr", true, "/tmp/x.log", "/tmp/x.log"},
		{"user override wins foreground", "/custom/a.log", false, "/tmp/x.log", "/custom/a.log"},
		{"user override wins daemon", "/custom/a.log", true, "/tmp/x.log", "/custom/a.log"},
		{"empty userFlag treated as stderr foreground", "", false, "/tmp/x.log", "stderr"},
		{"empty userFlag treated as stderr daemon", "", true, "/tmp/x.log", "/tmp/x.log"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ResolveLogOutput(tc.userFlag, tc.daemon, tc.defaultPath)
			if got != tc.want {
				t.Fatalf("ResolveLogOutput(%q, %v, %q) = %q; want %q",
					tc.userFlag, tc.daemon, tc.defaultPath, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 4.2: Run to confirm failure**

```
go test ./internal/common/daemonize/ -run TestResolveLogOutput -v
```
Expected: FAIL — undefined.

- [ ] **Step 4.3: Implement**

Append to `daemonize.go`:

```go
// ResolveLogOutput picks the effective log destination.
//
// If userFlag is an explicit file path, it wins unconditionally. If
// userFlag is empty or the literal "stderr", the result is "stderr" in
// foreground mode and defaultPath in daemon mode — because a detached
// process has no controlling terminal to write to.
func ResolveLogOutput(userFlag string, daemon bool, defaultPath string) string {
	if userFlag != "" && userFlag != "stderr" {
		return userFlag
	}
	if daemon {
		return defaultPath
	}
	return "stderr"
}
```

- [ ] **Step 4.4: Run to confirm pass**

```
go test ./internal/common/daemonize/ -v
```
Expected: all tests PASS.

- [ ] **Step 4.5: Commit**

```bash
git add internal/common/daemonize/daemonize.go internal/common/daemonize/daemonize_test.go
git commit -m "Add ResolveLogOutput helper"
```

---

## Task 5: Windows stub for `Spawn`

**Files:**
- Create: `internal/common/daemonize/spawn_windows.go`

- [ ] **Step 5.1: Create the stub**

Create `internal/common/daemonize/spawn_windows.go`:

```go
//go:build windows

package daemonize

import (
	"errors"
	"time"
)

// SpawnOpts configures Spawn. See the unix implementation for field
// semantics. The windows stub ignores the struct and always errors.
type SpawnOpts struct {
	LogPath      string
	PIDFilePath  string
	ReadyTimeout time.Duration
}

// Spawn returns an error on Windows: the daemon path relies on POSIX
// Setsid semantics that have no Windows equivalent worth shipping.
// Users on Windows can run the binary under a Windows service manager
// or skip --daemon entirely.
func Spawn(SpawnOpts) error {
	return errors.New("--daemon is not supported on windows")
}
```

- [ ] **Step 5.2: Verify cross-compile**

```
GOOS=windows go build ./internal/common/daemonize/
```
Expected: no output (builds cleanly).

- [ ] **Step 5.3: Commit**

```bash
git add internal/common/daemonize/spawn_windows.go
git commit -m "Add Windows stub for daemonize.Spawn"
```

---

## Task 6: Unix `Spawn` — tests first

**Files:**
- Create: `internal/common/daemonize/spawn_unix_test.go`

This task sets up the self-reexec test infrastructure and writes the tests. The implementation comes in Task 7; this task's tests will fail to compile until then.

- [ ] **Step 6.1: Create the test file**

Create `internal/common/daemonize/spawn_unix_test.go`:

```go
//go:build unix

package daemonize

import (
	"os"
	"os/exec"
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
			os.Stderr.WriteString("child: WritePIDFile failed: " + err.Error() + "\n")
			os.Exit(3)
		}
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)
		<-sigCh
		os.Exit(0)
	case "die":
		os.Stderr.WriteString("boom\n")
		os.Exit(2)
	case "slow":
		// Never write PID file; just sleep. Parent must time out.
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	os.Exit(m.Run())
}
```

- [ ] **Step 6.2: Append the Spawn tests to the same file**

Append these definitions after `TestMain` in `internal/common/daemonize/spawn_unix_test.go`:

```go
func TestSpawn_Success(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "child.log")
	pidPath := filepath.Join(dir, "child.pid")

	t.Setenv("HUBFUSE_TEST_ROLE", "ready")
	t.Setenv("HUBFUSE_TEST_PIDFILE", pidPath)

	// Capture the child's "started" line by redirecting parent stdout.
	// Spawn prints to os.Stdout; we swap it for a pipe and read.
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

	// The child must be alive.
	pid, alive, err := CheckRunning(pidPath)
	if err != nil {
		t.Fatalf("CheckRunning: %v", err)
	}
	if !alive {
		t.Fatal("child not alive after Spawn returned")
	}

	// Clean up: SIGTERM the child and wait for pidfile to vanish or the
	// process to die.
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

// keep the linter happy for imports we reference below once Spawn is
// implemented; these helpers are used there.
var _ = exec.Command
```

- [ ] **Step 6.3: Run to confirm compile-failure**

```
go test ./internal/common/daemonize/ -run TestSpawn -v
```
Expected: FAIL to compile — `Spawn`, `SpawnOpts`, `stripDaemonFlag` undefined.

- [ ] **Step 6.4: Commit the tests (still failing to compile)**

We deliberately commit the red tests so the subsequent implementation commit is small and its failure-then-green story is visible in git history.

```bash
git add internal/common/daemonize/spawn_unix_test.go
git commit -m "Add Spawn test harness (red; impl in next commit)"
```

---

## Task 7: Implement Unix `Spawn`

**Files:**
- Create: `internal/common/daemonize/spawn_unix.go`

- [ ] **Step 7.1: Write the implementation**

Create `internal/common/daemonize/spawn_unix.go`:

```go
//go:build unix

package daemonize

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// SpawnOpts configures Spawn.
type SpawnOpts struct {
	// LogPath is the file the detached child's stdout and stderr are
	// appended to. Resolved to an absolute path before fork.
	LogPath string

	// PIDFilePath is the file whose appearance signals that the child
	// is ready to serve. Spawn polls for this file.
	PIDFilePath string

	// ReadyTimeout bounds how long Spawn waits for the PID file to
	// appear before giving up and killing the child. Defaults to 5s.
	ReadyTimeout time.Duration
}

// Spawn re-execs the current binary as a detached child and waits for
// it to either (a) write the PID file while still alive — success — or
// (b) exit — failure — or (c) exceed ReadyTimeout — failure.
//
// The child is started with HUBFUSE_DAEMONIZED=1 in its environment and
// Setsid=true on its SysProcAttr, so it becomes its own session leader
// and is detached from the caller's controlling terminal. Its stdin is
// wired to /dev/null; its stdout and stderr are appended to LogPath.
//
// Must only be called when IsChild() is false.
func Spawn(opts SpawnOpts) error {
	if IsChild() {
		return errors.New("daemonize.Spawn called from child process (refusing to recurse)")
	}
	if opts.ReadyTimeout <= 0 {
		opts.ReadyTimeout = 5 * time.Second
	}

	absLogPath, err := filepath.Abs(opts.LogPath)
	if err != nil {
		return fmt.Errorf("resolve log path %q: %w", opts.LogPath, err)
	}
	absPIDPath, err := filepath.Abs(opts.PIDFilePath)
	if err != nil {
		return fmt.Errorf("resolve pid path %q: %w", opts.PIDFilePath, err)
	}

	if err := os.MkdirAll(filepath.Dir(absLogPath), 0o755); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}
	logFile, err := os.OpenFile(absLogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open log %q: %w", absLogPath, err)
	}
	defer logFile.Close()

	devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("open /dev/null: %w", err)
	}
	defer devNull.Close()

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}

	childArgs := stripDaemonFlag(os.Args[1:])

	cmd := exec.Command(exe, childArgs...)
	cmd.Env = append(os.Environ(), EnvDaemonized+"=1")
	cmd.Stdin = devNull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}

	// Release the child: we don't want to reap it. Parent exits soon.
	// Keeping cmd.Wait available means we still notice early exit
	// during the readiness poll below; we do NOT Wait afterwards.
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	deadline := time.Now().Add(opts.ReadyTimeout)
	for {
		select {
		case werr := <-waitErr:
			tail := tailFile(absLogPath, 20)
			if werr != nil {
				return fmt.Errorf("daemon exited during startup: %v\n%s", werr, tail)
			}
			return fmt.Errorf("daemon exited during startup\n%s", tail)
		default:
		}

		pid, alive, err := CheckRunning(absPIDPath)
		if err != nil {
			return fmt.Errorf("check pidfile: %w", err)
		}
		if alive && pid == cmd.Process.Pid {
			fmt.Fprintf(os.Stdout, "started (pid %d, logs: %s)\n", pid, absLogPath)
			return nil
		}

		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			// Drain Wait so we don't leak the goroutine.
			<-waitErr
			return fmt.Errorf("daemon did not become ready within %s (check %s)", opts.ReadyTimeout, absLogPath)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// stripDaemonFlag removes every form of the --daemon / -d flag from
// argv so the re-exec'd child never recurses even if the IsChild guard
// were removed. Handles:
//   --daemon, --daemon=true, --daemon=false, -d
// Does NOT handle the space-separated form "--daemon true" because
// --daemon is a bool flag in cobra; that form is never emitted and
// users cannot produce it via normal CLI usage.
func stripDaemonFlag(argv []string) []string {
	out := argv[:0:0]
	for _, a := range argv {
		if a == "--daemon" || a == "-d" {
			continue
		}
		if strings.HasPrefix(a, "--daemon=") {
			continue
		}
		out = append(out, a)
	}
	return out
}

// tailFile returns the last n lines of path, or a short diagnostic
// string if the file cannot be read.
func tailFile(path string, n int) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("(log unavailable: %v)", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return "--- last " + fmt.Sprint(n) + " log lines ---\n" + strings.Join(lines, "\n")
}

// io is referenced only for clarity in doc comments; silence the
// linter if unused.
var _ = io.Discard
```

If `io.Discard` unused-reference feels awkward to reviewers, delete the last two lines and the `"io"` import together.

- [ ] **Step 7.2: Run tests to confirm pass**

```
go test ./internal/common/daemonize/ -v -count=1
```
Expected: PASS (all tests, including TestSpawn_*).

If `TestSpawn_Timeout` fails intermittently on very slow CI, raise its `ReadyTimeout` from 300ms to 1s and re-run.

- [ ] **Step 7.3: Run go vet**

```
go vet ./internal/common/daemonize/
```
Expected: no output.

- [ ] **Step 7.4: Commit**

```bash
git add internal/common/daemonize/spawn_unix.go
git commit -m "Implement daemonize.Spawn with Setsid + readiness polling"
```

---

## Task 8: Add `OnReady` hook to `hub.Hub`

**Files:**
- Modify: `internal/hub/hub.go`

- [ ] **Step 8.1: Add the `OnReady` field and remove the inline PID write**

Open `internal/hub/hub.go`.

Change the `Hub` struct (adds a single field):

```go
type Hub struct {
	config     HubConfig
	store      store.Store
	registry   *Registry
	heartbeat  *HeartbeatMonitor
	grpcServer *grpc.Server
	logger     *slog.Logger

	// OnReady, if non-nil, is invoked exactly once from Start right
	// after net.Listen returns. The hub is serving at that point. The
	// cmd layer uses this hook to write the PID file.
	OnReady func()
}
```

In `Start`, replace the block

```go
	pidFile := filepath.Join(dataDir, "hubfuse-hub.pid")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0644); err != nil {
		h.logger.Warn("failed to write PID file", slog.String("path", pidFile), slog.Any("error", err))
	}
```

with

```go
	if h.OnReady != nil {
		h.OnReady()
	}
```

Remove imports that are now unused by `hub.go`: `"strconv"` and the `pidFile` local stays gone. Run `goimports` or let `go vet` / build flag the missing imports; remove any that no longer have a consumer.

**IMPORTANT:** the `os` import is still used elsewhere in the file (e.g., `os.MkdirAll`), so do not remove it. Only `"strconv"` should go if nothing else references it. Check by searching the file after the edit.

- [ ] **Step 8.2: Update `Stop` to not assume `os.Remove(pidFile)` lives here**

Currently `Stop` removes the PID file. That responsibility now belongs to the `cmd` layer, which owns the file. Find the block in `Stop` that looks like:

```go
	pidFile := filepath.Join(dataDir, "hubfuse-hub.pid")
	if err := os.Remove(pidFile); err != nil && !os.IsNotExist(err) {
		h.logger.Warn("remove PID file", slog.Any("error", err))
	}
```

Delete that block entirely. If `dataDir` is no longer referenced anywhere else in `Stop` after the deletion, remove its assignment too.

(If your copy of `Stop` differs, read the current file and adapt — the invariant is "Hub no longer writes or removes the PID file".)

- [ ] **Step 8.3: Build**

```
go build ./...
```
Expected: success. If a build error cites an unused import, delete the import.

- [ ] **Step 8.4: Run tests**

```
go test ./internal/hub/... -count=1
```
Expected: PASS. Existing hub tests do not assert PID-file behaviour, so they should continue to pass unchanged. If a test fails because it looked for a PID file after calling `Start`, update that test to set `h.OnReady = func() { daemonize.WritePIDFile(pidPath) }` before `Start`, with `pidPath` in the test's TempDir.

- [ ] **Step 8.5: Commit**

```bash
git add internal/hub/hub.go
git commit -m "Move hub PID-file write behind OnReady hook"
```

---

## Task 9: Add `OnReady` hook to `agent.Daemon`

**Files:**
- Modify: `internal/agent/daemon.go`

- [ ] **Step 9.1: Add field and call site**

Edit `internal/agent/daemon.go`.

Add the field to the `Daemon` struct (right after the existing fields, before `dataDir` or at the end):

```go
// OnReady, if non-nil, is invoked exactly once by Run, immediately
// after successful Register with the hub. At that point the agent
// can serve hub-driven RPCs. The cmd layer uses this hook to write
// the PID file.
OnReady func()
```

In `Run`, after step 4 (`Register with hub`) and before step 5 (`processInitialDevices`), add:

```go
	if d.OnReady != nil {
		d.OnReady()
	}
```

So the surrounding code reads:

```go
	// 4. Register with hub and get online devices.
	shares := configSharesToProto(d.config.Shares)
	regResp, err := d.hubClient.Register(ctx, shares, d.config.Agent.SSHPort)
	if err != nil {
		return fmt.Errorf("register with hub: %w", err)
	}
	d.logger.Info("registered with hub",
		"online_devices", len(regResp.DevicesOnline),
	)

	// 4a. Signal readiness to whoever launched us.
	if d.OnReady != nil {
		d.OnReady()
	}

	// 5. Process initial online devices.
	d.processInitialDevices(regResp.DevicesOnline)
```

- [ ] **Step 9.2: Build**

```
go build ./...
```
Expected: success.

- [ ] **Step 9.3: Run existing agent tests**

```
go test ./internal/agent/... -count=1
```
Expected: PASS unchanged.

- [ ] **Step 9.4: Commit**

```bash
git add internal/agent/daemon.go
git commit -m "Add OnReady hook to agent.Daemon"
```

---

## Task 10: Wire `--daemon` into `cmd/hubfuse-hub`

**Files:**
- Modify: `cmd/hubfuse-hub/main.go`

- [ ] **Step 10.1: Rewrite `startCmd` to use the daemonize helpers**

Open `cmd/hubfuse-hub/main.go`. Replace the existing `startCmd` function with:

```go
func startCmd() *cobra.Command {
	var (
		listen    string
		dataDir   string
		logLevel  string
		logOutput string
		daemon    bool
	)

	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the hub server",
		RunE: func(cmd *cobra.Command, args []string) error {
			expandedData := expandHome(dataDir)
			pidPath := filepath.Join(expandedData, "hubfuse-hub.pid")
			defaultLog := filepath.Join(expandedData, "hub.log")

			// Reject second concurrent start regardless of daemon flag.
			if pid, alive, err := daemonize.CheckRunning(pidPath); err != nil {
				return fmt.Errorf("check existing hub: %w", err)
			} else if alive {
				return fmt.Errorf("hub already running (pid %d)", pid)
			}

			// If we're the parent and --daemon was requested, re-exec.
			if daemon && !daemonize.IsChild() {
				if err := os.MkdirAll(expandedData, 0o700); err != nil {
					return fmt.Errorf("create data dir: %w", err)
				}
				return daemonize.Spawn(daemonize.SpawnOpts{
					LogPath:     daemonize.ResolveLogOutput(logOutput, true, defaultLog),
					PIDFilePath: pidPath,
				})
			}

			// Foreground path OR detached child past this point.
			effectiveLog := daemonize.ResolveLogOutput(logOutput, daemon || daemonize.IsChild(), defaultLog)

			cfg := hub.HubConfig{
				ListenAddr: listen,
				DataDir:    dataDir,
				LogLevel:   logLevel,
				LogOutput:  effectiveLog,
			}

			h, err := hub.NewHub(cfg)
			if err != nil {
				return fmt.Errorf("create hub: %w", err)
			}
			h.OnReady = func() {
				if err := daemonize.WritePIDFile(pidPath); err != nil {
					fmt.Fprintf(os.Stderr, "warning: write pid file: %v\n", err)
				}
			}
			defer func() {
				if err := os.Remove(pidPath); err != nil && !os.IsNotExist(err) {
					fmt.Fprintf(os.Stderr, "warning: remove pid file: %v\n", err)
				}
			}()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

			go func() {
				<-sigCh
				cancel()
				if err := h.Stop(); err != nil {
					fmt.Fprintf(os.Stderr, "hub stop: %v\n", err)
				}
			}()

			if err := h.Start(ctx); err != nil {
				return fmt.Errorf("hub start: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&listen, "listen", ":9090", "address to listen on")
	cmd.Flags().StringVar(&dataDir, "data-dir", "~/.hubfuse-hub", "data directory")
	cmd.Flags().StringVar(&logLevel, "log-level", "info", "log level (debug, info, warn, error)")
	cmd.Flags().StringVar(&logOutput, "log-output", "stderr", "log output (stderr or file path)")
	cmd.Flags().BoolVarP(&daemon, "daemon", "d", false, "detach from terminal and run in the background")

	return cmd
}
```

Add the new imports at the top of `cmd/hubfuse-hub/main.go` if not already present:

```go
	"github.com/ykhdr/hubfuse/internal/common/daemonize"
```

Ensure the import block already has `"os/signal"`, `"path/filepath"`, `"syscall"` (it does per the file we inspected).

- [ ] **Step 10.2: Build and vet**

```
go build ./cmd/hubfuse-hub/
go vet ./cmd/hubfuse-hub/
```
Expected: success.

- [ ] **Step 10.3: Commit**

```bash
git add cmd/hubfuse-hub/main.go
git commit -m "Wire --daemon flag into hubfuse-hub start"
```

---

## Task 11: Wire `--daemon` + log flags into `cmd/hubfuse-agent`

**Files:**
- Modify: `cmd/hubfuse-agent/main.go`

- [ ] **Step 11.1: Rewrite `startCmd`**

Open `cmd/hubfuse-agent/main.go`. Replace `startCmd` with:

```go
func startCmd() *cobra.Command {
	var (
		logLevel  string
		logOutput string
		daemon    bool
	)

	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the agent daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			dataDir := expandHome(defaultDataDir)
			cfgPath := filepath.Join(dataDir, configFile)
			pidPath := filepath.Join(dataDir, pidFile)
			defaultLog := filepath.Join(dataDir, "agent.log")

			if pid, alive, err := daemonize.CheckRunning(pidPath); err != nil {
				return fmt.Errorf("check existing agent: %w", err)
			} else if alive {
				return fmt.Errorf("agent already running (pid %d)", pid)
			}

			if daemon && !daemonize.IsChild() {
				if err := os.MkdirAll(dataDir, 0o700); err != nil {
					return fmt.Errorf("create data dir: %w", err)
				}
				return daemonize.Spawn(daemonize.SpawnOpts{
					LogPath:     daemonize.ResolveLogOutput(logOutput, true, defaultLog),
					PIDFilePath: pidPath,
				})
			}

			effectiveLog := daemonize.ResolveLogOutput(logOutput, daemon || daemonize.IsChild(), defaultLog)

			logger, err := common.SetupLogger(logLevel, effectiveLog)
			if err != nil {
				return fmt.Errorf("setup logger: %w", err)
			}

			d, err := agent.NewDaemon(cfgPath, logger)
			if err != nil {
				return fmt.Errorf("create daemon: %w", err)
			}
			d.OnReady = func() {
				if err := daemonize.WritePIDFile(pidPath); err != nil {
					logger.Warn("write pid file", "path", pidPath, "error", err)
				}
			}
			defer func() {
				if err := os.Remove(pidPath); err != nil && !os.IsNotExist(err) {
					logger.Warn("remove pid file", "path", pidPath, "error", err)
				}
			}()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

			go func() {
				<-sigCh
				cancel()
			}()

			if err := d.Run(ctx); err != nil {
				return fmt.Errorf("daemon run: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&logLevel, "log-level", "info", "log level (debug, info, warn, error)")
	cmd.Flags().StringVar(&logOutput, "log-output", "stderr", "log output (stderr or file path)")
	cmd.Flags().BoolVarP(&daemon, "daemon", "d", false, "detach from terminal and run in the background")

	return cmd
}
```

Update imports at the top of `cmd/hubfuse-agent/main.go`:

- Add: `"github.com/ykhdr/hubfuse/internal/common/daemonize"`.
- `"github.com/ykhdr/hubfuse/internal/common"` is already imported for `common.ProtocolVersion`; it also gives us `common.SetupLogger`. No other imports change.
- `"log/slog"` is still used by the other sub-commands (`joinCmd`, `pairCmd`, `devicesCmd`, etc.), so do not remove it.
- `"strconv"` was previously used for the manual PID-file write; if `startCmd` was its only consumer, remove it. Re-check all subcommands in the file — if none reference `strconv`, drop the import.

- [ ] **Step 11.2: Build and vet**

```
go build ./cmd/hubfuse-agent/
go vet ./cmd/hubfuse-agent/
```
Expected: success.

- [ ] **Step 11.3: Commit**

```bash
git add cmd/hubfuse-agent/main.go
git commit -m "Wire --daemon, --log-output, --log-level into hubfuse-agent start"
```

---

## Task 12: Full test + manual smoke + push

- [ ] **Step 12.1: Full build and test**

```
make vet
make test
```

Expected: both pass. If a hub integration test fails because the PID file no longer appears during `Start`, that's because the test relied on the old inline write; fix it by either setting `h.OnReady` in the test or removing the PID-file assertion — the test belongs to a caller, not the hub.

- [ ] **Step 12.2: Manual smoke — agent**

Assumes the agent has already joined a hub (i.e. `~/.hubfuse` contains `config.kdl`, `device.json`, and `tls/`). If not, this smoke test is skipped with a note in the PR description.

```bash
hubfuse-agent start --daemon
# expected: "started (pid N, logs: /Users/.../agent.log)" and control returns
hubfuse-agent status
# expected: "agent is running (pid N)"
hubfuse-agent devices
# expected: table of online devices (or "no devices online")
hubfuse-agent stop
# expected: "sent SIGTERM to agent (pid N)"
sleep 1
ls ~/.hubfuse/hubfuse-agent.pid 2>/dev/null && echo "LEAKED PIDFILE" || echo "pidfile cleaned"
# expected: "pidfile cleaned"
hubfuse-agent start --daemon
hubfuse-agent start --daemon
# expected (second call): error "agent already running (pid N)", exit 1
hubfuse-agent stop
```

- [ ] **Step 12.3: Manual smoke — hub**

```bash
hubfuse-hub start --daemon
hubfuse-hub status
hubfuse-hub stop
sleep 1
ls ~/.hubfuse-hub/hubfuse-hub.pid 2>/dev/null && echo "LEAKED PIDFILE" || echo "pidfile cleaned"
hubfuse-hub start --daemon
hubfuse-hub start --daemon
# expected (second call): error "hub already running (pid N)", exit 1
hubfuse-hub stop
```

- [ ] **Step 12.4: Push the branch**

```bash
git push -u origin feature/daemon-mode
```

- [ ] **Step 12.5: Open the pull request**

```bash
gh pr create --title "Add --daemon mode for agent and hub (#2)" --body "$(cat <<'EOF'
## Summary
- Adds `--daemon` / `-d` flag to `hubfuse-agent start` and `hubfuse-hub start`.
- Introduces reusable `internal/common/daemonize` package (self-respawn with Setsid, PID-file lifecycle, log-output resolution).
- Routes PID-file writes through a new `OnReady func()` hook on `agent.Daemon` and `hub.Hub` so the readiness signal is accurate.
- Agent gains `--log-output` and `--log-level` flags, matching the hub.

Closes #2.

## Test plan
- [x] `make vet`
- [x] `make test`
- [x] Manual smoke: `hubfuse-agent start --daemon` + `status` + `devices` + `stop` + duplicate-start error
- [x] Manual smoke: `hubfuse-hub start --daemon` + `status` + `stop` + duplicate-start error
EOF
)"
```

---

## Self-review notes

- **Spec coverage:** package skeleton (Task 1), IsChild (Task 1), WritePIDFile (Task 2), CheckRunning (Task 3), ResolveLogOutput (Task 4), Windows stub (Task 5), Spawn tests (Task 6), Spawn impl (Task 7), hub OnReady (Task 8), agent OnReady (Task 9), hub cmd wiring (Task 10), agent cmd wiring (Task 11), test + manual smoke + PR (Task 12). Every spec section has a concrete task.
- **Placeholder scan:** no "TBD" or "add error handling" remain. Every code step shows the full code.
- **Type consistency:** `SpawnOpts` fields (`LogPath`, `PIDFilePath`, `ReadyTimeout`) are used identically in Task 7, Task 10, Task 11. `OnReady` is `func()` in both `hub.Hub` and `agent.Daemon`. `CheckRunning` signature `(int, bool, error)` is consistent between Task 3 and its callers in Tasks 10/11.
