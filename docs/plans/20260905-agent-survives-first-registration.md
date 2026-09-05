# Agent survives a failed first registration (#74, #98)

## Overview

The macOS agent dies if its very first hub registration fails. `Run` calls
`registerAndSubscribe`, which returns the first `sessionOnce` error verbatim, and `Run` returns it
— the process exits. Meanwhile `reconnectSession`, which runs for every *later* session, retries
forever with backoff. So the daemon tolerates any failure except the first one.

That asymmetry is what makes the macOS local-network denial fatal. Measured on the test bed
(macOS 26.4): under a LaunchAgent, the first `connect()` from an identity macOS has not yet
registered is refused with `EHOSTUNREACH` — the kernel log names `reason: NECP` — and the *next*
attempts seconds later succeed. Two independent ad-hoc probes showed `fail, ok, ok` and
`fail, fail, ok`. Apple documents the shape (TN3179: the system "may deny the operation immediately
before the user responds") and has an open bug where a short-lived process dies before the grant
lands (FB16131937). `hubfuse devices` as a one-shot died there; `hubfuse start` would have survived
had it retried.

The user's requirement, stated literally: when the Mac comes back, the agent must come back on its
own. Retry delivers that for every cause at once — a Mac waking up, a network that is not up yet, a
hub that is still starting, and the NECP first-contact denial.

Two shipped artifacts of #74 are also retracted here. Both were wrong:

- the error message tells the operator to approve hubfuse under System Settings → Privacy &
  Security → Local Network. A bare ad-hoc Mach-O cannot appear there: `nehelper` logs
  `Could not find bundle ID or display name for app: (bundleID: hubfuse-<hash>, name: (null),
  teamID: (null))` and no entry can be constructed. The repo owner went looking for that entry and
  could not find it.
- the message and CLAUDE.md claim the decision "follows the PATH, not the file". That was never
  isolated — every comparison changed the launch context at the same time. The earlier CDHash
  reading (#87) was the same confound.

## Context (from discovery)

- `internal/agent/daemon.go:525` `Run` — returns on `registerAndSubscribe` error.
- `internal/agent/daemon.go:747` `registerAndSubscribe` — returns the first `sessionOnce` error.
- `internal/agent/daemon.go:1190` `reconnectSession` — the loop we want: infinite backoff
  (`backoffInitial` → `backoffMax`), `ErrHubRejected` logged at Error and retried, the
  local-network denial streak, the one-per-episode connection swap, returns nil only on ctx.Done.
- `internal/agent/connector.go:95` `Connect` — already retries, but `grpc.NewClient` is lazy, so it
  cannot observe an unreachable hub. The first real failure is always the Register RPC.
- `internal/common/daemonize/spawn_unix.go:109` — waits `ReadyTimeout` (5s default) for the PID
  file, kills the child on timeout, and the timeout branch omits the log tail the exit branch shows.
- `internal/agent/localnetwork.go:107` `localNetworkDenialMessage` — carries both retracted claims.
- `cmd/hubfuse/launchagent.go` — writes `KeepAlive: true` with no `ThrottleInterval` (#98).

Tests that pin behaviour or claims this change deliberately replaces (spec change, not green-chasing):

- `internal/agent/localnetwork_test.go:173` `TestRegisterAndSubscribe_NamesLocalNetworkDenialAtStartup`
  — requires `registerAndSubscribe` to return an error. With retry it would block forever.
- `internal/agent/localnetwork_test.go:196` `TestRegisterAndSubscribe_StaysQuietOnAnOrdinaryStartupFailure`
  — same shape.
- `internal/agent/localnetwork_test.go:164` — asserts the message contains "path" and "rebuilding
  hubfuse in place does not produce a fresh prompt".
- `cmd/hubfuse/launchagent_test.go:110` — pins "the decision follows the PATH, not the file".

Nothing pins the startup exit itself: `tests/integration/liveness_test.go:82` asserts
`ErrHubRejected` on a direct `client.Register` call, not on `Daemon.Run`.

## Development Approach

- **testing approach**: Regular (the change is a control-flow move; the tests that matter are the
  new spec, written immediately after each change)
- every task ends with tests, run before the next task starts
- `make test` must be green before the PR; `make test-race` for the agent package
- backward compatibility: the config, the CLI surface and the plist label are unchanged

## Testing Strategy

- **unit** (`internal/agent`): startup retry succeeds after N transient failures; `onReady` fires
  exactly once and only after the first success; a cancelled context returns cleanly with `onReady`
  never called. The message tests are rewritten against the new text.
- **unit** (`cmd/hubfuse`): the generated plist pins `KeepAlive`/`ThrottleInterval` content and no
  longer pins the retracted claim.
- **scenario** (`tests/scenarios`): start the daemon BEFORE the hub, then start the hub, and assert
  the device reaches `online`. `helpers.launchDaemon` waits on the daemon's own
  `ssh server listening` line, which is emitted before `registerAndSubscribe`, so the harness
  supports a hubless start as-is. This is the user's requirement expressed literally.
- **test bed**: the NECP denial can no longer be reproduced on that Mac (TN3179 says the privilege
  cannot be reset — FB14944392), but "hub down when the agent starts → hub up → device online" can,
  by stopping and starting the hub on the Linux box while the LaunchAgent daemon runs. Required
  before the PR is merged.

## Progress Tracking

- mark completed items `[x]` immediately
- ➕ for newly discovered tasks, ⚠️ for blockers

## Solution Overview

`registerAndSubscribe` stops being a one-shot. On the first `sessionOnce` failure it hands over to
`reconnectSession(ctx)` — the same loop `supervise` uses — and only gives up when that returns nil,
which happens exactly when the context is cancelled.

**One path for every failure, including `ErrHubRejected`.** The alternative (exit at startup on a
refusal, retry mid-life) was considered and rejected:

- nothing pins the exit, so it is a free choice;
- under launchd — the path that actually matters — exiting plus `KeepAlive` is #98's restart loop,
  which is strictly worse than one process retrying on backoff;
- it removes a genuine asymmetry: today the identical error kills the daemon at startup and is
  survivable one second later.

The cost, stated plainly: `hubfuse start` in the foreground against a pruned identity now loops
instead of exiting. It is not silent — `reconnectSession` already logs the hub's own
"run hubfuse join" instruction at Error on every attempt, and backoff caps at 60s.

`Spawn`'s readiness timeout gains the log tail its exit branch already has, so the `-d` path still
shows the real cause instead of only "did not become ready within 5s". Deliberate consequence:
`hubfuse start -d` against a down hub now fails after ~5s instead of ~1s.

The denial message becomes a **diagnostic, not a remedy**. The daemon no longer dies, so the
message's job is to say what is happening and what is verifiable. Nothing unmeasured goes in it:
the "run once from Terminal.app" idea is not in this plan, because whether a Terminal-attributed run
registers anything a later launchd run can use has not been measured and cannot be measured on the
one Mac available.

## Technical Details

`registerAndSubscribe` after the change:

1. `sessionOnce(ctx)` → success: `go supervise(ctx, stream)`, return nil.
2. failure: log the startup diagnostic, then `stream := d.reconnectSession(ctx)`.
3. `stream == nil` → the context was cancelled → return `ctx.Err()` wrapped.
4. otherwise `go supervise(ctx, stream)`, return nil.

`localNetworkEvidenceStartup` stays: it is emitted once, for the first failure, before entering the
loop, and it is the honest description of what has been seen at that moment (one failure, not a
streak). `reconnectSession`'s own streak logic then takes over.

`onReady` is unchanged — it lives inside `sessionOnce` under a `sync.Once`, so the PID file is
written when the daemon actually registers, whichever attempt that is. That is the #69 contract and
it survives untouched.

New plist keys:

```
<key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
<key>ThrottleInterval</key><integer>30</integer>
```

`SuccessfulExit: false` means "restart only if it exited non-zero", which is what makes
`hubfuse stop` (SIGTERM → exit 0) actually stop the daemon instead of having launchd bring it
straight back. `ThrottleInterval` 30 bounds a crash loop to 2 launches a minute.

## What Goes Where

- **Implementation Steps**: code, tests, docs in this repo.
- **Post-Completion**: the test-bed run and the leftover background items on the Mac.

## Implementation Steps

### Task 1: Startup failure falls into the reconnect loop

**Files:**
- Modify: `internal/agent/daemon.go`
- Modify: `internal/agent/localnetwork_test.go`
- Create: `internal/agent/startupretry_test.go`

- [ ] rewrite `registerAndSubscribe` to hand over to `reconnectSession` on the first failure
- [ ] rewrite its doc comment: why one path, why `ErrHubRejected` is not special-cased, what the
      cost is
- [ ] write a test: N transient `registerFn` failures then success → `Run`/`registerAndSubscribe`
      does not return, `onReady` fires exactly once and only after the successful attempt
- [ ] write a test: context cancelled mid-retry → returns cleanly, `onReady` never called
- [ ] rewrite `TestRegisterAndSubscribe_NamesLocalNetworkDenialAtStartup` against the new spec
      (cancellable ctx; the diagnostic is still logged once; the daemon does NOT exit)
- [ ] rewrite `TestRegisterAndSubscribe_StaysQuietOnAnOrdinaryStartupFailure` the same way
- [ ] run `go test ./internal/agent/...` — must pass before Task 2

### Task 2: Spawn's readiness timeout reports the cause

**Files:**
- Modify: `internal/common/daemonize/spawn_unix.go`
- Modify: `internal/common/daemonize/spawn_unix_test.go`

- [ ] include `tailFile(absLogPath, 20)` in the timeout error, as the exit branch already does
- [ ] write a test: a child that never writes a PID file → the timeout error carries the log tail
- [ ] run `go test ./internal/common/...` — must pass before Task 3

### Task 3: Retract the two wrong claims from the denial message

**Files:**
- Modify: `internal/agent/localnetwork.go`
- Modify: `internal/agent/localnetwork_test.go`

- [ ] rewrite `localNetworkDenialMessage`: name NECP as the mechanism, say the daemon is retrying,
      say a Local Network prompt should be allowed if it appears, attribute the
      `com.apple.network.local-network` CIDR keys to TN3179 rather than asserting them
- [ ] remove the "per binary PATH" and "rebuilding in place" claims
- [ ] rewrite the doc comment on `isLocalNetworkDenial` to drop the unmeasured keying story and
      keep only what the logs show
- [ ] rewrite the message test to pin the new content and to assert the retracted phrases are ABSENT
- [ ] run `go test ./internal/agent/...` — must pass before Task 4

### Task 4: LaunchAgent stops restart-looping (#98)

**Files:**
- Modify: `cmd/hubfuse/launchagent.go`
- Modify: `cmd/hubfuse/launchagent_test.go`

- [ ] replace `KeepAlive: true` with `SuccessfulExit: false` and add `ThrottleInterval` 30
- [ ] rewrite the post-install instructions printed by `install-agent` to match the new message
- [ ] remove the retracted "follows the PATH" claim from the file and its test
- [ ] write a test pinning the plist's `KeepAlive`/`ThrottleInterval` content
- [ ] write a test that `hubfuse stop` semantics are preserved: the plist must NOT restart on a
      zero exit
- [ ] run `go test ./cmd/...` — must pass before Task 5

### Task 5: Scenario — the agent starts before the hub and still comes up

**Files:**
- Create: `tests/scenarios/startup_retry_test.go`

- [ ] launch the daemon with no hub running, wait for its `ssh server listening` line
- [ ] assert the daemon process is still alive after several retry cycles (the regression guard:
      before this change it would be gone)
- [ ] start the hub, then assert the device reaches `online` within a bounded wait
- [ ] run `make test-integration` and the scenario suite — must pass before Task 6

### Task 6: Verify acceptance criteria

- [ ] `make test` green
- [ ] `make test-race` green for `./internal/agent/...`
- [ ] `make vet` and the linter green
- [ ] confirm no test still asserts the retracted claims: grep for "follows the PATH", "per binary",
      "System Settings" outside `docs/plans/completed/`

### Task 7: [Final] Documentation

- [ ] rewrite the `localnetwork.go` paragraph in CLAUDE.md: NECP, the nehelper evidence, the
      unreconciled older evidence named as unreconciled, and the retry as the fix
- [ ] update README.md's macOS section: drop the impossible System Settings instruction, add the
      TN3179 CIDR keys as Apple's documented option
- [ ] move this plan to `docs/plans/completed/` (in this branch, before the merge)

## Post-Completion

**Manual verification:**
- on the test bed: stop the hub, start the Mac daemon under its LaunchAgent, confirm it stays alive
  and logs retries; start the hub; confirm `hubfuse devices` shows `mac online` without touching the
  Mac.
- confirm `hubfuse stop` actually stops the LaunchAgent-managed daemon (the `SuccessfulExit` fix).

**Housekeeping on the test-bed Mac:**
- remove the leftover background-item registrations from this session's probes (`soak.sh`,
  `probe-plain`, `probe-plist`) — check with `sfltool dumpbtm`.
- remove `~/Applications/HubFuseProbe.app` once the owner has finished inspecting the Local Network
  pane.

**Not in scope, recorded deliberately:**
- shipping the agent as a `.app` bundle. It is the only measured way to make the binary listable in
  System Settings, but the owner declined the build change and the measurements show access does not
  require it (a bare ad-hoc binary under a LaunchAgent: 18/18 LAN connects over 4 minutes).
