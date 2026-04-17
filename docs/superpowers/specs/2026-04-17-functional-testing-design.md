# Functional testing strategy

**Date:** 2026-04-17
**Status:** Approved (ready for implementation plan)
**Motivation:** Issue #20 regression — usage block stopped appearing for malformed CLI commands like `hubfuse join` (no args). The bug slipped through because no test exercises the compiled binary as a process. We need a testing layer that catches this class of bug — and the broader class of multi-process behaviour bugs that integration tests also miss.

## Goals

1. **Catch CLI-contract regressions** before they reach `master` — output formatting, exit codes, usage visibility, error message text.
2. **Catch multi-process behaviour regressions** — pairing, mounting, reconnection, event propagation across hub + two agents.
3. **Validate the real SSH path** (embedded SSH server + key exchange + sftp) end-to-end without depending on FUSE infrastructure.
4. **Validate real FUSE end-to-end** before any merge to master, accepting the operational cost.

## Non-goals

- Full pytest-style fixture system — we use the standard `testing` package.
- Performance / load testing.
- Cross-platform CI (macOS CI for FUSE is impractical due to macFUSE installation flow; local mac dev is supported instead).
- Mutation testing, fuzzing.

## Architecture overview

Three test layers, each with a distinct purpose and execution model:

| Layer | Purpose | Tool | Execution |
|---|---|---|---|
| **CLI contracts (A)** | One command in, output + exit code out | `testscript` (`rogpeppe/go-internal`) | In-process — registered `Run()` function |
| **Scenarios (C)** | Multi-process flows with real SSH | Plain Go `_test.go` + `os/exec` | Real subprocesses, real network on loopback, stub `sshfs` (real SSH, no FUSE) |
| **FUSE smoke** | Real `sshfs` end-to-end | Plain Go `_test.go` + `os/exec`, build tag `fuse_smoke` | Real subprocesses, real `sshfs` from `apt`, real FUSE kernel module |

Existing layers (unit tests in `internal/`, integration tests in `tests/integration/`) are untouched.

## Repository layout

```
hubfuse/
├── cmd/
│   ├── hubfuse/
│   │   ├── main.go              # thin: os.Exit(cli.Run())
│   │   └── cli/
│   │       └── cli.go           # Run() int — current main() body lives here
│   └── hubfuse-hub/
│       ├── main.go              # thin: os.Exit(cli.Run())
│       └── cli/
│           └── cli.go           # Run() int
├── tests/
│   ├── integration/             # unchanged
│   ├── cli/                     # NEW
│   │   ├── cli_test.go          # TestMain registers binaries, TestCLI runs testscript.Run
│   │   └── testdata/
│   │       └── *.txtar          # one file per CLI scenario
│   ├── scenarios/               # NEW
│   │   ├── helpers/
│   │   │   ├── hub.go           # StartHub, Stop, Restart
│   │   │   ├── agent.go         # StartAgent + high-level ops (Join, Pair, Mount, Devices, ...)
│   │   │   ├── ports.go         # FreePort, WaitForPort
│   │   │   └── workdir.go       # isolated HOME, log buffers, t.Cleanup wiring
│   │   ├── main_test.go         # TestMain — go build all needed binaries once
│   │   ├── join_pair_test.go
│   │   ├── mount_test.go
│   │   ├── reconnect_test.go
│   │   ├── events_test.go
│   │   ├── prune_test.go
│   │   └── fuse_test.go         # build tag fuse_smoke — real FUSE tests
│   └── tools/
│       └── stub-sshfs/
│           └── main.go          # fake sshfs binary: real ssh.Dial + sftp.ReadDir, no FUSE
├── docs/
│   └── superpowers/
│       └── specs/
│           └── 2026-04-17-functional-testing-design.md   # this file
├── Makefile                     # extended (see below)
└── .github/
    └── workflows/
        └── ci.yml               # extended (see below)
```

## Layer 1: CLI contracts (A)

### Production code change

The minimum invasive change to enable in-process binary registration in `testscript`:

```go
// cmd/hubfuse/main.go (after change — 4 lines)
package main

import (
    "os"
    "github.com/ykhdr/hubfuse/cmd/hubfuse/cli"
)

func main() {
    os.Exit(cli.Run())
}
```

```go
// cmd/hubfuse/cli/cli.go (was the body of func main())
package cli

func Run() int {
    rootCmd := newRootCmd()
    if err := rootCmd.Execute(); err != nil {
        return 1
    }
    return 0
}
```

Same shape for `cmd/hubfuse-hub`. No business logic moves; this is pure refactor. All existing imports/symbols inside `cli` package adjust namespaces accordingly.

### Test harness

```go
// tests/cli/cli_test.go
package cli_test

import (
    "os"
    "testing"

    "github.com/rogpeppe/go-internal/testscript"

    hubfuse "github.com/ykhdr/hubfuse/cmd/hubfuse/cli"
    hub "github.com/ykhdr/hubfuse/cmd/hubfuse-hub/cli"
)

func TestMain(m *testing.M) {
    os.Exit(testscript.RunMain(m, map[string]func() int{
        "hubfuse":     hubfuse.Run,
        "hubfuse-hub": hub.Run,
    }))
}

func TestCLI(t *testing.T) {
    testscript.Run(t, testscript.Params{
        Dir: "testdata",
    })
}
```

### Initial scenario set

Each file is a self-contained `.txtar` scenario. New regression scenarios are added as `.txtar` files; no Go code changes required.

| File | What it asserts |
|---|---|
| `help_top_level.txtar` | `hubfuse --help` lists all expected subcommands |
| `version.txtar` | `hubfuse --version` prints version string |
| `unknown_command.txtar` | unknown subcommand → error + Cobra suggestion |
| `join_no_args.txtar` | **issue #20 regression**: error AND usage block both shown |
| `join_too_many_args.txtar` | error AND usage block |
| `join_unreachable.txtar` | hub down → friendly "cannot reach hub" message, NO usage block |
| `pair_no_args.txtar` | error AND usage |
| `pair_invalid_code.txtar` | malformed code → friendly error |
| `rename_no_args.txtar` | error AND usage |
| `mount_bad_format.txtar` | malformed `device:share` → friendly error |
| `status_not_joined.txtar` | "not joined to hub" message, NO usage block |
| `hub_help.txtar` | `hubfuse-hub --help` lists subcommands |
| `hub_invalid_config.txtar` | malformed KDL → human-readable parse error |

Example file:

```
# tests/cli/testdata/join_no_args.txtar
! hubfuse join
stderr 'error: accepts 1 arg\(s\), received 0'
stderr 'Usage:'
stderr 'hubfuse join <hub-address>'
```

```
# tests/cli/testdata/join_unreachable.txtar
! hubfuse join 127.0.0.1:1
stderr 'error: cannot reach hub'
! stderr 'Usage:'
```

Hub is generally not started for CLI tests — they verify CLI-side behaviour. Where hub state is needed, it belongs in the scenarios layer instead.

### Dependency added

- `github.com/rogpeppe/go-internal` (testscript only). Lightweight, mature, no transitive concerns.

## Layer 2: Scenarios (C)

### Helper API

```go
// tests/scenarios/helpers/hub.go
type Hub struct {
    Address  string  // "127.0.0.1:NNNN"
    StateDir string
    // unexported: cmd, logBuf, cancel
}

type HubOption func(*hubConfig)

func StartHub(t *testing.T, opts ...HubOption) *Hub
func (h *Hub) Stop(t *testing.T)
func (h *Hub) Restart(t *testing.T)
```

```go
// tests/scenarios/helpers/agent.go
type Agent struct {
    Nickname string
    HomeDir  string  // isolated $HOME for configs/keys/state
    SSHPort  int
    // unexported: cmd, logBuf, hub *Hub, stubMountDir string
}

type AgentOption func(*agentConfig)

func WithExports(paths ...string) AgentOption
func WithSSHPort(port int) AgentOption
func WithConfig(path string) AgentOption
func WithEnv(kv ...string) AgentOption

func StartAgent(t *testing.T, hub *Hub, nickname string, opts ...AgentOption) *Agent

// High-level operations — wrappers over exec.Command(hubfuseBinary, ...)
func (a *Agent) Join(t *testing.T)
func (a *Agent) RequestPairing(t *testing.T) string             // returns invite code
func (a *Agent) ConfirmPairing(t *testing.T, code string)
func (a *Agent) Mount(t *testing.T, src, dst string)
func (a *Agent) Unmount(t *testing.T, dst string)
func (a *Agent) Devices(t *testing.T) []DeviceInfo
func (a *Agent) Status(t *testing.T) StatusInfo
func (a *Agent) Rename(t *testing.T, newNickname string)
func (a *Agent) HasPeer(name string) bool
func (a *Agent) MountMarker(src string) string                  // path to JSON marker written by stub-sshfs
```

### Process orchestration

`TestMain` builds all required binaries once into a temp directory:

```go
// tests/scenarios/main_test.go
var (
    hubBinary       string
    agentBinary     string
    stubSSHFSBinary string
    binDir          string
)

func TestMain(m *testing.M) {
    var err error
    binDir, err = os.MkdirTemp("", "hubfuse-scenarios-bin")
    if err != nil { panic(err) }
    defer os.RemoveAll(binDir)

    builds := []struct{ out, pkg string }{
        {"hubfuse-hub", "./cmd/hubfuse-hub"},
        {"hubfuse",     "./cmd/hubfuse"},
        {"sshfs",       "./tests/tools/stub-sshfs"},  // named sshfs to land on PATH
    }
    for _, b := range builds {
        out := filepath.Join(binDir, b.out)
        cmd := exec.Command("go", "build", "-o", out, b.pkg)
        cmd.Dir = repoRoot()
        if out, err := cmd.CombinedOutput(); err != nil {
            fmt.Fprintf(os.Stderr, "build %s failed: %v\n%s", b.pkg, err, out)
            os.Exit(1)
        }
    }
    hubBinary       = filepath.Join(binDir, "hubfuse-hub")
    agentBinary     = filepath.Join(binDir, "hubfuse")
    stubSSHFSBinary = filepath.Join(binDir, "sshfs")

    os.Exit(m.Run())
}
```

Each test gets `t.TempDir()` for HOME directories and free ports via `helpers.FreePort(t)`. Subprocess stdout/stderr captured into `bytes.Buffer`; on `t.Failed()`, helpers dump them with prefixes like `[hub]`, `[alice]`, `[bob]`.

`stub-sshfs` is placed on each agent's `PATH` by prepending `binDir` to `PATH` in the agent's environment. The agent's mounter finds `sshfs` and invokes it as if it were the real binary.

### stub-sshfs binary

Lives at `tests/tools/stub-sshfs/main.go`. Behavior:

1. Parse same arg shape as real sshfs: `user@host:/remote/path /local/mount-point -p PORT -o IdentityFile=/path/to/key -o ...`
2. Establish a real SSH connection via `golang.org/x/crypto/ssh` (Dial, key exchange, auth via the provided IdentityFile).
3. Open an SFTP channel via `github.com/pkg/sftp` and `ReadDir(/remote/path)` — proves the export is reachable.
4. Write a JSON marker file to `$HUBFUSE_STUB_MOUNT_DIR/<sanitized-mount-path>.json`:
   ```json
   {
     "src": "alice@127.0.0.1:2300:/data",
     "dst": "/local/data",
     "key": "/path/to/key",
     "remote_files": ["a.txt", "b.txt"],
     "pid": 12345
   }
   ```
5. Stay alive (like real sshfs daemonizes — emulate by blocking on a context) until SIGTERM or the SSH connection drops.
6. On exit, close the SSH session and delete the marker file.

A second small stub (`tests/tools/stub-fusermount/main.go` or via a flag in `stub-sshfs`) replaces `fusermount` / `umount`: simply removes the marker file.

### Dependencies added

- `golang.org/x/crypto/ssh` (already in indirect tree via gRPC TLS chain — verify)
- `github.com/pkg/sftp` (new, mature, MIT)

### Initial scenario set

| File | Tests |
|---|---|
| `join_pair_test.go` | `TestJoinPersistsCert`, `TestJoinNicknameCollision` |
| `mount_test.go` | `TestPairAndMountBasic`, `TestMountSurvivesAgentRestart`, `TestUnmountClean` |
| `reconnect_test.go` | `TestAgentReconnectsAfterHubRestart`, `TestPairingsSurviveHubRestart` |
| `events_test.go` | `TestDeviceOnlineOfflineEvents`, `TestSharesUpdatedEvent` |
| `prune_test.go` | `TestStaleDevicePruned`, `TestPruneTriggersUnmount` |

Example test shape:

```go
func TestPairAndMountBasic(t *testing.T) {
    hub := helpers.StartHub(t)
    alice := helpers.StartAgent(t, hub, "alice", helpers.WithExports("/data"))
    bob := helpers.StartAgent(t, hub, "bob")

    code := alice.RequestPairing(t)
    bob.ConfirmPairing(t, code)

    require.Eventually(t, func() bool { return bob.HasPeer("alice") },
        5*time.Second, 100*time.Millisecond)

    bob.Mount(t, "alice:data", "/local/data")

    marker := bob.MountMarker("alice:data")
    require.FileExists(t, marker)

    var m StubMountMarker
    readJSON(t, marker, &m)
    require.Equal(t, "alice", m.RemoteUser)
    require.Equal(t, alice.SSHPort, m.RemotePort)
}
```

## Layer 3: FUSE smoke tests

### Scope

Lives in `tests/scenarios/fuse_test.go` under build tag `fuse_smoke`. NOT triggered by the default `make test`. Triggered by `make test-fuse` and the `test-fuse` CI job.

Three tests, intentionally limited:

| Test | What it proves |
|---|---|
| `TestFUSEBasicMount` | join → pair → mount → write file on B → read on A → matches |
| `TestFUSEUnmountClean` | clean unmount via `hubfuse umount` releases the mount point |
| `TestFUSEUnmountOnAgentCrash` | `kill -9` agent → next agent start can re-acquire the mount point (no zombie mount) |

These tests use the **real** `/usr/bin/sshfs`, not the stub. They do NOT prepend `stub-sshfs` to PATH.

### Anti-flake measures (implemented from day one)

1. **No parallelism for FUSE tests.** No `t.Parallel()`. `make test-fuse` runs `go test -p 1`.
2. **`require.Eventually` around every FUSE-touching operation** — mount readiness, file visibility on the other side, unmount completion. Per-operation retries, not whole-test retries.
3. **Aggressive cleanup helper** `setupFUSE(t)`:
   - Pre-flight: verify `/dev/fuse` accessible, `sshfs` on PATH, mount target free (lazy `fusermount -uz` if not).
   - `t.Cleanup`: guaranteed `fusermount -uz` (lazy) on the mount point + `os.RemoveAll`.
4. **Pinned versions in CI:** `runs-on: ubuntu-22.04` (not `ubuntu-latest`); pin `sshfs` apt version once a stable one is identified.
5. **System state dump on failure:** `t.Cleanup` for FUSE tests, when `t.Failed()`, additionally dumps:
   - `mount | grep fuse`
   - `pgrep -a sshfs`
   - `dmesg | tail -50` (if accessible)
6. **Workflow-level `fail-fast: false`** so a FUSE flake doesn't cancel `test-scenarios` and confuse triage.

If a flake appears in practice, the response is to fix the underlying race or harden the helper — not to add test-level retries or move the test back outside the gate.

## Makefile

```make
test:           test-unit test-integration test-cli test-scenarios
test-unit:        ; go test ./internal/...
test-integration: ; go test ./tests/integration/... -timeout 120s
test-cli:         ; go test ./tests/cli/...
test-scenarios:   ; go test ./tests/scenarios/... -timeout 180s
test-fuse:        ; go test -tags=fuse_smoke -p 1 ./tests/scenarios/... -timeout 180s -run FUSE
test-all:       test test-fuse
```

`make test` deliberately excludes FUSE so it works on a clean macOS dev machine without macFUSE. `test-fuse` is the explicit opt-in for local FUSE runs (works on the dev's mac since macFUSE is configured) and is what CI runs in the dedicated job.

## CI integration

GitHub merge queue is the gating mechanism. Required checks block merge to `master`; light checks run on every PR push, heavy checks run only on the merge-queue candidate.

```yaml
# .github/workflows/ci.yml
on:
  pull_request:
  merge_group:
  push:
    branches: [master]

jobs:
  build:
    runs-on: ubuntu-22.04
    # unchanged

  test-unit:
    runs-on: ubuntu-22.04
    # unchanged

  test-integration:
    runs-on: ubuntu-22.04
    # unchanged

  test-cli:
    runs-on: ubuntu-22.04
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: '1.25' }
      - run: make test-cli

  test-scenarios:
    if: github.event_name == 'merge_group' || github.event_name == 'push'
    runs-on: ubuntu-22.04
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: '1.25' }
      - run: make test-scenarios

  test-fuse:
    if: github.event_name == 'merge_group' || github.event_name == 'push'
    runs-on: ubuntu-22.04
    steps:
      - uses: actions/checkout@v4
      - run: sudo apt-get update && sudo apt-get install -y sshfs
      - uses: actions/setup-go@v5
        with: { go-version: '1.25' }
      - run: make test-fuse
```

`fail-fast: false` is set at the workflow level so one flaky job doesn't cancel the others.

### Branch protection (Settings → Branches → master)

- Require a pull request before merging
- Require status checks to pass before merging
- Required checks: `build`, `test-unit`, `test-integration`, `test-cli`, `test-scenarios`, `test-fuse`
- Require merge queue:
  - Build concurrency: 1
  - Maximum requests: 5
  - Minimum requests: 1

After enabling merge queue, the merge button reads "Merge when ready". Pressing it adds the PR to the queue; GitHub creates a temp `gh-readonly-queue/master/...` branch with the PR rebased onto current master, runs the workflow with `merge_group` event, and on green auto-merges to master. On red, the PR is ejected with the failure context.

### Effective lifecycle

```
push to feature branch
  └─ build, test-unit, test-integration, test-cli   (~30–60s)

press "Merge when ready"
  └─ merge_group event:
     build, test-unit, test-integration, test-cli,
     test-scenarios (~30–60s), test-fuse (~30–60s)
  └─ all green → auto-merge to master
  └─ any red    → PR ejected, fix and re-queue
```

## Risks and trade-offs

| Risk | Mitigation |
|---|---|
| FUSE flakes block merges | Anti-flake measures listed above; flakes treated as bugs, not as reasons to weaken the gate |
| `sshfs` apt package changes break us | Pinned to `ubuntu-22.04`; package version pinned once stable |
| `cli/` package introduces import cycles | `cli` is leaf-level; only `main.go` depends on it |
| Test runtime grows over time | Scenarios use `t.Parallel()` where safe; FUSE explicitly serial |
| Local `make test` broken by FUSE deps on Mac | FUSE under build tag; default `make test` excludes it |
| Stub-sshfs drifts from real sshfs interface | Argument parsing matches the real sshfs flag set used by `mounter.go`; if mounter adds new flags, stub is updated alongside |

## What this design does NOT do

- Does not introduce a separate test runner, only standard `go test`.
- Does not require Docker for the default workflow.
- Does not change existing unit/integration tests.
- Does not introduce snapshot testing, golden-image tooling, or other novel infrastructure beyond `testscript` (which is widely used in the Go ecosystem).

## Open questions for implementation phase

These are intentionally deferred to the implementation plan, not decided here:

1. Exact CLI shape for `Run() int` — does it accept `args []string` for testability, or rely on `os.Args`? (Cobra defaults to the latter; testscript handles `os.Args` rewriting.)
2. Whether `helpers.WaitForPort` needs a backoff strategy beyond simple polling.
3. Whether stub-sshfs needs a `--mode=ls` vs `--mode=mount` distinction, or one binary handles both unmount and mount via arg detection.
4. Migration path: do we move existing `tests/integration/prune_test.go` (which already uses stub mounter) into the new structure, or leave it? Recommendation: leave it.
