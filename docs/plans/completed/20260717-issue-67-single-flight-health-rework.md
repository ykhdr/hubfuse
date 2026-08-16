# Issue #67: single-flight mount health and periodic reconciliation

> **Статус: выполнен.** Шаги 1–6 реализованы, PR #68 смержен (`4e691e2`), issue #67 закрыт.
> Три прохода ревью дали 0 critical; найденные MEDIUM/LOW закрыты в ветке — в том числе
> broadcast результата пробы через `close` вместо send, привязка залипшей горутины к
> `probeEntry` (одна на маунт, а не одна на тик), перечитка желаемого состояния перед
> каждым `Mount` и экспоненциальный backoff только на установку с нуля.
>
> Открытым остаётся только ручной прогон на живой паре машин (см. Risks ниже) — стенд со
> стабом проверяет оркестрацию, но не поведение реального ядра и FUSE.

## Goal and acceptance criteria

- A dead sshfs/FUSE mount at an unchanged IP/port is detected and remounted automatically.
- Recovery works without a new `DeviceOnline` event.
- Health is tri-state: `healthy`, `dead`, `unknown`; timeout and ambiguous errors never trigger teardown.
- At most one potentially hanging health probe exists for a given active mount generation.
- A probe result from an old `*Mount` can never tear down its replacement.
- A failed remount that removes the old entry is retried by the next reconciliation tick.
- Monitor shutdown follows the daemon context; existing #47/#49/#50/#61 behavior remains intact.

## Current evidence

- `internal/agent/mounter.go:319-335` probes same-endpoint mounts through `mountpointGoneCtx`.
- `internal/agent/mounter.go:533-573` treats every check error, including EACCES/EINTR, as proof that a mount is gone.
- `internal/agent/mounter.go:846-869` starts a fresh bounded wrapper goroutine for every active mount on every `DeadMounts` sweep; a syscall that never returns can therefore accumulate goroutines indefinitely.
- `internal/agent/daemon.go:750-841` performs detect-then-act healing from a stale `[]*Mount` snapshot and only visits entries already classified dead.
- `internal/agent/daemon.go:731-736` documents that after successful teardown plus failed remount no active entry remains and periodic recovery stops until an unrelated event; this is not reconciliation.
- Existing scenario work in `internal/agent/stubmount.go` and `tests/scenarios/heal_test.go` provides a useful marker/PID harness and an eventless healing test; retain it unless focused review finds a correctness issue.
- No new dependency is required; the implementation can use existing `sync`, `context`, `errors`, `os`, and `syscall` facilities.

## Read-only plan review

- CRITICAL confirmed: `mountpointGoneCtx` always starts a new goroutine, and both the periodic `DeadMounts` sweep and same-endpoint `Mount` can start probes for the same mount concurrently. Steps 2–4 replace this with a generation-bound single-flight probe and remove `DeadMounts`.
- MAJOR confirmed: the existing classifier treats every check error as dead. Step 1 restricts confirmed-dead errors to ENOTCONN and missing-path cases; all other errors are unknown and cannot trigger teardown.
- The reviewer found the online-remount stale snapshot path generation-safe because `Mount` rechecks `activeMounts` under `m.mu`. The replacement design still removes detect-then-act snapshots entirely, so offline/unconfigured actions cannot accidentally apply a stale verdict to a replacement mount.
- Review gate result: no unresolved critical or major planning issue remains; implementation may proceed using this corrective plan.

## Decision record

Goal: heal confirmed-dead same-endpoint mounts, including eventless failures.

Chosen approach: conservative single-flight tri-state probing inside `Mounter.Mount` (Ensure semantics), driven periodically by a daemon desired-state reconciliation loop.

Rejected alternative: repeated `DeadMounts` sweeps using `mountpointGoneCtx`; they can leak one permanently blocked goroutine per tick and collapse ambiguous errors into `dead`.

Rejected alternative: foreground `sshfs -f` process supervision; it changes backend lifecycle and does not by itself solve wedged-but-alive FUSE behavior.

Constraints: never destroy on timeout/unknown; preserve generation identity across unlocked probes; no hub/proto or user config changes.

Open assumption: `ENOTCONN`, a missing path, or a successful `isMountpoint == false` are sufficient confirmed-dead signals for v1. Other errors remain `unknown` until live evidence justifies expanding the set.

## Scope

- Modify: `internal/agent/mounter.go` — tri-state classifier, per-key/per-generation single-flight probe, generation-safe Ensure flow.
- Modify: `internal/agent/mounter_test.go` — classification, timeout, single-flight, stale-generation, and same-endpoint remount tests.
- Modify: `internal/agent/daemon.go` — replace detect-only `DeadMounts` monitor with desired-state periodic reconciliation.
- Modify: `internal/agent/daemon_test.go` — eventless reconciliation, failed-remount retry, cancellation, and no-overlap tests.
- Retain and adjust as needed: `internal/agent/stubmount.go`, `internal/agent/stubmount_test.go`, `tests/scenarios/heal_test.go`, and scenario helpers — end-to-end proof only.
- Reassess and remove if no longer needed: `processInitialDevices` pruning added by the existing branch; it is not required for desired-state healing and must not remain as unrelated scope without a demonstrated invariant.
- Update after implementation: `docs/plans/20260714-fix-issue-67-mount-liveness-healing.md` and `CLAUDE.md` so documentation describes the final mechanism rather than the superseded `DeadMounts` design.
- Do not change: hub registry, heartbeat/proto contracts, sshfs foreground/background mode, persistent config schema, mount interval as a user-facing config option.

## Steps

1. Add a conservative health model in `internal/agent/mounter.go`.
   - Define internal `mountHealthHealthy`, `mountHealthDead`, and `mountHealthUnknown` states.
   - Classify `(true, nil)` as healthy and `(false, nil)` as dead.
   - Classify `ENOTCONN` and missing-path errors as dead.
   - Classify timeout, cancellation, EACCES, EINTR, and all other errors as unknown.
   - Completion: table tests prove every classification and no unknown result reaches unmount.

2. Add a generation-bound single-flight probe registry to `Mounter`.
   - Store one probe per `mountKey`, including the exact `*Mount`, a completion channel, and immutable result written before channel close.
   - Concurrent callers for the same key/generation join the existing probe.
   - A timed-out caller returns unknown but leaves the in-flight probe registered until its syscall actually returns.
   - Replacing/removing a mount invalidates applicability through pointer identity; stale completion may be discarded but cannot affect the new entry.
   - Completion: a blocking test checker observes one invocation across multiple monitor ticks/callers, and a stale-generation test proves the replacement is untouched.

3. Refactor `Mounter.Mount` into generation-safe Ensure semantics.
   - Do not hold `m.mu` while waiting for a health probe.
   - After the probe, reacquire `m.mu` and verify `activeMounts[key] == probedMount`; if state changed, restart the decision loop.
   - Same endpoint: healthy/unknown returns without teardown; dead uses the existing bounded force-unmount plus normal mount path.
   - Changed endpoint retains #61 behavior without requiring a liveness probe.
   - Remove `DeadMounts` after all callers migrate.
   - Completion: existing endpoint-change tests pass; same-endpoint healthy/dead/unknown and concurrent replacement tests pass under `-race`.

4. Replace `healDeadMounts` with desired-state reconciliation in `internal/agent/daemon.go`.
   - On each tick, snapshot config and online devices under `d.mu`, then release it.
   - Build desired mounts by intersecting configured mounts with online, paired peers and currently exported shares.
   - Sequentially call `mounter.Mount(ctx, ...)` for every desired mount; this both probes existing entries and creates missing entries.
   - Do not launch overlapping sweeps; a slow sweep delays the next tick.
   - Do not tear down a mount merely because a peer is absent from the snapshot; event paths retain ownership of ordinary offline cleanup.
   - Completion: a dead existing mount heals without events, and a missing entry left by a failed previous remount is retried on the next tick.

5. Reconcile existing branch scope.
   - Keep the truthful stub marker/PID harness only to the extent required by scenario verification.
   - Evaluate the added `processInitialDevices` pruning independently; remove it if desired-state reconciliation and existing event semantics do not require it.
   - Remove superseded `DeadMounts` tests and replace them with single-flight/reconciliation tests rather than layering more tests on obsolete behavior.
   - Completion: `git diff origin/master...HEAD` contains no unrelated behavioral change and documentation matches the final design.

6. Verify focused behavior and regressions.
   - Run `go test ./internal/agent/... -run 'Test.*(Mount|Probe|Health|Reconcile|Monitor)' -count=1`.
   - Run `go test ./internal/agent/... -race -count=1`.
   - Run `go test ./tests/scenarios/... -run 'TestMonitorRemountsDeadMount|TestOfflineOnlineCycleRemountsMount' -count=1`.
   - Run `make vet`, `make test`, and `make build`.
   - Inspect `git diff --check` and `git status --short --branch`.

## Risks / open questions

- Go cannot cancel a goroutine blocked inside `syscall.Stat`; single-flight bounds this to one blocked goroutine per mount generation but cannot reclaim it.
- Holding `m.mu` through the current full mount establishment avoids duplicate mount races; the refactor must release it only for the health wait and revalidate generation before acting.
- Existing branch size is much larger than the core fix. Scope reduction is preferred over preserving already-written code solely because it exists.
- Real FUSE behavior still requires the post-completion two-machine manual check described in issue #67; the stub scenario proves orchestration, not kernel/FUSE error behavior.
