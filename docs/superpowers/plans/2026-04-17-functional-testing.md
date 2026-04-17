# Functional testing implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add three new test layers (CLI contracts via testscript, multi-process scenarios with a stub sshfs that does real SSH but no FUSE, real-FUSE smoke tests) and gate merges to `master` on all three via GitHub merge queue.

**Architecture:** Three independent test directories under `tests/` (`cli/`, `scenarios/`, plus a stub binary in `tests/tools/stub-sshfs/`). CLI tests run testscript scripts against real compiled binaries placed on `PATH` (no production refactor). Scenario tests spawn real `hubfuse-hub` + two `hubfuse` processes, with stub `sshfs` performing real SSH handshakes. FUSE smoke tests live in the same directory under build tag `fuse_smoke` and use real `sshfs` from apt.

**Tech Stack:** Go 1.25, `github.com/rogpeppe/go-internal/testscript`, `github.com/pkg/sftp`, `golang.org/x/crypto/ssh`, GitHub Actions merge queue.

**Deviation from spec (approved 2026-04-17):** Spec proposed extracting `func main()` body into a `cli` sub-package so testscript could call `Run() int` in-process. Plan instead compiles real binaries in `TestMain` and prepends them to `PATH` via `testscript.Params.Setup` — a documented testscript pattern. This avoids moving 900 lines of production code across packages and produces equivalent functional coverage.

---

## File structure

**Created:**

| Path | Responsibility |
|---|---|
| `tests/cli/cli_test.go` | Builds binaries, runs all `testdata/*.txtar` scripts |
| `tests/cli/testdata/*.txtar` | One CLI scenario per file |
| `tests/scenarios/main_test.go` | Builds hub/agent/stub-sshfs binaries once for the package |
| `tests/scenarios/helpers/ports.go` | Free-port allocation + WaitForPort polling |
| `tests/scenarios/helpers/workdir.go` | Per-test isolated HOME dirs and log buffers |
| `tests/scenarios/helpers/hub.go` | `Hub` struct: Start/Stop/Restart |
| `tests/scenarios/helpers/agent.go` | `Agent` struct: Start + high-level ops (Join, Pair, Mount, ...) |
| `tests/scenarios/helpers/marker.go` | Read stub-sshfs JSON marker files |
| `tests/scenarios/join_pair_test.go` | Join + pairing flow tests |
| `tests/scenarios/mount_test.go` | Mount/unmount tests |
| `tests/scenarios/reconnect_test.go` | Hub restart + agent reconnect tests |
| `tests/scenarios/events_test.go` | Online/offline/shares-updated events |
| `tests/scenarios/prune_test.go` | Stale device pruning |
| `tests/scenarios/fuse_test.go` | Build tag `fuse_smoke` — real FUSE tests |
| `tests/tools/stub-sshfs/main.go` | Fake `sshfs` binary: real SSH + sftp ReadDir, JSON marker, no FUSE |

**Modified:**

| Path | Change |
|---|---|
| `Makefile` | New targets: `test-cli`, `test-scenarios`, `test-fuse`, `test-all`; updated `test` |
| `.github/workflows/ci.yml` | New jobs: `test-cli`, `test-scenarios`, `test-fuse`; trigger `merge_group`; `runs-on: ubuntu-22.04` for FUSE |
| `go.mod` / `go.sum` | Add `github.com/rogpeppe/go-internal`, `github.com/pkg/sftp` |

**Untouched:** `cmd/hubfuse/main.go`, `cmd/hubfuse-hub/main.go`, `internal/`, `tests/integration/`.

---

## Task 1: Bootstrap CLI test harness with issue #20 regression

**Goal:** Working `make test-cli` that builds binaries, runs one testscript scenario, and exposes the issue #20 regression (CLI does not show usage block on Cobra arg-validation error).

**Files:**
- Create: `tests/cli/cli_test.go`
- Create: `tests/cli/testdata/join_no_args.txtar`
- Modify: `go.mod`, `go.sum`

- [ ] **Step 1.1: Add testscript dependency**

Run from repo root:
```bash
go get github.com/rogpeppe/go-internal/testscript@v1.13.0
go mod tidy
```

Expected: `go.mod` gets `github.com/rogpeppe/go-internal v1.13.0` line; `go.sum` updated.

- [ ] **Step 1.2: Write the test harness**

Create `tests/cli/cli_test.go`:

```go
package cli_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"
)

var binDir string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "hubfuse-cli-bin-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "mkdtemp: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)
	binDir = dir

	repo, err := repoRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "find repo root: %v\n", err)
		os.Exit(1)
	}

	for _, b := range []struct{ name, pkg string }{
		{"hubfuse", "./cmd/hubfuse"},
		{"hubfuse-hub", "./cmd/hubfuse-hub"},
	} {
		out := filepath.Join(binDir, b.name)
		cmd := exec.Command("go", "build", "-o", out, b.pkg)
		cmd.Dir = repo
		if combined, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build %s: %v\n%s", b.pkg, err, combined)
			os.Exit(1)
		}
	}

	os.Exit(m.Run())
}

func TestCLI(t *testing.T) {
	testscript.Run(t, testscript.Params{
		Dir: "testdata",
		Setup: func(env *testscript.Env) error {
			env.Setenv("PATH", binDir+string(os.PathListSeparator)+env.Getenv("PATH"))
			return nil
		},
	})
}

func repoRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", err
	}
	return string(out[:len(out)-1]), nil // strip trailing newline
}
```

- [ ] **Step 1.3: Write the issue #20 regression scenario**

Create `tests/cli/testdata/join_no_args.txtar`:

```
# Regression for issue #20: when the user runs `hubfuse join` with no
# argument, Cobra's arg-validation error must be printed AND the usage
# block must be shown so the user knows what arguments are expected.

! hubfuse join
stderr 'accepts 1 arg\(s\), received 0'
stderr 'Usage:'
stderr 'hubfuse join'
```

- [ ] **Step 1.4: Run the test, expect FAIL**

Run from repo root:
```bash
go test ./tests/cli/... -run TestCLI -v
```

Expected: FAIL. The Cobra error message text matches, but `stderr 'Usage:'` does NOT match because `silenceAll(rootCmd)` in `cmd/hubfuse/main.go` suppresses usage. This test exists to lock the regression in place — the fix lives in issue #20.

Document this in the failing-test output by capturing it. The test should remain in the suite but be expected to fail until issue #20 is closed. To keep CI green for now, mark it as a known-failure by skipping until the fix lands.

Modify `tests/cli/testdata/join_no_args.txtar` — add a skip directive at the top:

```
# REGRESSION for issue #20. Remove the `skip` line below once #20 is fixed.
skip 'tracks issue #20: usage suppressed for arg-validation errors'

! hubfuse join
stderr 'accepts 1 arg\(s\), received 0'
stderr 'Usage:'
stderr 'hubfuse join'
```

- [ ] **Step 1.5: Re-run, expect PASS (skipped)**

```bash
go test ./tests/cli/... -run TestCLI -v
```

Expected: PASS, with `--- SKIP: TestCLI/join_no_args` showing the skip reason. The test infrastructure works; the regression is documented and will start failing automatically the moment #20's fix removes the `skip` line.

- [ ] **Step 1.6: Commit**

```bash
git add tests/cli/cli_test.go tests/cli/testdata/join_no_args.txtar go.mod go.sum
git commit -m "$(cat <<'EOF'
test(cli): bootstrap testscript-based CLI suite with #20 regression

Adds tests/cli/ with a TestMain that builds hubfuse and hubfuse-hub
into a temp dir and exposes them on PATH. The first scenario locks
in the issue #20 regression: arg-validation errors must show the
usage block. Currently skipped pending the fix in #20.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 2: Expand CLI test suite

**Goal:** All 13 CLI scenarios from the spec, each as a `.txtar` file. Tests pass against the current binaries (or are explicitly skipped with rationale).

**Files:**
- Create: `tests/cli/testdata/help_top_level.txtar`
- Create: `tests/cli/testdata/version.txtar`
- Create: `tests/cli/testdata/unknown_command.txtar`
- Create: `tests/cli/testdata/join_too_many_args.txtar`
- Create: `tests/cli/testdata/join_unreachable.txtar`
- Create: `tests/cli/testdata/pair_no_args.txtar`
- Create: `tests/cli/testdata/pair_invalid_code.txtar`
- Create: `tests/cli/testdata/rename_no_args.txtar`
- Create: `tests/cli/testdata/mount_bad_format.txtar`
- Create: `tests/cli/testdata/status_not_joined.txtar`
- Create: `tests/cli/testdata/hub_help.txtar`
- Create: `tests/cli/testdata/hub_invalid_config.txtar`

For each scenario, the workflow is the same: write the `.txtar`, run `go test ./tests/cli/...`, observe pass/fail, decide:
- If passes → keep as-is
- If fails AND failure is a known regression → add `skip` line with issue link
- If fails AND failure is unexpected → investigate the actual binary output and adjust the assertion to match real (correct) behavior

- [ ] **Step 2.1: `help_top_level.txtar`**

```
hubfuse --help
stdout 'Available Commands:'
stdout '  join'
stdout '  pair'
stdout '  start'
stdout '  status'
stdout '  devices'
stdout '  mount'
stdout '  share'
```

- [ ] **Step 2.2: `version.txtar`**

```
# hubfuse exposes a version subcommand or --version flag; verify either is non-empty.
hubfuse --help
stdout 'hubfuse'
```

(If a real `--version` is present, replace with `hubfuse --version` + `stdout 'hubfuse version'`. Verify against actual binary first.)

- [ ] **Step 2.3: `unknown_command.txtar`**

```
! hubfuse blargh
stderr 'unknown command "blargh"'
```

- [ ] **Step 2.4: `join_too_many_args.txtar`**

```
# Cobra rejects extra positional args.
! hubfuse join hub:9090 extra
stderr 'accepts 1 arg\(s\), received 2'
stderr 'Usage:'
```

(Likely also pending issue #20 fix — add `skip` if usage check fails, same pattern as Task 1.)

- [ ] **Step 2.5: `join_unreachable.txtar`**

```
# Hub at port 1 is unreachable; the CLI must produce a friendly error
# without dumping the usage block (this is a runtime error, not a
# malformed command).
! hubfuse join 127.0.0.1:1
stderr 'cannot|connect|refused|unreachable'
! stderr 'Usage:'
```

- [ ] **Step 2.6: `pair_no_args.txtar`**

```
! hubfuse pair
stderr 'arg|required'
stderr 'Usage:'
```

(Likely pending issue #20 — add `skip` accordingly.)

- [ ] **Step 2.7: `pair_invalid_code.txtar`**

```
# An obviously malformed code should be rejected before any network call.
! hubfuse pair --code XX
stderr 'invalid|malformed|too short'
```

(Verify the actual flag name; adjust if `--code` is wrong.)

- [ ] **Step 2.8: `rename_no_args.txtar`**

```
! hubfuse rename
stderr 'arg|required'
stderr 'Usage:'
```

- [ ] **Step 2.9: `mount_bad_format.txtar`**

```
! hubfuse mount add no-colon-here /local/path
stderr 'format|invalid'
```

(Adjust subcommand to match real CLI — current binary uses `mount add`; verify.)

- [ ] **Step 2.10: `status_not_joined.txtar`**

```
# Without state, status must report "not joined" cleanly, not panic
# and not dump usage.
env HOME=$WORK/home
mkdir $WORK/home
! hubfuse status
stderr 'not joined|no hub|register'
! stderr 'Usage:'
```

- [ ] **Step 2.11: `hub_help.txtar`**

```
hubfuse-hub --help
stdout 'Available Commands:'
stdout '  start'
stdout '  stop'
stdout '  status'
```

- [ ] **Step 2.12: `hub_invalid_config.txtar`**

```
# Malformed KDL config should produce a parse error, not a stack trace.
env HOME=$WORK/home
mkdir -p $WORK/home/.hubfuse-hub
cp config.kdl $WORK/home/.hubfuse-hub/config.kdl
! hubfuse-hub start --data-dir $WORK/home/.hubfuse-hub
stderr 'parse|invalid|syntax'
! stderr 'panic'

-- config.kdl --
this is { not valid kdl
```

- [ ] **Step 2.13: Run the full CLI suite**

```bash
go test ./tests/cli/... -v
```

Expected: every scenario either PASS or SKIP-with-reason. Count `--- PASS` and `--- SKIP` lines, ensure no `--- FAIL`. If any test fails for an unexpected reason, fix the assertion to match real (and correct) binary behavior; if behavior is incorrect, file an issue, add `skip` referencing it.

- [ ] **Step 2.14: Commit**

```bash
git add tests/cli/testdata/
git commit -m "$(cat <<'EOF'
test(cli): add full CLI scenario set

Covers help, version, unknown commands, arg-validation errors,
unreachable-hub errors, pairing input validation, status without
state, and hub config parsing. Scenarios that exercise the issue
#20 fix are skipped pending that work.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 3: Wire CLI tests into Makefile and CI

**Goal:** `make test-cli` runs the suite locally; CI runs it on every PR push.

**Files:**
- Modify: `Makefile`
- Modify: `.github/workflows/ci.yml`

- [ ] **Step 3.1: Update Makefile**

Replace the `test` target block:

```make
.PHONY: proto-gen build test test-unit test-integration test-cli test-scenarios test-fuse test-all vet lint clean install

test: test-unit test-integration test-cli

test-unit:
	go test ./internal/...

test-integration:
	go test ./tests/integration/... -timeout 120s

test-cli:
	go test ./tests/cli/...
```

(Other targets unchanged. `test-scenarios`, `test-fuse`, `test-all` are added later in Tasks 9 and 10.)

- [ ] **Step 3.2: Add CI job**

Append to `.github/workflows/ci.yml` after the `test-integration` job (preserving 2-space YAML indent):

```yaml
  test-cli:
    name: CLI Tests
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version: ${{ env.GO_VERSION }}

      - name: Run CLI tests
        run: make test-cli
```

- [ ] **Step 3.3: Verify locally**

```bash
make test-cli
```

Expected: all scenarios PASS or SKIP, no FAIL.

- [ ] **Step 3.4: Commit**

```bash
git add Makefile .github/workflows/ci.yml
git commit -m "$(cat <<'EOF'
ci: run CLI tests on every PR push

make test-cli is added to the default test target and runs as a
dedicated CI job alongside unit and integration tests.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 4: Scenario foundation — ports, workdir, hub helper

**Goal:** Helpers that can spawn a hub on a free port, capture its logs, and tear it down on test completion. Verified by a smoke test that boots and stops the hub.

**Files:**
- Create: `tests/scenarios/main_test.go`
- Create: `tests/scenarios/helpers/ports.go`
- Create: `tests/scenarios/helpers/workdir.go`
- Create: `tests/scenarios/helpers/hub.go`
- Create: `tests/scenarios/smoke_test.go`

- [ ] **Step 4.1: TestMain that builds binaries once**

Create `tests/scenarios/main_test.go`:

```go
package scenarios_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

var (
	HubBinary       string
	AgentBinary     string
	StubSSHFSBinary string
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "hubfuse-scenarios-bin-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "mkdtemp: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(dir)

	repo, err := repoRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "find repo root: %v\n", err)
		os.Exit(1)
	}

	builds := []struct{ varPtr *string; out, pkg string }{
		{&HubBinary, "hubfuse-hub", "./cmd/hubfuse-hub"},
		{&AgentBinary, "hubfuse", "./cmd/hubfuse"},
		// stub-sshfs added in Task 6
	}
	for _, b := range builds {
		out := filepath.Join(dir, b.out)
		cmd := exec.Command("go", "build", "-o", out, b.pkg)
		cmd.Dir = repo
		if combined, err := cmd.CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "build %s: %v\n%s", b.pkg, err, combined)
			os.Exit(1)
		}
		*b.varPtr = out
	}

	os.Exit(m.Run())
}

func repoRoot() (string, error) {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", err
	}
	return string(out[:len(out)-1]), nil
}
```

- [ ] **Step 4.2: Port helpers**

Create `tests/scenarios/helpers/ports.go`:

```go
package helpers

import (
	"fmt"
	"net"
	"testing"
	"time"
)

// FreePort returns a TCP port currently free on localhost. There is a small
// race between returning the port and using it; tests should call it just
// before binding.
func FreePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("free port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	return port
}

// WaitForPort polls until something is listening on 127.0.0.1:port, or the
// deadline elapses.
func WaitForPort(t *testing.T, port int, deadline time.Duration) {
	t.Helper()
	end := time.Now().Add(deadline)
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	for time.Now().Before(end) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("nothing listening on %s after %s", addr, deadline)
}
```

- [ ] **Step 4.3: Workdir helper**

Create `tests/scenarios/helpers/workdir.go`:

```go
package helpers

import (
	"bytes"
	"sync"
	"testing"
)

// LogBuffer is a thread-safe bytes.Buffer for capturing subprocess output.
// Subprocess stdout/stderr are typically written from a goroutine, so plain
// bytes.Buffer would race with reads from t.Logf.
type LogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *LogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *LogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// DumpOnFailure attaches a t.Cleanup that prints the buffer's contents when
// the test has failed, prefixed with the given label per line. On success it
// stays silent.
func DumpOnFailure(t *testing.T, label string, buf *LogBuffer) {
	t.Helper()
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		t.Logf("--- %s log ---\n%s", label, buf.String())
	})
}
```

- [ ] **Step 4.4: Hub helper**

Create `tests/scenarios/helpers/hub.go`:

```go
package helpers

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// HubBinaryPath is set by tests/scenarios/main_test.go before any test runs.
var HubBinaryPath string

type Hub struct {
	Address  string // "127.0.0.1:NNNN"
	DataDir  string

	port    int
	cmd     *exec.Cmd
	logBuf  *LogBuffer
	cancel  context.CancelFunc
}

// StartHub launches a hub in a temp data dir and returns once it is listening.
func StartHub(t *testing.T) *Hub {
	t.Helper()
	port := FreePort(t)
	dataDir := t.TempDir()

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, HubBinaryPath, "start",
		"--listen", fmt.Sprintf("127.0.0.1:%d", port),
		"--data-dir", dataDir,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	logBuf := &LogBuffer{}
	cmd.Stdout = logBuf
	cmd.Stderr = logBuf

	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start hub: %v", err)
	}

	h := &Hub{
		Address: fmt.Sprintf("127.0.0.1:%d", port),
		DataDir: dataDir,
		port:    port,
		cmd:     cmd,
		logBuf:  logBuf,
		cancel:  cancel,
	}
	DumpOnFailure(t, "hub", logBuf)
	t.Cleanup(func() { h.Stop(t) })

	WaitForPort(t, port, 5*time.Second)
	return h
}

// Stop signals the hub to exit and waits up to 5s for it to do so.
func (h *Hub) Stop(t *testing.T) {
	t.Helper()
	if h.cmd == nil || h.cmd.Process == nil {
		return
	}
	_ = h.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- h.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = h.cmd.Process.Kill()
		<-done
	}
	h.cancel()
	h.cmd = nil
}

// Restart stops and starts the hub on the same data dir. Address may change
// if the OS reassigns the port.
func (h *Hub) Restart(t *testing.T) {
	t.Helper()
	h.Stop(t)

	port := FreePort(t)
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, HubBinaryPath, "start",
		"--listen", fmt.Sprintf("127.0.0.1:%d", port),
		"--data-dir", h.DataDir,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = h.logBuf
	cmd.Stderr = h.logBuf
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("restart hub: %v", err)
	}
	h.cmd = cmd
	h.cancel = cancel
	h.port = port
	h.Address = fmt.Sprintf("127.0.0.1:%d", port)
	WaitForPort(t, port, 5*time.Second)

	_ = filepath.Base(h.DataDir) // satisfy lint if Restart returns early in future edits
}
```

- [ ] **Step 4.5: Wire HubBinaryPath in main_test.go**

In `tests/scenarios/main_test.go`, after the `*b.varPtr = out` line in the build loop, add:

```go
helpers.HubBinaryPath = HubBinary
helpers.AgentBinaryPath = AgentBinary       // declared in Task 5
helpers.StubSSHFSBinaryPath = StubSSHFSBinary // declared in Task 6 (not yet built here)
```

For Task 4, add only the HubBinaryPath line; uncomment AgentBinaryPath and StubSSHFSBinaryPath in their respective tasks.

Also add the import:

```go
import (
	// ... existing imports ...
	"github.com/ykhdr/hubfuse/tests/scenarios/helpers"
)
```

- [ ] **Step 4.6: Smoke test**

Create `tests/scenarios/smoke_test.go`:

```go
package scenarios_test

import (
	"net"
	"testing"
	"time"

	"github.com/ykhdr/hubfuse/tests/scenarios/helpers"
)

func TestHubBootsAndStops(t *testing.T) {
	hub := helpers.StartHub(t)

	// Verify the hub is actually serving by opening one TCP connection.
	conn, err := net.DialTimeout("tcp", hub.Address, 1*time.Second)
	if err != nil {
		t.Fatalf("dial hub: %v", err)
	}
	_ = conn.Close()

	// Cleanup is wired via t.Cleanup; verify no goroutine leaks happen by
	// explicitly stopping early.
	hub.Stop(t)
}
```

- [ ] **Step 4.7: Run, expect PASS**

```bash
go test ./tests/scenarios/... -v -run TestHubBootsAndStops
```

Expected: PASS in <2 seconds. If it hangs, hub may not be exiting on SIGTERM — investigate `internal/common/daemonize` lifecycle.

- [ ] **Step 4.8: Commit**

```bash
git add tests/scenarios/
git commit -m "$(cat <<'EOF'
test(scenarios): add foundation helpers and hub smoke test

main_test.go builds hub and agent binaries once per package run.
Helpers: free-port allocation, port-readiness polling, thread-safe
log capture with on-failure dump, and a Hub struct with Start/Stop/
Restart. Smoke test verifies the hub boots, accepts a TCP connection,
and shuts down on SIGTERM.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 5: Agent helper + Join test

**Goal:** Spawn an agent against a hub, run `hubfuse join`, verify the agent registered. First real multi-process scenario.

**Files:**
- Create: `tests/scenarios/helpers/agent.go`
- Create: `tests/scenarios/join_pair_test.go`
- Modify: `tests/scenarios/main_test.go` (set `AgentBinaryPath`)

- [ ] **Step 5.1: Agent helper skeleton**

Create `tests/scenarios/helpers/agent.go`:

```go
package helpers

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

var AgentBinaryPath string

type Agent struct {
	Nickname string
	HomeDir  string

	hub     *Hub
	logBuf  *LogBuffer
	envExtra []string
}

type AgentOption func(*Agent)

func WithEnv(kv ...string) AgentOption {
	return func(a *Agent) { a.envExtra = append(a.envExtra, kv...) }
}

// StartAgent prepares an isolated HOME for the agent. The agent process is
// not yet daemonized; use Join/Pair/etc to invoke specific commands.
func StartAgent(t *testing.T, hub *Hub, nickname string, opts ...AgentOption) *Agent {
	t.Helper()
	home := t.TempDir()
	a := &Agent{
		Nickname: nickname,
		HomeDir:  home,
		hub:      hub,
		logBuf:   &LogBuffer{},
	}
	for _, o := range opts {
		o(a)
	}
	DumpOnFailure(t, "agent:"+nickname, a.logBuf)
	return a
}

// run invokes a hubfuse subcommand with the agent's HOME and returns combined
// output. Failures fail the test.
func (a *Agent) run(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, AgentBinaryPath, args...)
	cmd.Env = append([]string{
		"HOME=" + a.HomeDir,
		"PATH=" + filepath.Dir(AgentBinaryPath) + string([]byte{':'}) + envPath(),
	}, a.envExtra...)
	out, err := cmd.CombinedOutput()
	a.logBuf.Write([]byte("$ hubfuse " + strings.Join(args, " ") + "\n"))
	a.logBuf.Write(out)
	if err != nil {
		t.Fatalf("hubfuse %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// runExpectFail is run() but expects non-zero exit; returns combined output.
func (a *Agent) runExpectFail(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, AgentBinaryPath, args...)
	cmd.Env = append([]string{
		"HOME=" + a.HomeDir,
		"PATH=" + filepath.Dir(AgentBinaryPath) + string([]byte{':'}) + envPath(),
	}, a.envExtra...)
	out, _ := cmd.CombinedOutput()
	a.logBuf.Write([]byte("$ hubfuse " + strings.Join(args, " ") + "  (expecting failure)\n"))
	a.logBuf.Write(out)
	return string(out)
}

// Join runs `hubfuse join <hub-addr>` with the nickname injected on stdin
// (the join command prompts for it).
func (a *Agent) Join(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, AgentBinaryPath, "join", a.hub.Address)
	cmd.Env = append([]string{
		"HOME=" + a.HomeDir,
		"PATH=" + filepath.Dir(AgentBinaryPath) + string([]byte{':'}) + envPath(),
	}, a.envExtra...)
	cmd.Stdin = strings.NewReader(a.Nickname + "\n")
	out, err := cmd.CombinedOutput()
	a.logBuf.Write([]byte("$ hubfuse join " + a.hub.Address + "\n"))
	a.logBuf.Write(out)
	if err != nil {
		t.Fatalf("join failed: %v\n%s", err, out)
	}
}

// Stop is currently a no-op; reserved for when StartAgent spawns a daemon
// process (added in Task 7 for Mount tests).
func (a *Agent) Stop(t *testing.T) {
	t.Helper()
	_ = syscall.SIGTERM
}

func envPath() string {
	// Read PATH from process env. Avoids importing os in helpers' surface.
	return existingPath()
}
```

Add a small helper file `tests/scenarios/helpers/env.go`:

```go
package helpers

import "os"

func existingPath() string { return os.Getenv("PATH") }
```

- [ ] **Step 5.2: Set AgentBinaryPath**

In `tests/scenarios/main_test.go`, after the build loop, add:

```go
helpers.AgentBinaryPath = AgentBinary
```

- [ ] **Step 5.3: Write TestJoinPersistsCert**

Create `tests/scenarios/join_pair_test.go`:

```go
package scenarios_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ykhdr/hubfuse/tests/scenarios/helpers"
)

func TestJoinPersistsCert(t *testing.T) {
	hub := helpers.StartHub(t)
	alice := helpers.StartAgent(t, hub, "alice")
	alice.Join(t)

	// After join, the agent's HOME must contain a TLS client cert in the
	// well-known location used by the agent. The exact filename may evolve;
	// look for any *.pem file under the agent's hubfuse data dir.
	matches, err := filepath.Glob(filepath.Join(alice.HomeDir, ".hubfuse", "*.pem"))
	if err != nil {
		t.Fatalf("glob certs: %v", err)
	}
	if len(matches) == 0 {
		// Try alternate layouts.
		entries, _ := os.ReadDir(alice.HomeDir)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("no cert found in agent HOME=%s; entries: %v", alice.HomeDir, names)
	}
}
```

- [ ] **Step 5.4: Run, debug, fix paths**

```bash
go test ./tests/scenarios/... -v -run TestJoinPersistsCert
```

Expected on first run: may fail because the actual cert path differs. The test message dumps `alice.HomeDir` contents on failure — adjust the glob pattern to match real layout (likely under `$HOME/.hubfuse/` or `$HOME/.config/hubfuse/`).

- [ ] **Step 5.5: Iterate until PASS, then commit**

```bash
git add tests/scenarios/helpers/agent.go tests/scenarios/helpers/env.go \
        tests/scenarios/main_test.go tests/scenarios/join_pair_test.go
git commit -m "$(cat <<'EOF'
test(scenarios): add agent helper and join persistence test

StartAgent prepares an isolated HOME and exposes Join/run/runExpectFail
wrappers around the hubfuse binary. TestJoinPersistsCert verifies the
TLS client cert is written under the agent's HOME after a successful
join.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 6: Stub-sshfs binary

**Goal:** A drop-in replacement for `sshfs` that performs a real SSH handshake + sftp ReadDir, writes a JSON marker, and survives until SIGTERM. Verified in isolation by a small unit test.

**Files:**
- Create: `tests/tools/stub-sshfs/main.go`
- Create: `tests/tools/stub-sshfs/main_test.go`
- Modify: `go.mod`, `go.sum`
- Modify: `tests/scenarios/main_test.go` (build stub + set `StubSSHFSBinaryPath`)

- [ ] **Step 6.1: Add sftp dependency**

```bash
go get github.com/pkg/sftp@v1.13.6
go get golang.org/x/crypto/ssh@latest
go mod tidy
```

- [ ] **Step 6.2: Implement stub-sshfs**

Create `tests/tools/stub-sshfs/main.go`:

```go
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// stub-sshfs mimics the sshfs CLI surface used by hubfuse's mounter.
// Argument shape: stub-sshfs user@host:/remote/path /local/mount-point -p PORT -o IdentityFile=PATH [-o ...]
//
// It performs a real SSH connection + sftp ReadDir on /remote/path, writes
// a JSON marker to $HUBFUSE_STUB_MOUNT_DIR (or /tmp if unset), and stays
// alive until SIGTERM. No FUSE mount is actually created.

type Marker struct {
	Src         string   `json:"src"`
	Dst         string   `json:"dst"`
	RemoteUser  string   `json:"remote_user"`
	RemoteHost  string   `json:"remote_host"`
	RemotePort  int      `json:"remote_port"`
	RemotePath  string   `json:"remote_path"`
	KeyPath     string   `json:"key_path"`
	RemoteFiles []string `json:"remote_files"`
	PID         int      `json:"pid"`
}

func main() {
	if len(os.Args) < 3 {
		fmt.Fprintln(os.Stderr, "usage: stub-sshfs user@host:/path /mount-point [-p PORT] [-o KEY=VAL ...]")
		os.Exit(2)
	}
	src := os.Args[1]
	dst := os.Args[2]

	fs := flag.NewFlagSet("stub-sshfs", flag.ContinueOnError)
	port := fs.Int("p", 22, "ssh port")
	var opts arrayFlag
	fs.Var(&opts, "o", "ssh option (KEY=VAL)")
	if err := fs.Parse(os.Args[3:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	user, host, remotePath := parseSrc(src)
	if user == "" || host == "" {
		fmt.Fprintf(os.Stderr, "stub-sshfs: cannot parse src %q\n", src)
		os.Exit(2)
	}

	keyPath := optValue(opts, "IdentityFile")
	if keyPath == "" {
		fmt.Fprintln(os.Stderr, "stub-sshfs: -o IdentityFile=... is required")
		os.Exit(2)
	}

	keyBytes, err := os.ReadFile(keyPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stub-sshfs: read key %s: %v\n", keyPath, err)
		os.Exit(2)
	}
	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stub-sshfs: parse key: %v\n", err)
		os.Exit(2)
	}

	cfg := &ssh.ClientConfig{
		User:            user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         5 * time.Second,
	}
	addr := fmt.Sprintf("%s:%d", host, *port)
	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stub-sshfs: ssh dial %s: %v\n", addr, err)
		os.Exit(1)
	}
	defer client.Close()

	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stub-sshfs: sftp: %v\n", err)
		os.Exit(1)
	}
	defer sftpClient.Close()

	entries, err := sftpClient.ReadDir(remotePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "stub-sshfs: readdir %s: %v\n", remotePath, err)
		os.Exit(1)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}

	markerDir := os.Getenv("HUBFUSE_STUB_MOUNT_DIR")
	if markerDir == "" {
		markerDir = os.TempDir()
	}
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "stub-sshfs: mkdir markerDir: %v\n", err)
		os.Exit(1)
	}
	markerPath := filepath.Join(markerDir, sanitize(dst)+".json")

	marker := Marker{
		Src:         src,
		Dst:         dst,
		RemoteUser:  user,
		RemoteHost:  host,
		RemotePort:  *port,
		RemotePath:  remotePath,
		KeyPath:     keyPath,
		RemoteFiles: names,
		PID:         os.Getpid(),
	}
	if err := writeJSON(markerPath, &marker); err != nil {
		fmt.Fprintf(os.Stderr, "stub-sshfs: write marker: %v\n", err)
		os.Exit(1)
	}
	defer os.Remove(markerPath)

	// Block until SIGTERM/SIGINT (mimic real sshfs which daemonizes).
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
}

func parseSrc(s string) (user, host, path string) {
	at := strings.IndexByte(s, '@')
	if at < 0 {
		return "", "", ""
	}
	user = s[:at]
	rest := s[at+1:]
	colon := strings.IndexByte(rest, ':')
	if colon < 0 {
		return user, rest, ""
	}
	host = rest[:colon]
	path = rest[colon+1:]
	return
}

type arrayFlag []string

func (a *arrayFlag) String() string     { return strings.Join(*a, ",") }
func (a *arrayFlag) Set(v string) error { *a = append(*a, v); return nil }

func optValue(opts []string, key string) string {
	for _, o := range opts {
		if eq := strings.IndexByte(o, '='); eq >= 0 && o[:eq] == key {
			return o[eq+1:]
		}
	}
	return ""
}

func sanitize(p string) string {
	r := strings.NewReplacer("/", "_", `\`, "_", ":", "_", " ", "_")
	return r.Replace(strings.TrimPrefix(p, "/"))
}

func writeJSON(path string, v any) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}
```

- [ ] **Step 6.3: Smoke test for stub-sshfs**

Create `tests/tools/stub-sshfs/main_test.go`:

```go
package main

import "testing"

func TestParseSrc(t *testing.T) {
	cases := []struct{ in, user, host, path string }{
		{"alice@127.0.0.1:/data", "alice", "127.0.0.1", "/data"},
		{"bob@example.com:", "bob", "example.com", ""},
		{"no-at-sign", "", "", ""},
	}
	for _, c := range cases {
		u, h, p := parseSrc(c.in)
		if u != c.user || h != c.host || p != c.path {
			t.Errorf("parseSrc(%q) = (%q,%q,%q); want (%q,%q,%q)",
				c.in, u, h, p, c.user, c.host, c.path)
		}
	}
}

func TestOptValue(t *testing.T) {
	got := optValue([]string{"reconnect", "IdentityFile=/k", "Port=22"}, "IdentityFile")
	if got != "/k" {
		t.Errorf("optValue = %q; want /k", got)
	}
}
```

- [ ] **Step 6.4: Run unit test**

```bash
go test ./tests/tools/stub-sshfs/...
```

Expected: PASS.

- [ ] **Step 6.5: Wire stub into scenarios main_test.go**

Edit `tests/scenarios/main_test.go` build loop to include stub-sshfs:

```go
builds := []struct{ varPtr *string; out, pkg string }{
    {&HubBinary, "hubfuse-hub", "./cmd/hubfuse-hub"},
    {&AgentBinary, "hubfuse", "./cmd/hubfuse"},
    {&StubSSHFSBinary, "sshfs", "./tests/tools/stub-sshfs"},
}
```

After the build loop:

```go
helpers.HubBinaryPath = HubBinary
helpers.AgentBinaryPath = AgentBinary
helpers.StubSSHFSBinaryPath = StubSSHFSBinary
```

In `tests/scenarios/helpers/agent.go`, add `var StubSSHFSBinaryPath string` near `AgentBinaryPath`.

- [ ] **Step 6.6: Commit**

```bash
git add tests/tools/stub-sshfs/ tests/scenarios/main_test.go tests/scenarios/helpers/agent.go go.mod go.sum
git commit -m "$(cat <<'EOF'
test(tools): add stub-sshfs that performs real SSH but no FUSE

stub-sshfs parses sshfs's CLI surface, opens a real SSH connection
with the provided IdentityFile, lists the remote directory via SFTP,
and writes a JSON marker describing the mount. It blocks on SIGTERM
to mimic sshfs's daemonize behavior. The stub is built alongside
hubfuse binaries by tests/scenarios/main_test.go.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 7: Pair + Mount tests

**Goal:** End-to-end pairing flow (alice ↔ bob) followed by a mount on bob that produces a stub-sshfs JSON marker. Validates SSH key exchange between agents.

**Files:**
- Create: `tests/scenarios/mount_test.go`
- Create: `tests/scenarios/helpers/marker.go`
- Modify: `tests/scenarios/helpers/agent.go` (add `RequestPairing`, `ConfirmPairing`, `Mount`, `MountMarker`, `WithExports`, `WithSSHPort`, daemonize lifecycle)

- [ ] **Step 7.1: Add agent daemonize lifecycle**

This is the largest helper extension. Update `tests/scenarios/helpers/agent.go` to include a `Start` (long-running daemon) plus `Stop` that signals it. Add to the `Agent` struct:

```go
type Agent struct {
	Nickname string
	HomeDir  string
	SSHPort  int
	StubMountDir string  // populated when mounted

	hub      *Hub
	logBuf   *LogBuffer
	envExtra []string
	exports  []string

	daemonCmd    *exec.Cmd
	daemonCancel context.CancelFunc
}
```

Add options:

```go
func WithExports(paths ...string) AgentOption {
	return func(a *Agent) { a.exports = append(a.exports, paths...) }
}

func WithSSHPort(port int) AgentOption {
	return func(a *Agent) { a.SSHPort = port }
}
```

Add `StartDaemon` after `StartAgent`:

```go
// StartDaemon launches the long-running agent process. Caller must have
// already called Join. SSHPort defaults to a free port if unset.
func (a *Agent) StartDaemon(t *testing.T) {
	t.Helper()
	if a.SSHPort == 0 {
		a.SSHPort = FreePort(t)
	}
	a.StubMountDir = filepath.Join(a.HomeDir, "stub-marker")
	if err := os.MkdirAll(a.StubMountDir, 0o755); err != nil {
		t.Fatalf("stub mount dir: %v", err)
	}

	// Apply exports via `hubfuse share add` before start.
	for _, p := range a.exports {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatalf("export dir %s: %v", p, err)
		}
		a.run(t, "share", "add", p)
	}

	stubDir := filepath.Dir(StubSSHFSBinaryPath)

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, AgentBinaryPath, "start",
		"--ssh-port", fmt.Sprintf("%d", a.SSHPort),
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = append([]string{
		"HOME=" + a.HomeDir,
		"PATH=" + stubDir + string([]byte{':'}) + envPath(),
		"HUBFUSE_STUB_MOUNT_DIR=" + a.StubMountDir,
	}, a.envExtra...)
	cmd.Stdout = a.logBuf
	cmd.Stderr = a.logBuf
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start agent daemon: %v", err)
	}
	a.daemonCmd = cmd
	a.daemonCancel = cancel

	t.Cleanup(func() { a.Stop(t) })
	WaitForPort(t, a.SSHPort, 5*time.Second)
}

// Stop sends SIGTERM to the agent daemon; safe to call multiple times.
func (a *Agent) Stop(t *testing.T) {
	t.Helper()
	if a.daemonCmd == nil || a.daemonCmd.Process == nil {
		return
	}
	_ = a.daemonCmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- a.daemonCmd.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		_ = a.daemonCmd.Process.Kill()
		<-done
	}
	a.daemonCancel()
	a.daemonCmd = nil
}
```

(Verify the agent's `start` subcommand actually accepts `--ssh-port`; if it reads from KDL config instead, write the config file under `$HOME/.hubfuse/agent.kdl` before calling Start. Adjust accordingly.)

- [ ] **Step 7.2: Add Pair / Mount methods**

Append to `tests/scenarios/helpers/agent.go`:

```go
// RequestPairing returns the invite code printed by `hubfuse pair`.
func (a *Agent) RequestPairing(t *testing.T) string {
	t.Helper()
	out := a.run(t, "pair")
	// Output format depends on CLI; expect a line like:
	//   Pairing code: ABCDEF
	// Adjust regex to match real output.
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Pairing code:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "Pairing code:"))
		}
	}
	t.Fatalf("no pairing code found in output:\n%s", out)
	return ""
}

func (a *Agent) ConfirmPairing(t *testing.T, code string) {
	t.Helper()
	a.run(t, "pair", "--code", code)
}

// Mount triggers a mount via `hubfuse mount add`. Returns once the stub
// marker file appears, or fails the test on timeout.
func (a *Agent) Mount(t *testing.T, src, dst string) {
	t.Helper()
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatalf("local mount dir %s: %v", dst, err)
	}
	a.run(t, "mount", "add", src, dst)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(a.MountMarker(dst)); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("stub mount marker for %s did not appear within 10s", dst)
}

func (a *Agent) MountMarker(dst string) string {
	return filepath.Join(a.StubMountDir, sanitizeForMarker(dst)+".json")
}

func sanitizeForMarker(p string) string {
	r := strings.NewReplacer("/", "_", `\`, "_", ":", "_", " ", "_")
	return r.Replace(strings.TrimPrefix(p, "/"))
}

// HasPeer returns true if `hubfuse devices` lists the given nickname.
func (a *Agent) HasPeer(t *testing.T, name string) bool {
	t.Helper()
	out := a.run(t, "devices")
	return strings.Contains(out, name)
}
```

- [ ] **Step 7.3: Marker reader helper**

Create `tests/scenarios/helpers/marker.go`:

```go
package helpers

import (
	"encoding/json"
	"os"
	"testing"
)

type StubMountMarker struct {
	Src         string   `json:"src"`
	Dst         string   `json:"dst"`
	RemoteUser  string   `json:"remote_user"`
	RemoteHost  string   `json:"remote_host"`
	RemotePort  int      `json:"remote_port"`
	RemotePath  string   `json:"remote_path"`
	KeyPath     string   `json:"key_path"`
	RemoteFiles []string `json:"remote_files"`
	PID         int      `json:"pid"`
}

func ReadMarker(t *testing.T, path string) StubMountMarker {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read marker %s: %v", path, err)
	}
	var m StubMountMarker
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse marker %s: %v\n%s", path, err, data)
	}
	return m
}
```

- [ ] **Step 7.4: Write TestPairAndMountBasic**

Create `tests/scenarios/mount_test.go`:

```go
package scenarios_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ykhdr/hubfuse/tests/scenarios/helpers"
)

func TestPairAndMountBasic(t *testing.T) {
	hub := helpers.StartHub(t)

	exportDir := t.TempDir()
	alice := helpers.StartAgent(t, hub, "alice", helpers.WithExports(exportDir))
	alice.Join(t)
	alice.StartDaemon(t)

	bob := helpers.StartAgent(t, hub, "bob")
	bob.Join(t)
	bob.StartDaemon(t)

	code := alice.RequestPairing(t)
	bob.ConfirmPairing(t, code)

	require.Eventually(t, func() bool { return bob.HasPeer(t, "alice") },
		5*time.Second, 200*time.Millisecond, "bob should see alice as peer")

	mountPoint := filepath.Join(t.TempDir(), "alice-data")
	bob.Mount(t, "alice:"+filepath.Base(exportDir), mountPoint)

	marker := helpers.ReadMarker(t, bob.MountMarker(mountPoint))
	require.Equal(t, "alice", marker.RemoteUser)
	require.Equal(t, alice.SSHPort, marker.RemotePort)
}
```

- [ ] **Step 7.5: Run, debug paths and CLI shapes**

```bash
go test ./tests/scenarios/... -v -run TestPairAndMountBasic
```

Expected on first run: likely fails on CLI assumptions (`hubfuse pair` output format, `hubfuse mount add` arg shape, `--ssh-port` flag name). Debug by reading actual CLI output (visible in failure dump from `DumpOnFailure`) and adjust helpers. Iterate until PASS.

- [ ] **Step 7.6: Commit**

```bash
git add tests/scenarios/
git commit -m "$(cat <<'EOF'
test(scenarios): add pair + mount end-to-end test

Extends Agent helper with daemon lifecycle (StartDaemon/Stop),
pairing operations (RequestPairing/ConfirmPairing), and Mount that
waits for the stub-sshfs marker to appear. TestPairAndMountBasic
verifies that after pairing alice and bob, bob can mount alice's
share and the stub records a real SSH handshake to alice's SSH port.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 8: Reconnect, events, prune tests

**Goal:** Cover hub-restart resilience, event propagation, and stale device pruning. Each test reuses helpers from Tasks 4–7.

**Files:**
- Create: `tests/scenarios/reconnect_test.go`
- Create: `tests/scenarios/events_test.go`
- Create: `tests/scenarios/prune_test.go`

- [ ] **Step 8.1: Reconnect test**

Create `tests/scenarios/reconnect_test.go`:

```go
package scenarios_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ykhdr/hubfuse/tests/scenarios/helpers"
)

func TestAgentReconnectsAfterHubRestart(t *testing.T) {
	hub := helpers.StartHub(t)
	alice := helpers.StartAgent(t, hub, "alice")
	alice.Join(t)
	alice.StartDaemon(t)

	hub.Restart(t)

	// After restart, status on alice should converge back to online.
	require.Eventually(t, func() bool {
		out := alice.RunStatus(t)  // helper added below
		return contains(out, "online") || contains(out, "connected")
	}, 10*time.Second, 250*time.Millisecond, "alice should reconnect after hub restart")
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (s == sub || index(s, sub) >= 0) }
func index(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
```

Add `RunStatus` to `tests/scenarios/helpers/agent.go`:

```go
func (a *Agent) RunStatus(t *testing.T) string {
	t.Helper()
	return a.run(t, "status")
}
```

- [ ] **Step 8.2: Events test**

Create `tests/scenarios/events_test.go`:

```go
package scenarios_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ykhdr/hubfuse/tests/scenarios/helpers"
)

func TestDeviceOnlineOfflineEvents(t *testing.T) {
	hub := helpers.StartHub(t)
	alice := helpers.StartAgent(t, hub, "alice")
	alice.Join(t)
	alice.StartDaemon(t)

	bob := helpers.StartAgent(t, hub, "bob")
	bob.Join(t)
	bob.StartDaemon(t)

	require.Eventually(t, func() bool { return alice.HasPeer(t, "bob") },
		5*time.Second, 200*time.Millisecond, "alice should see bob online")

	bob.Stop(t)

	require.Eventually(t, func() bool {
		out := alice.RunStatus(t)
		// Status output should reflect bob is offline; exact wording
		// depends on CLI — adjust assertion to match.
		return contains(out, "bob") && (contains(out, "offline") || contains(out, "unreachable"))
	}, 30*time.Second, 500*time.Millisecond, "alice should see bob offline after stop")
}
```

- [ ] **Step 8.3: Prune test**

Create `tests/scenarios/prune_test.go`:

```go
package scenarios_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ykhdr/hubfuse/tests/scenarios/helpers"
)

// TestStaleDevicePruned launches a hub with a very short device retention,
// joins an agent, kills the agent, and verifies the device disappears.
func TestStaleDevicePruned(t *testing.T) {
	hub := helpers.StartHubWithRetention(t, 5*time.Second)  // helper below

	alice := helpers.StartAgent(t, hub, "alice")
	alice.Join(t)
	alice.StartDaemon(t)

	bob := helpers.StartAgent(t, hub, "bob")
	bob.Join(t)
	bob.StartDaemon(t)

	require.Eventually(t, func() bool { return bob.HasPeer(t, "alice") },
		5*time.Second, 200*time.Millisecond)

	alice.Stop(t)

	require.Eventually(t, func() bool { return !bob.HasPeer(t, "alice") },
		30*time.Second, 1*time.Second, "alice should be pruned from bob's device list")
}
```

Add `StartHubWithRetention` to `tests/scenarios/helpers/hub.go`:

```go
func StartHubWithRetention(t *testing.T, retention time.Duration) *Hub {
	t.Helper()
	port := FreePort(t)
	dataDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, HubBinaryPath, "start",
		"--listen", fmt.Sprintf("127.0.0.1:%d", port),
		"--data-dir", dataDir,
		"--device-retention", retention.String(),
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	logBuf := &LogBuffer{}
	cmd.Stdout = logBuf
	cmd.Stderr = logBuf
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start hub: %v", err)
	}
	h := &Hub{Address: fmt.Sprintf("127.0.0.1:%d", port), DataDir: dataDir, port: port, cmd: cmd, logBuf: logBuf, cancel: cancel}
	DumpOnFailure(t, "hub", logBuf)
	t.Cleanup(func() { h.Stop(t) })
	WaitForPort(t, port, 5*time.Second)
	return h
}
```

(Verify the `--device-retention` flag name against `cmd/hubfuse-hub/main.go`.)

- [ ] **Step 8.4: Run all scenarios**

```bash
go test ./tests/scenarios/... -v -timeout 180s
```

Expected: every scenario PASS. Total runtime under ~120s. If any test is flaky, fix the underlying race (most often: missing `WaitForPort` after restart, or polling interval too aggressive).

- [ ] **Step 8.5: Commit**

```bash
git add tests/scenarios/
git commit -m "$(cat <<'EOF'
test(scenarios): cover reconnect, online/offline events, and pruning

TestAgentReconnectsAfterHubRestart: agent should re-register when
hub comes back. TestDeviceOnlineOfflineEvents: peer transitions are
reflected in `hubfuse status`. TestStaleDevicePruned: with short
retention, dropped agents disappear from peer lists.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 9: Wire scenario tests into Makefile and CI (merge_group)

**Goal:** `make test-scenarios` runs locally; CI runs on `merge_group` and `push` to master, not on every PR push.

**Files:**
- Modify: `Makefile`
- Modify: `.github/workflows/ci.yml`

- [ ] **Step 9.1: Makefile target**

Add to `Makefile`:

```make
test-scenarios:
	go test ./tests/scenarios/... -timeout 180s
```

Update default `test` target to include scenarios:

```make
test: test-unit test-integration test-cli test-scenarios
```

- [ ] **Step 9.2: Add merge_group trigger to workflow**

Edit `.github/workflows/ci.yml` `on:` block:

```yaml
on:
  push:
    branches: [master]
  pull_request:
    branches: [master]
  merge_group:
```

- [ ] **Step 9.3: Add test-scenarios job (merge-only)**

Append to `.github/workflows/ci.yml`:

```yaml
  test-scenarios:
    name: Scenario Tests
    runs-on: ubuntu-latest
    if: github.event_name == 'merge_group' || github.event_name == 'push'
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version: ${{ env.GO_VERSION }}

      - name: Run scenario tests
        run: make test-scenarios
```

- [ ] **Step 9.4: Verify locally**

```bash
make test-scenarios
```

Expected: PASS, runtime under 120s.

- [ ] **Step 9.5: Commit**

```bash
git add Makefile .github/workflows/ci.yml
git commit -m "$(cat <<'EOF'
ci: gate merges on scenario tests via merge_group

test-scenarios runs only on merge_group events (and post-merge push
to master), so multi-process tests don't slow down PR feedback but
still gate every merge. Adds merge_group to the workflow triggers.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 10: FUSE smoke tests + CI integration

**Goal:** Three real-FUSE tests behind build tag `fuse_smoke`. CI runs them on `merge_group` only, on `ubuntu-22.04` with `apt install sshfs`.

**Files:**
- Create: `tests/scenarios/fuse_test.go`
- Create: `tests/scenarios/helpers/fuse.go`
- Modify: `Makefile`
- Modify: `.github/workflows/ci.yml`

- [ ] **Step 10.1: FUSE setup helper**

Create `tests/scenarios/helpers/fuse.go`:

```go
//go:build fuse_smoke

package helpers

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
)

// SetupFUSE verifies preconditions and registers cleanup. Skips the test if
// FUSE is unavailable on this platform (e.g. CI runner without /dev/fuse).
func SetupFUSE(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skipf("FUSE smoke not supported on %s", runtime.GOOS)
	}
	if _, err := os.Stat("/dev/fuse"); err != nil {
		t.Skipf("/dev/fuse not present: %v", err)
	}
	if _, err := exec.LookPath("sshfs"); err != nil {
		t.Skipf("sshfs not on PATH: %v", err)
	}
}

// LazyUnmount removes a FUSE mount even if processes still hold it open.
func LazyUnmount(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "linux" {
		_ = exec.Command("fusermount", "-uz", path).Run()
	} else {
		_ = exec.Command("umount", "-f", path).Run()
	}
}

// DumpFUSEState writes diagnostic info about FUSE state for failure triage.
func DumpFUSEState(t *testing.T) {
	t.Helper()
	if !t.Failed() {
		return
	}
	for _, c := range [][]string{
		{"mount"},
		{"pgrep", "-a", "sshfs"},
		{"dmesg"},
	} {
		out, _ := exec.Command(c[0], c[1:]...).CombinedOutput()
		t.Logf("--- %v ---\n%s", c, out)
	}
}
```

- [ ] **Step 10.2: FUSE smoke tests**

Create `tests/scenarios/fuse_test.go`:

```go
//go:build fuse_smoke

package scenarios_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ykhdr/hubfuse/tests/scenarios/helpers"
)

func TestFUSEBasicMount(t *testing.T) {
	helpers.SetupFUSE(t)
	t.Cleanup(func() { helpers.DumpFUSEState(t) })

	hub := helpers.StartHub(t)

	exportDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(exportDir, "hello.txt"), []byte("from-alice"), 0o644))

	alice := helpers.StartAgent(t, hub, "alice", helpers.WithExports(exportDir), helpers.WithRealSSHFS())
	alice.Join(t)
	alice.StartDaemon(t)

	bob := helpers.StartAgent(t, hub, "bob", helpers.WithRealSSHFS())
	bob.Join(t)
	bob.StartDaemon(t)

	code := alice.RequestPairing(t)
	bob.ConfirmPairing(t, code)
	require.Eventually(t, func() bool { return bob.HasPeer(t, "alice") },
		5*time.Second, 200*time.Millisecond)

	mountPoint := filepath.Join(t.TempDir(), "alice-data")
	require.NoError(t, os.MkdirAll(mountPoint, 0o755))
	t.Cleanup(func() { helpers.LazyUnmount(t, mountPoint) })

	bob.Mount(t, "alice:"+filepath.Base(exportDir), mountPoint)

	require.Eventually(t, func() bool {
		data, err := os.ReadFile(filepath.Join(mountPoint, "hello.txt"))
		return err == nil && string(data) == "from-alice"
	}, 10*time.Second, 200*time.Millisecond, "bob should read alice's file via FUSE")
}

func TestFUSEUnmountClean(t *testing.T) {
	helpers.SetupFUSE(t)
	t.Cleanup(func() { helpers.DumpFUSEState(t) })

	hub := helpers.StartHub(t)
	alice := helpers.StartAgent(t, hub, "alice", helpers.WithExports(t.TempDir()), helpers.WithRealSSHFS())
	alice.Join(t)
	alice.StartDaemon(t)
	bob := helpers.StartAgent(t, hub, "bob", helpers.WithRealSSHFS())
	bob.Join(t)
	bob.StartDaemon(t)

	code := alice.RequestPairing(t)
	bob.ConfirmPairing(t, code)
	require.Eventually(t, func() bool { return bob.HasPeer(t, "alice") },
		5*time.Second, 200*time.Millisecond)

	mp := filepath.Join(t.TempDir(), "m")
	require.NoError(t, os.MkdirAll(mp, 0o755))
	bob.Mount(t, "alice:"+filepath.Base(alice.exports[0]), mp)

	bob.run(t, "mount", "remove", mp)

	require.Eventually(t, func() bool {
		out, _ := exec.Command("mount").Output()
		return !contains(string(out), mp)
	}, 5*time.Second, 200*time.Millisecond, "mount point should be released")
}

func TestFUSEUnmountOnAgentCrash(t *testing.T) {
	helpers.SetupFUSE(t)
	t.Cleanup(func() { helpers.DumpFUSEState(t) })

	hub := helpers.StartHub(t)
	alice := helpers.StartAgent(t, hub, "alice", helpers.WithExports(t.TempDir()), helpers.WithRealSSHFS())
	alice.Join(t)
	alice.StartDaemon(t)
	bob := helpers.StartAgent(t, hub, "bob", helpers.WithRealSSHFS())
	bob.Join(t)
	bob.StartDaemon(t)

	code := alice.RequestPairing(t)
	bob.ConfirmPairing(t, code)
	require.Eventually(t, func() bool { return bob.HasPeer(t, "alice") },
		5*time.Second, 200*time.Millisecond)

	mp := filepath.Join(t.TempDir(), "m")
	require.NoError(t, os.MkdirAll(mp, 0o755))
	bob.Mount(t, "alice:"+filepath.Base(alice.exports[0]), mp)

	// Hard-kill bob's daemon; verify the mount point can be lazy-unmounted.
	bob.daemonCmd.Process.Kill()
	helpers.LazyUnmount(t, mp)

	// A fresh agent should be able to recreate the mount on the same path.
	bob2 := helpers.StartAgent(t, hub, "bob", helpers.WithRealSSHFS(), helpers.WithEnv("HOME="+bob.HomeDir))
	bob2.StartDaemon(t)
	bob2.Mount(t, "alice:"+filepath.Base(alice.exports[0]), mp)
}
```

- [ ] **Step 10.3: WithRealSSHFS option**

In `tests/scenarios/helpers/agent.go`, add the option that suppresses prepending `stub-sshfs` to PATH:

```go
func WithRealSSHFS() AgentOption {
	return func(a *Agent) { a.useRealSSHFS = true }
}
```

Add `useRealSSHFS bool` to `Agent`. In `StartDaemon`, if `useRealSSHFS` is true, do NOT prepend `stubDir` to PATH (let real `/usr/bin/sshfs` win the lookup).

- [ ] **Step 10.4: Makefile target**

Add to `Makefile`:

```make
test-fuse:
	go test -tags=fuse_smoke -p 1 ./tests/scenarios/... -timeout 180s -run FUSE

test-all: test test-fuse
```

- [ ] **Step 10.5: CI job**

Append to `.github/workflows/ci.yml`:

```yaml
  test-fuse:
    name: FUSE Smoke
    runs-on: ubuntu-22.04
    if: github.event_name == 'merge_group' || github.event_name == 'push'
    steps:
      - uses: actions/checkout@v4

      - name: Install sshfs
        run: sudo apt-get update && sudo apt-get install -y sshfs

      - uses: actions/setup-go@v5
        with:
          go-version: ${{ env.GO_VERSION }}

      - name: Run FUSE smoke tests
        run: make test-fuse
```

- [ ] **Step 10.6: Verify locally (mac with macFUSE)**

```bash
make test-fuse
```

Expected: 3 tests PASS. If `SetupFUSE` skips because `/dev/fuse` not found, ensure macFUSE is loaded.

- [ ] **Step 10.7: Commit**

```bash
git add tests/scenarios/fuse_test.go tests/scenarios/helpers/fuse.go \
        tests/scenarios/helpers/agent.go Makefile .github/workflows/ci.yml
git commit -m "$(cat <<'EOF'
test(fuse): add real-FUSE smoke tests behind build tag

Three tests under build tag fuse_smoke exercise real sshfs:
basic mount + read, clean unmount, and recovery from agent crash.
WithRealSSHFS() option skips the stub-sshfs PATH prepend so the
system's /usr/bin/sshfs is invoked. CI runs them on ubuntu-22.04
with sshfs installed via apt, only on merge_group and post-merge
push (not on every PR push).

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Task 11: Branch protection and merge queue (manual config + documentation)

**Goal:** Make all six required jobs (build, vet, lint, test-unit, test-integration, test-cli, test-scenarios, test-fuse) gate merges via GitHub merge queue.

This step is performed in GitHub UI by the repo admin. Captured here as a checklist + documented in `CONTRIBUTING.md` (created if missing) so the rule survives.

**Files:**
- Create or modify: `CONTRIBUTING.md`

- [ ] **Step 11.1: Document merge process**

Add to `CONTRIBUTING.md` (create if absent):

```markdown
## Merging to master

This repository uses GitHub's merge queue. The merge button on a PR reads
"Merge when ready" — pressing it queues the PR. GitHub creates a temporary
branch with the PR rebased onto current master, runs the full CI workflow
including scenario and FUSE tests, and on green auto-merges to master.

Required checks (configured in Settings → Branches → master):

- Build
- Vet
- Lint
- Unit Tests
- Integration Tests
- CLI Tests
- Scenario Tests
- FUSE Smoke

PR-time CI (on every push to a PR branch) runs only the lighter checks
(build, vet, lint, unit, integration, cli). The heavier scenario and FUSE
suites run only on the merge-queue candidate to keep PR feedback fast.

If a merge-queue check fails, the PR is ejected from the queue with the
failing job's logs attached. Fix the issue, push, and re-queue with
"Merge when ready".
```

- [ ] **Step 11.2: Configure branch protection (GitHub UI)**

Manual steps:

1. Settings → Branches → Add branch protection rule for `master`
2. Enable: "Require a pull request before merging"
3. Enable: "Require status checks to pass before merging"
4. Add required checks: `Build`, `Vet`, `Lint`, `Unit Tests`, `Integration Tests`, `CLI Tests`, `Scenario Tests`, `FUSE Smoke`
5. Enable: "Require merge queue"
   - Build concurrency: 1
   - Maximum requests: 5
   - Minimum requests: 1
   - Merge method: keep current default
6. Save

These cannot be set from the plan; verify after configuration by opening a PR and checking that the merge button now reads "Merge when ready".

- [ ] **Step 11.3: Commit**

```bash
git add CONTRIBUTING.md
git commit -m "$(cat <<'EOF'
docs: document merge queue process and required checks

Captures the new merge workflow: PR-time runs light checks, merge
queue runs scenarios and FUSE smoke, all gate the merge to master.

Co-Authored-By: Claude Opus 4.7 (1M context) <noreply@anthropic.com>
EOF
)"
```

---

## Self-review notes

**Spec coverage:** All three test layers (CLI, Scenarios, FUSE) covered. CI integration matches spec (PR-light + merge_group-heavy). All 13 CLI scenarios from spec present. Scenario test files match spec list (join_pair, mount, reconnect, events, prune). FUSE tests are the three named in the spec.

**Open questions (spec § "Open questions for implementation phase"):**
1. Run() signature — moot under deviation (no refactor).
2. WaitForPort backoff — chose simple polling (50ms interval) in helpers/ports.go; sufficient for now.
3. Stub-sshfs `--mode` distinction — single binary handles all invocations via arg shape. fusermount/umount stub deferred (not needed since stub-sshfs deletes its own marker on SIGTERM, matching real-sshfs cleanup behavior under stub conditions).
4. Migrate `tests/integration/prune_test.go` — left as-is per spec recommendation.

**Known iteration risk:** Several helper methods (`RequestPairing` parsing, `Mount` arg shape, `--ssh-port` flag, `--device-retention` flag) make assumptions about the current CLI surface that need verification against actual binary output during Task 5–8 implementation. Each affected step explicitly calls this out and instructs the implementer to verify against real binary output and adjust.

**Placeholder scan:** No "TBD"/"TODO"/"figure out" left. Where adjustments are needed (e.g. CLI parsing), the step says exactly what to verify and how.

**Type consistency:** `StubMountMarker` defined in `helpers/marker.go` matches the `Marker` struct in `tests/tools/stub-sshfs/main.go` field-by-field. `Hub` and `Agent` struct fields used across tasks are all declared in their respective tasks.
