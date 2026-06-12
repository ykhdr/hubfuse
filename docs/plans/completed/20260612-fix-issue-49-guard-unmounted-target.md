# Fix #49: Guard the Mount Target When Not Mounted

## Problem statement

When a configured mount target is **not actually mounted** — because the mount
failed, the peer is offline, or a live mount died and was reaped (#47) — the
target path is just a normal local directory. Writes to it silently land on the
**local** filesystem with `rc=0` and no indication. If a real mount later
succeeds over that path, the local files are shadowed and resurface when the
mount drops again. This is silent data divergence.

Repro: configure `mount mac:macshare -> ~/claude/remote` with the mount NOT
established → `echo hi > ~/claude/remote/x.txt` returns `rc=0` and the file
persists on the LOCAL fs.

PR #44 (commit `ae64c6e`) added mount **detection** for the mounting agent:
`Mount` now polls `checkMountpoint(mc.To)` and fails if the mountpoint never
appears. That closes the "mount silently failed but we recorded it" gap, but it
does **not** cover #49 because:

- A live mount can drop **later** (peer restart) — the path becomes a writable
  local dir again until the next re-mount, with nothing guarding it in between.
- **Any** process can write to the configured target path while it is unmounted
  (during startup before the peer is online, between offline and re-online,
  after a reap, etc.).

The window between "configured" and "verified mounted" is unguarded.

## Context (verified against the worktree at HEAD `a6c42c5`)

All paths are under
`/Users/ykhdr/projects/hubfuse/.worktrees/issue-49-guard-target`.
This branch is **stacked on the #47/#50 fix** already committed here, so the
mounter already has: classify-then-reap in `Unmount`/`unmountKey`, ctx-bounded
`unmountPath` with the per-OS escalation ladder, `UnmountAllForce`,
`UnmountDevice` (force), and `mountpointGoneCtx`.

### `internal/agent/mounter.go`

- `Mount(ctx, mc, deviceID, deviceIP, sshPort)` (L246):
  - L248: `os.MkdirAll(mc.To, 0755)` — **the leak source.** Creates/leaves the
    target world-traversable + owner-writable.
  - L257-259: rejects a duplicate mount ("already mounted").
  - L281-312: verify loop polls `m.checkMountpoint(mc.To)` until the mountpoint
    appears, the deadline passes, or ctx is cancelled. On timeout/cancel it
    kills+reaps the backend and returns an error **without** recording the
    mount. This is exactly the path after which the target is exposed and must
    be (re-)guarded.
- `unmountKey(ctx, key, force)` (L414): the core unmount. On command failure it
  re-checks via `mountpointGoneCtx` and, if gone (or ctx-timed-out), reaps:
  `delete(m.activeMounts, key)` + WARN (L438-445). On command success it
  deletes + INFO logs (L454-459). **Both branches are where a re-guard must
  happen** — after a successful or reaped unmount the path is exposed again.
  Note L452 comment: lazy unmount may still look mounted briefly, so the
  success branch does **not** re-check.
- `mountpointGoneCtx(ctx, path)` (L381): ctx-guarded mountpoint check (returns
  "gone" on ctx timeout). Reusable for any wedge-safe stat.
- `isMountpoint(path)` (L99): stdlib `syscall.Stat` st_dev comparison. Unix-only.
- Test seams: `execCommand`, `unmount`, `checkMountpoint`,
  `mountVerifyTimeout/Interval`; setters `SetExecCommandForTests`,
  `SetUnmountForTests`, `SetMountpointCheckForTests` (L571-591).
- `NewMounter` (L221) bypasses mountpoint verification when
  `HUBFUSE_STUB_MOUNT_DIR` is set (scenario harness).

### `internal/agent/daemon.go`

- `processInitialDevices` (L389): startup auto-mount loop → `d.mounter.Mount`.
- `Shutdown` (L350): `UnmountAllForce(uctx)` under a 5s timeout.
- `shouldMount(nickname)` (L416): returns the `MountConfig` for a nickname.

### `internal/agent/events.go`

- `handleDeviceOnline` (L35) → `Mount`; `handlePairingCompleted` (L169) →
  `Mount`. Auto-mount paths.
- `handleDeviceOffline` (L77) / `handleDeviceRemoved` (L97) →
  `UnmountDevice` (force=true).
- `handleSharesUpdated` (L117) → `Unmount` for shares removed remotely.

### `internal/agent/confighandler.go`

- `onConfigChange` (L14): `diff.MountsAdded` → `tryMount` (L90); `MountsRemoved`
  → `Unmount` (L35). `tryMount` skips when the device is offline/unpaired —
  exactly the case where the target sits unmounted and must be guarded.

### `cmd/hubfuse/main.go`

- `mountAddCmd` (L769) / `mountRemoveCmd` (L821): edit `config.kdl` and exit.
  They do **not** touch the filesystem mountpoint directly; the running daemon's
  config watcher (`onConfigChange`) reacts. So perm restore on `mount remove`
  belongs in the daemon's `MountsRemoved` handler, not the CLI.

### `internal/agent/config/config.go`

- `MountConfig{ Device, Share, To string }` (L53). `To` is the target path.

### Tests

- `internal/agent/mounter_test.go`: `newTestMounter` sets `unmount`,
  `checkMountpoint` (default `true,nil`), tiny verify timeouts. Mount tests
  assert `os.Stat(mountTo)` (L411) and use `t.TempDir()`.
- `internal/agent/daemon_test.go`: `buildTestDaemon` (L27) builds a real
  `Daemon` with a real `Mounter`; pre-mount tests override the seam via
  `d.mounter.SetMountpointCheckForTests(...)` (L775).

### Empirical chmod check (run during discovery)

A `0500` directory: `touch dir/x` → **`Permission denied`** even for the owner
(owner write bit clear), while `cd dir` / `stat dir` still succeed (execute bit
set). Confirmed on darwin; Linux dir-permission semantics are identical (the
**write** bit on a directory governs creation/deletion of entries; the
**execute** bit governs traversal/lookup).

## Chosen approach

**A single "guard" helper that chmods the target to a restricted, traversable
mode (`0500`) whenever it is configured-but-not-verified-mounted, applied at
every mountpoint-creation and post-unmount/reap site; plus a pre-mount
non-empty refusal; plus a perm-restore on config-driven `mount remove`.**

Three new unexported `Mounter` methods (kept in `mounter.go` next to `Mount`):

```go
// guardMode is the restricted mode applied to an unmounted mount target:
// r-x for owner, nothing for group/other. The cleared write bit blocks entry
// creation/deletion (so stray local writes fail with EACCES); the set execute
// bit still lets the agent traverse the path to mount over it. A live FUSE
// mount masks this mode while active, and it is re-applied on unmount/reap.
const guardMode os.FileMode = 0o500

// guardTarget creates the target dir if absent and chmods it to guardMode so an
// unmounted target cannot silently absorb local writes. Idempotent. It never
// touches a path that is currently a mountpoint (the mount masks the dir mode).
func (m *Mounter) guardTarget(path string) error

// unguardTarget restores the target dir to a normal mode (0o755) so a path the
// user has removed from config behaves like an ordinary directory again.
func (m *Mounter) unguardTarget(path string) error

// targetHasLocalContents reports whether path is a non-mountpoint directory
// that contains entries — i.e. real local files that a mount would shadow.
// Returns (false, nil) when the dir is absent, empty, or currently a mountpoint.
func (m *Mounter) targetHasLocalContents(path string) (bool, error)
```

### Mount flow (rewritten head of `Mount`, mounter.go ~L246-275)

1. `guardTarget(mc.To)` instead of `os.MkdirAll(mc.To, 0755)`. This creates the
   dir if needed **and** restricts it immediately — so even the create→mount
   window is guarded.
2. **Non-empty refusal (under the lock, before `cmd.Start`):** if
   `targetHasLocalContents(mc.To)` is true, **hard refuse**: log a loud WARN and
   return an error without mounting. Do not shadow pre-existing local data.
3. Build args, `cmd.Start`, run the existing verify loop.
4. On verify **timeout / ctx-cancel** (the kill+reap branches at L286-309):
   before returning the error, re-apply `guardTarget(mc.To)` — the failed mount
   may have left the dir traversable/created, so re-restrict it. (Belt-and-
   suspenders: `guardTarget` at step 1 already restricted it, but sshfs/FUSE can
   chmod or create the mountpoint during a partial mount.)
5. On success: record the mount. **Do not** chmod — the live mount masks the
   underlying dir's mode, and the underlying restricted mode is exactly what we
   want exposed again the moment it unmounts.

### Unmount flow (in `unmountKey`, mounter.go L414-461)

After the entry is deleted (both the **reap** branch ~L438 and the **success**
branch ~L454), call `guardTarget(mnt.LocalPath)` to re-restrict the now-exposed
underlying directory before any process can write into it.

- **Skip the re-guard during shutdown.** `UnmountAllForce` (shutdown) should not
  bother chmod-ing on the way out — the process is exiting and the budget is
  tight. Thread a `reguard bool` through `unmountKey`/`unmountAll` so
  `UnmountAllForce` passes `reguard=false` and every other caller passes
  `reguard=true`. (See "force vs reguard" below — they are orthogonal.)
- `guardTarget` errors here are logged at WARN, never returned — a failed
  re-guard must not turn a successful unmount into a failure (and errcheck is
  satisfied by `if err := ...; err != nil { log }`).

### Startup / hot-reload / auto-mount coverage

Because **every** mount goes through `Mount` (startup loop, `handleDeviceOnline`,
`handlePairingCompleted`, `tryMount`), and `Mount`'s first action is now
`guardTarget`, the create-time guard is covered centrally — no per-call-site
change is needed for the happy path. Two gaps remain that `Mount` alone does not
cover, because `Mount` is only called when the peer is online+paired:

- **`tryMount` early-returns** (confighandler.go L101-115) when the device is
  offline or unpaired — `Mount` is never called, so a freshly-added target sits
  ungated. Fix: in `tryMount`, when it decides **not** to mount, call
  `d.mounter.guardTarget(mc.To)` (log WARN on error) so the target is restricted
  the moment it is configured.
- **Startup before first connect:** `processInitialDevices` only mounts online
  peers; offline-peer targets are ungated until they come online. Add a
  `d.guardConfiguredTargets()` pass in `NewDaemon` (or early in `Run`, before
  `registerAndSubscribe`) that iterates `cfg.Mounts` and `guardTarget`s each
  `mc.To`. Idempotent and cheap.

This makes `guardTarget` the single mechanism, applied at: (a) mountpoint
creation inside `Mount`, (b) the "decided not to mount" branch of `tryMount`,
(c) post-unmount/reap in `unmountKey`, and (d) a startup sweep of all configured
targets.

### Perm restore on `mount remove`

In `onConfigChange`, the `diff.MountsRemoved` loop (confighandler.go L34-42)
already calls `Unmount`. After that, call `d.mounter.unguardTarget(mc.To)` so a
target the user has deliberately removed from config returns to a normal
`0o755` directory (no surprise read-only dir lingering). The CLI `mount remove`
only edits config; the running daemon performs the filesystem restore via this
handler. Document: if the daemon is **not** running when config is edited, the
dir stays at `0o500` until the next daemon run unmounts/reaps it — acceptable,
and `unguardTarget` is safe to also call from the startup sweep for targets no
longer in config if we want to be thorough (optional; see Risks).

## Plan-review fixes to incorporate (MUST before impl)

1. **RESOLVE THE `checkMountpoint` CONTRADICTION — neither `guardTarget` nor
   `targetHasLocalContents` may consult `checkMountpoint`.** The plan body is
   self-contradictory (some sections say "skip if mountpoint via checkMountpoint",
   others say "do NOT, rely on call-site ordering"). Final decision:
   - `guardTarget(path)`: NO `checkMountpoint` call. It is only ever invoked on
     paths the caller has already confirmed are unmounted (pre-mount before
     `cmd.Start`, on Mount-failure, after `delete` in `unmountKey`, in the
     startup sweep, in `tryMount`'s not-mounted branch). Just `os.MkdirAll(dir
     parent)` + `os.Chmod(path, guardMode)`. Avoids the wedged-`Stat` hang.
   - `targetHasLocalContents(path)`: NO `checkMountpoint` call either. It is only
     called pre-mount in `Mount`, before our mount exists, so it simply
     `os.ReadDir`s the path. `os.IsNotExist` → `(false,nil)`; empty → `(false,
     nil)`; ≥1 entry → `(true,nil)`. (Enumerating a pre-existing *foreign*
     mountpoint and refusing to mount over it is the correct, desired behavior.)
   - This also removes the test-seam collision: the default `newTestMounter` sets
     `checkMountpoint → (true,nil)`, which would otherwise make these helpers
     no-op in every test. With this resolution the guard-mode tests (1/2/5/6/11)
     are meaningful without per-test seam overrides.
2. **Fix the stub no-op rationale (keep the no-op).** The stated reason ("scenario
   tests write through the fake mount path → EACCES") is WRONG: stub-sshfs writes
   a marker into `HUBFUSE_STUB_MOUNT_DIR`, never through the target path, and no
   scenario test writes to the target. Keep `guardTarget`/`targetHasLocalContents`
   as no-ops under the stub anyway (the dir is never masked by a real mount, so a
   lingering `0o500` is pointless and could confuse future tests) — but correct
   the justification comment to this.
3. **Close the `handlePairingCompleted` gap (nit → just do it).** In
   `events.go handlePairingCompleted`, when it early-returns because the peer is
   offline, call `guardTarget(mc.To)` (WARN on error), symmetric with `tryMount`.
   Cheap, removes the brief-ungated window.
4. **`MountsRemoved`: `unguardTarget` must run even when `Unmount` errored.** In
   `confighandler.go` ensure `unguardTarget(mc.To)` is OUTSIDE/after the
   `if err != nil` log block (a failed unmount must not skip the perm restore).
5. **Test 3 asserts the WARN content**, not just that an error is returned — the
   message must name the path and be actionable (per coordinator decision #2).

## Coordinator-confirmed decisions

1. **Re-guard on device-offline (`reguard=true`), NOT on shutdown
   (`reguard=false`).** Confirmed.
2. **Hard-refuse over a non-empty target, no `--force` escape hatch this PR** —
   confirmed, BUT the WARN must be **actionable**: name the exact path, the entry
   count, and tell the user precisely what to do (move/remove the local files, or
   point `to` elsewhere) so auto-mount can be unblocked in-band.
3. **Startup sweep in `Run`** (before `registerAndSubscribe`), not `NewDaemon`.
   Confirmed.
4. **Stub harness: `guardTarget`/`unguardTarget` MUST be a no-op when
   `HUBFUSE_STUB_MOUNT_DIR` is set.** With stub-sshfs there is no real FUSE mount
   to mask the `0o500` dir, so guarding it would make scenario tests that write
   through the (fake) mount path fail with EACCES. Gate the guard the SAME way
   `NewMounter` already bypasses mountpoint verification: capture
   `os.Getenv("HUBFUSE_STUB_MOUNT_DIR") != ""` into a `Mounter` field at
   construction (do not read the env on every call) and skip the chmod when set.
   `targetHasLocalContents` should likewise return `(false, nil)` under the stub
   so the non-empty refusal never trips in scenario tests. Add a unit test
   asserting the no-op-under-stub behavior.

## Resolved design tensions

### 1. Owner-can't-traverse — **chosen mode: `0o500`** (not `0000`-then-chmod-up)

`0o500` (`r-x------`) clears the directory **write** bit, so creating or deleting
entries fails with `EACCES` — blocking the silent local write — while the
**execute** bit stays set, so the agent (the owner) can `stat`/traverse the path
and sshfs/FUSE can mount over it. Empirically confirmed during discovery:
`touch` into a `0o500` dir is denied even for the owner, yet `cd`/`stat` work.

Rejected `0000`-then-chmod-up-right-before-mount because: (a) it needs a
chmod-up immediately before `cmd.Start` and a chmod-down on every failure/exit
path, widening the unguarded window and the error surface; (b) `0000` blocks the
agent's own traversal, which FUSE needs; (c) `0o500` is a single stable mode
that is correct both at rest and during mount (the live mount masks it anyway),
so there is no up/down dance. We keep owner read+execute (not `0o100`) so
`mount list` / diagnostics can still `stat`/list the (empty) mountpoint.

### 2. Re-guard on unmount/reap — **in `unmountKey`, after `delete`, both branches; gated by a `reguard` flag**

The chmod-down happens inside `unmountKey` right after the entry is removed from
`activeMounts`, in **both** the reap branch and the success branch (the path is
exposed in both cases). It is **not** a separate helper at the call sites,
because `unmountKey` is the single funnel for every unmount path
(`Unmount`, `UnmountAll`, `UnmountAllForce`, `UnmountDevice`, `handleSharesUpdated`,
`MountsRemoved`).

`reguard` is **orthogonal to `force`**: `force` selects the unmount command
ladder; `reguard` decides whether to chmod the underlying dir afterward.
- Shutdown (`UnmountAllForce`): `force=true, reguard=false` (process exiting,
  don't bother).
- Device offline/removed (`UnmountDevice`): `force=true, reguard=true` (the
  target stays in config; re-restrict so a write can't leak before re-online).
- Interactive (`Unmount`, shares-updated, `MountsRemoved`): `force=false,
  reguard=true`.

(`MountsRemoved` re-guards via `unmountKey`, then `onConfigChange`
**un**guards via `unguardTarget` — net result: removed-from-config target ends
at `0o755`. The transient re-guard is harmless and keeps `unmountKey` uniform.)

### 3. Refuse to mount over a non-empty local dir — **HARD REFUSE + loud WARN** (recommended default)

Before `cmd.Start`, if `targetHasLocalContents(mc.To)` is true, return an error
and log a WARN naming the path and entry count, telling the user to move/remove
the local contents or pick a different `to`. Rationale: mounting over non-empty
local data **silently shadows** it (the exact #49 failure mode in reverse) and
risks data loss perception when it resurfaces. Refusing is the safe default —
the user's local files are never hidden without consent. An **empty** dir, or a
dir we just created in this `Mount` call, is fine and proceeds normally. (A dir
we created is empty, so the single "is it empty?" check covers both.)

This is consistent with PR #44's philosophy of failing loudly rather than
proceeding into a silent-divergence state. A future `--force`/config opt-in
could relax it, but that is out of scope.

### 4. Startup / hot-reload — **single `guardTarget` helper at all creation sites + a startup sweep + a `tryMount` not-mounted guard**

`Mount`'s `guardTarget` covers every online-mount path centrally. The two paths
that do **not** reach `Mount` are handled explicitly: a startup sweep
(`guardConfiguredTargets` over `cfg.Mounts` in `NewDaemon`/early `Run`) and the
"decided not to mount" branch of `tryMount`. `handleSharesUpdated`'s remote-
removal unmount re-guards via `unmountKey`. No `events.go` auto-mount change is
needed beyond what `Mount` already does.

### 5. Restoring perms — **yes, restore to `0o755` on config-driven `mount remove`**

`onConfigChange`'s `MountsRemoved` loop calls `unguardTarget(mc.To)` after
unmount, restoring `0o755`. If the daemon is offline when config is edited, the
dir stays restricted until the next run's unmount/reap; documented as an
accepted edge. We do **not** delete the dir (it may pre-exist / be intentional).

### 6. Cross-platform — **no macOS-specific gotcha**

`os.Chmod` maps to `chmod(2)` on both Linux and darwin with identical
directory-permission semantics (write bit = entry create/delete, execute bit =
traversal). FUSE-T (macOS) and fusermount/sshfs (Linux) both mount over a
traversable dir; the live mount masks the underlying `0o500` mode in both. The
empirical check above was on darwin. One noted non-issue: macOS may set an ACL
or extended attrs on some dirs, but `chmod` of the POSIX mode bits is what
governs `touch`, which is what the bug is about. `isMountpoint`/`guardTarget`
are already Unix-only (consistent with the existing mounter), so no Windows
concern.

## Functions / files to change

### `internal/agent/mounter.go`

```go
// New const + three methods (near Mount):
const guardMode os.FileMode = 0o500
func (m *Mounter) guardTarget(path string) error
func (m *Mounter) unguardTarget(path string) error
func (m *Mounter) targetHasLocalContents(path string) (bool, error)

// Mount (L246): replace os.MkdirAll(mc.To, 0755) with m.guardTarget(mc.To);
//   add the targetHasLocalContents hard-refuse before cmd.Start;
//   re-apply m.guardTarget(mc.To) on the verify timeout & ctx-cancel error paths.

// unmountKey (L414): add a reguard bool param; after delete in BOTH the reap
//   and success branches, if reguard { if err := m.guardTarget(mnt.LocalPath); err != nil { WARN } }
func (m *Mounter) unmountKey(ctx context.Context, key mountKey, force, reguard bool) error

// Thread reguard through the loop + public wrappers:
func (m *Mounter) unmountAll(ctx context.Context, force, reguard bool) error
func (m *Mounter) Unmount(device, share string) error            // reguard=true,  force=false
func (m *Mounter) UnmountAll() error                             // reguard=true,  force=false
func (m *Mounter) UnmountAllForce(ctx context.Context) error     // reguard=false, force=true  (shutdown)
func (m *Mounter) UnmountDevice(deviceNickname string) error     // reguard=true,  force=true
```

`targetHasLocalContents` uses `os.ReadDir` (returns `[]DirEntry`); treat
`os.IsNotExist` as `(false, nil)`. Guard against a live mountpoint via the
existing `m.checkMountpoint` (don't enumerate a mounted FS): if it is a
mountpoint, return `(false, nil)`.

### `internal/agent/daemon.go`

```go
// Add:
func (d *Daemon) guardConfiguredTargets() {
    d.mu.RLock(); cfg := d.config; d.mu.RUnlock()
    for _, mc := range cfg.Mounts {
        if err := d.mounter.guardTarget(mc.To); err != nil {
            d.logger.Warn("guard configured mount target", "to", mc.To, "error", err)
        }
    }
}
// Call it once early in Run (before registerAndSubscribe) — or at end of NewDaemon.
```

### `internal/agent/confighandler.go`

```go
// onConfigChange, MountsRemoved loop (L34-42): after d.mounter.Unmount(...),
//   if err := d.mounter.unguardTarget(mc.To); err != nil { WARN }
// tryMount (L101-115): in EACH not-mounted early-return branch (offline / unpaired),
//   call d.mounter.guardTarget(mc.To) (WARN on error) before returning.
```

`guardTarget`/`unguardTarget`/`targetHasLocalContents` are unexported methods on
`*Mounter`; the daemon is in the same `agent` package, so it can call them
directly (no new exported API).

## Implementation checklist

1. [ ] Add `guardMode` const + `guardTarget`, `unguardTarget`,
   `targetHasLocalContents` to `mounter.go` (Unix `os.Chmod`/`os.ReadDir`;
   skip when path is a mountpoint via `m.checkMountpoint`).
2. [ ] `Mount`: swap `MkdirAll(...,0755)` → `guardTarget`; add the
   non-empty-dir hard refuse (+ loud WARN) before `cmd.Start`; re-apply
   `guardTarget` on the two failure-exit branches.
3. [ ] `unmountKey`: add `reguard bool`; re-guard after `delete` in both
   branches; WARN (don't return) on guard error.
4. [ ] Thread `reguard` through `unmountAll` and the public wrappers per the
   table above (`UnmountAllForce` → `reguard=false`; all others `true`).
5. [ ] `daemon.go`: add `guardConfiguredTargets`, call it early in `Run`.
6. [ ] `confighandler.go`: `unguardTarget` on `MountsRemoved`; `guardTarget`
   in `tryMount`'s not-mounted branches.
7. [ ] Update unit tests + add new ones (below). Update any call site that
   constructs `unmountKey`/`unmountAll` directly (these are unexported — only
   `mounter.go`/its tests).
8. [ ] `make build && make vet && go test ./internal/agent/... && make test`.
   Run `golangci-lint run` (errcheck strict).

## Test plan

All filesystem assertions use `t.TempDir()` and `os.Stat(...).Mode().Perm()`.
No real FUSE needed — the chmod/ReadDir logic operates on plain temp dirs and is
driven through the existing `checkMountpoint`/`unmount` seams. Add to
`internal/agent/mounter_test.go` (and a couple of daemon-level tests to
`daemon_test.go`). Match testify `require`/`assert` style and `newTestMounter`.

1. **Mount restricts the target on creation.** Fresh `t.TempDir()` subpath that
   does not exist. `newTestMounter` (checkMountpoint=true so verify passes).
   `Mount` succeeds → assert the dir exists and (before the mount masks it, i.e.
   inspect via a stubbed-mount scenario) was chmod'd. Concretely: assert
   `guardTarget` set `0o500` by calling `Mount` then `os.Stat(mc.To).Mode().Perm()
   == 0o500` (the test mounter never creates a real mount, so the underlying
   mode is observable).
2. **Mount-fail leaves the target restricted (#49 core).** `checkMountpoint`
   returns `false,nil` + tiny `mountVerifyTimeout` so `Mount` fails the verify
   loop. Assert `Mount` returns an error AND `os.Stat(mc.To).Mode().Perm() ==
   0o500` — proving a failed mount does not leave a writable target. Bonus:
   `touch` into the dir returns `EACCES` (skip on root, where mode is ignored).
3. **Non-empty target is refused.** Pre-create `mc.To` with a file inside (and
   chmod back to writable so the file can be planted). `checkMountpoint` returns
   `false,nil` so it's seen as a plain dir. Assert `Mount` returns an error whose
   message mentions local contents, the unmount/exec seam was **never** called
   (capture invocation), and the planted file is untouched.
4. **Empty target proceeds.** Pre-create `mc.To` empty. `Mount` (verify=true)
   succeeds; no refusal.
5. **Re-guard after successful unmount.** Mount (records entry), then `Unmount`
   (seam returns nil). Assert `IsActive` false AND `os.Stat(mc.To).Mode().Perm()
   == 0o500` (re-restricted after a clean unmount).
6. **Re-guard after reap.** Mount, then set `unmount` seam to error +
   `checkMountpoint`→`false,nil` (reap path). `Unmount` returns nil, entry
   reaped, AND target is `0o500`.
7. **Shutdown does NOT re-guard (reguard=false).** Mount, then chmod the target
   to a sentinel (e.g. `0o700`) to detect a chmod. `UnmountAllForce(ctx)` with
   seam returning nil. Assert entry gone AND mode is **still** the sentinel
   (no re-guard during shutdown). (Asserts the `reguard=false` wiring.)
8. **`mount remove` restores perms (daemon-level).** `buildTestDaemon`; override
   the mounter seam to allow Mount; pre-mount a target (it becomes `0o500`).
   Drive `onConfigChange(old, new)` where `new` drops the mount (MountsRemoved).
   Assert the target dir is back to `0o755`.
9. **`tryMount` guards a not-mounted target (daemon-level).** Configure a mount
   whose device is offline/unpaired → `tryMount` early-returns. Assert
   `guardTarget` ran: `os.Stat(mc.To).Mode().Perm() == 0o500`.
10. **`guardConfiguredTargets` startup sweep.** `buildTestDaemon` with two
    configured mounts whose dirs are pre-created `0o755`. Call
    `d.guardConfiguredTargets()`. Assert both dirs are `0o500`.
11. **`targetHasLocalContents` unit table.** absent dir → `(false,nil)`; empty
    dir → `(false,nil)`; dir with a file → `(true,nil)`; path that
    `checkMountpoint` says is a mountpoint → `(false,nil)` (do not enumerate).
12. **Existing tests stay green.** `TestMount_CreatesMountPointDirectory` (L393)
    still passes — the dir is created, just at `0o500` not `0o755`; update its
    assertion if it checks mode (it only `os.Stat`s existence today, so it
    passes unchanged). `TestUnmount_*`, `TestUnmountAll*`, `TestUnmountDevice_*`
    keep working with the new `reguard` param (defaulted by the public wrappers).

## Risks & edge cases

- **errcheck STRICT.** Every `os.Chmod`/`os.MkdirAll`/`os.ReadDir` and the
  re-guard call must handle the error explicitly. In `unmountKey` the re-guard
  is `if err := m.guardTarget(...); err != nil { m.logger.Warn(...) }` — never
  blank-discarded, never returned (a re-guard failure must not fail the unmount).
- **`guardTarget` must NOT chmod a live mountpoint.** Chmod-ing while a FUSE
  mount is active would change the mount root's mode, not the underlying dir,
  and could confuse the mount. `guardTarget` is only ever called when the path
  is known-unmounted (pre-mount before `cmd.Start`, on Mount-failure, and after
  `delete` in `unmountKey`). As defense, `guardTarget` may `checkMountpoint`
  first and skip if mounted — but in a wedged-FUSE scenario `Stat` can hang, so
  prefer relying on call-site ordering (we only call it on confirmed-unmounted
  paths) over an extra check that could block. Note this in the code comment.
- **MkdirAll parent vs target mode.** `guardTarget` must create the **target**
  at a sane parent mode but chmod only the **leaf** to `0o500` — do
  `os.MkdirAll(filepath.Dir(path), 0o755)` then `os.Mkdir(path, guardMode)`
  (or `MkdirAll(path, 0o755)` then `os.Chmod(path, guardMode)`). Don't restrict
  parents; a `0o500` parent would block siblings.
- **Owner-write under root / CI.** When tests run as root (some CI), mode bits
  are ignored for access checks — a root `touch` into `0o500` succeeds. So the
  "touch is denied" assertion (test 2 bonus) must be guarded by
  `if os.Geteuid() == 0 { t.Skip(...) }`. The **mode-bit** assertions
  (`Mode().Perm() == 0o500`) are root-safe and are the primary checks.
- **Wedged-FUSE `Stat` in `targetHasLocalContents`.** It guards with
  `checkMountpoint` first; a wedged mountpoint check could hang. Acceptable: this
  runs in `Mount` (pre-mount, path is not yet our mount) and the verify loop
  already bounds Mount. Do not call `targetHasLocalContents` on a path we
  believe is mounted.
- **`HUBFUSE_STUB_MOUNT_DIR` scenario harness.** With the stub, `checkMountpoint`
  always returns true, so `Mount` records a "mount" without a real FUSE mount and
  the underlying `0o500` dir is **not** masked. Stray writes into it would be
  denied — but the stub harness writes via the (fake) mount, not the raw dir, so
  this should be fine. Verify scenario tests still pass; if a scenario test
  writes to the raw target path expecting success, relax via the stub env (the
  stub already short-circuits verification — extend it to skip guarding if
  needed). Call this out for the impl worker to check against
  `tests/integration` / scenario suites.
- **`mountAddCmd` to an existing non-empty dir.** The CLI only edits config; the
  refusal happens when the daemon's `tryMount`/`Mount` runs. The user sees the
  WARN in the daemon log, not at `mount add` time. Acceptable; optionally
  `mountAddCmd` could pre-check, but that splits the logic — keep it in the
  daemon for one source of truth.
- **Daemon offline when `mount remove` edits config.** Dir stays `0o500` until a
  later daemon run unmounts/reaps it (documented above). The startup sweep only
  guards configured targets, so a removed target won't be re-guarded; it simply
  won't be restored until a run observes the removal. Optional thoroughness:
  have the sweep also `unguardTarget` any non-config target it can identify —
  but it has no record of removed mounts, so skip.
- **No `cmd/.../main_test.go`.** Perm-restore coverage lives in
  `daemon_test.go` (the daemon's `onConfigChange`), never in `cmd/hubfuse`.
- **Back-compat.** Public `Unmount`/`UnmountAll`/`UnmountAllForce`/`UnmountDevice`
  signatures are unchanged; only the unexported `unmountKey`/`unmountAll` gain a
  `reguard` param, so all external callers (events.go, confighandler.go,
  daemon.go, integration tests) compile untouched except the two intentional
  additions in confighandler.go.
