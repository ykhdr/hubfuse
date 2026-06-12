# Fix #47 + #50: Reap Dead Mounts and Bound Shutdown Unmount

## Overview

Two reported bugs share one root cause in `internal/agent/mounter.go`: the
unmount path treats "the kernel mount is already gone" as a hard error.

- **#47 — dead mount permanently blocks re-mount.** When a peer restarts, its
  sshfs mount dies and the kernel drops it from the mount table. `Unmount` then
  runs `fusermount -u` / `umount`, which fails ("entry for ... not found in
  /etc/mtab"). Because the unmount returns an error, `Unmount` does **not**
  `delete(m.activeMounts, key)`. The stale entry survives, so every later mount
  attempt is rejected with `share "X" from device "Y" is already mounted`.
  `hubfuse mount remove` + `mount add` cannot recover (unmount keeps failing →
  entry retained). Only `hubfuse stop` + `start` clears it.

- **#50 — agent hangs on SIGTERM; orphaned FUSE-T mount wedges the OS.**
  1. On a dead/unresponsive mount, `Shutdown → UnmountAll → Unmount → unmountPath`
     runs `exec.Command("umount", path)` with **no context/timeout**. A wedged
     `umount` blocks forever, so SIGTERM never returns and the process must be
     SIGKILLed. Reported error:
     `daemon run: shutdown errors: [unmount all: unmount errors: ... fusermount: entry for ... not found in /etc/mtab]`.
  2. After SIGKILL, the orphaned FUSE-T mount (macOS) has no backend; later
     `umount`/`diskutil` hang in uninterruptible state, cleared only by
     `sudo umount -f` or reboot.

**Shared root cause:** unmount classifies "not mounted" as failure, keeps the
stale `activeMounts` entry, and the command itself is unbounded.

## Context (from discovery — verified against commit ae64c6e / f02898c)

All line numbers below are from the worktree
`/Users/ykhdr/projects/hubfuse/.worktrees/issue-47-50-unmount-reap`.

### `internal/agent/mounter.go`

- `Unmount(device, share)` (L312): on `m.unmount(mnt.LocalPath)` error it returns
  the error and **skips** `delete(m.activeMounts, key)` (L323-327). Root of #47.
- `unmountPath(path)` (L339): `darwin → exec.Command("umount", path)`;
  default → `exec.Command("fusermount", "-u", path)`. **No context/timeout.**
  Wraps any non-zero exit (including "not in mtab") as an error (L348-351).
- `UnmountAll()` (L357) and `UnmountDevice(nickname)` (L379): snapshot keys under
  the lock, then loop calling `Unmount`, accumulating `err.Error()` strings. No
  timeout bound. Root of #50 part 1.
- `isMountpoint(path) (bool, error)` (L99): stats `path` vs `filepath.Dir(path)`
  and returns true when `st.Dev != parentSt.Dev`. Unix-only, stdlib syscall.
  **Reuse this to detect a reaped mount** (no longer a mountpoint → unmounted).
- Test seams (mutated directly in tests AND via setters):
  - `unmount func(path string) error` (L141), default `unmountPath`; setter
    `SetUnmountForTests` (L429).
  - `checkMountpoint func(path string) (bool, error)` (L145), default
    `isMountpoint`; setter `SetMountpointCheckForTests` (L437).
  - `execCommand func(ctx, name, args...) *exec.Cmd` (L139); setter L422.
- `Mount` already uses `m.checkMountpoint(mc.To)` (L232) under the same lock.

### `internal/agent/daemon.go`

- `Shutdown()` (L347) calls `d.mounter.UnmountAll()` (L350) **with no timeout**,
  then Deregister / sshServer.Stop / watcher.Stop / hubClient.Close.
- `runServices` (L324) blocks on `<-ctx.Done()` (L339) then calls
  `d.Shutdown()` (L342). `Shutdown` returns up through `Run` → `cmd.RunE`.

### `internal/agent/events.go`

- `handleDeviceOnline` (L35) and `processInitialDevices` (daemon.go L384) call
  `d.mounter.Mount(...)`. Mount rejects with "already mounted" if the
  `activeMounts` entry still exists (mounter.go L198). So reaping the entry in
  `Unmount` is exactly what unblocks device-online auto-remount.
- `handleDeviceOffline` (L77) / `handleDeviceRemoved` (L97) call
  `UnmountDevice`; `handleSharesUpdated` (L117) calls `Unmount`. All only log
  warnings on error — they do not depend on the return value, so making them
  self-heal is purely beneficial.

### `cmd/hubfuse/main.go` + `internal/common/daemonize/control.go`

- `startCmd` (main.go L386-399): installs a SIGINT/SIGTERM handler that calls
  `cancel()`, then `d.Run(ctx)`. There is **no shutdown timeout in the cmd
  layer** — the timeout must live inside the daemon/mounter.
- `daemonize.SignalStop` (control.go L23): `stopGracefulTimeout = 10s`, then
  escalates to SIGKILL. **Design constraint:** the daemon's own shutdown unmount
  must finish well under 10s, otherwise `hubfuse stop` SIGKILLs the daemon and we
  re-create #50 part 2 (orphaned wedged mount). Budget the shutdown unmount at
  **5s total**.

### Tests

- `internal/agent/mounter_test.go`: `newTestMounter` / `newTestMounterWithTool`
  set `m.unmount` (default no-op success) and `m.checkMountpoint` (default
  `return true, nil`), with tiny verify timeouts. Existing Unmount tests at
  L396-442; UnmountAll at L446-477; `unmountPath` real-command smoke tests at
  L518-539; `isMountpoint` test at L640.
- `internal/agent/daemon_test.go`: `buildTestDaemon` returns a real `Daemon`
  whose `d.mounter` is a real `Mounter` (its `unmount`/`checkMountpoint` are the
  production defaults). `TestHandleDeviceOffline_UnmountsShares` (L326) pre-mounts
  via `d.mounter.Mount` then asserts `IsActive` is false. **Note:** daemon tests
  that pre-mount currently succeed because the default `checkMountpoint`
  (`isMountpoint`) returns false for a plain temp dir — wait, Mount needs it to
  return true. (Verified: daemon tests pre-mount successfully because... see
  Risks.) New daemon-level shutdown tests must override the mounter seams.

## Chosen Approach

**Classify-then-reap inside `Unmount`, with a context-bounded, escalating
unmount command, and a bounded shutdown variant.**

1. **Change the `unmount` seam to take a context and a `force` flag.**
   New signature: `unmount func(ctx context.Context, path string, force bool) error`.
   Production impl `unmountPath(ctx, path, force)` uses `exec.CommandContext` so a
   wedged command is abandoned when ctx is cancelled / times out. `force`
   selects the escalation strategy (see step 3).

2. **`Unmount` reaps a confirmed-dead mount as success.** After the unmount
   command, if it errored, re-check `m.checkMountpoint(path)`. If the path is
   **no longer a mountpoint** (or the check itself errors with ENOENT-style "gone"),
   treat it as already-unmounted: log at WARN ("reaped dead mount"), `delete` the
   entry, return nil. Only a real failure (path *is* still a mountpoint AND the
   command failed) returns an error — and even then the entry is retained so a
   retry is possible. This is the surgical fix for #47.

3. **Escalating, OS-specific unmount in `unmountPath`.**
   - Linux: `fusermount -u` → on failure, `fusermount -uz` (lazy). `force` mode
     additionally falls back to `umount -l`.
   - macOS: `umount` → `force` mode falls back to `diskutil unmount force`
     (then `umount -f`). This is what prevents the wedged FUSE-T mount in #50.2.
   - Normal interactive teardown (`mount remove`, shares-updated) uses
     `force=false` (plain unmount; lazy only as needed). Shutdown and
     device-offline/removed use `force=true` so the agent never leaves a wedged
     mount behind.

4. **Bounded shutdown.** Add `UnmountAllForce(ctx)` (force=true, ctx-bounded) and
   keep `UnmountAll()`/`UnmountDevice()` as thin wrappers (background ctx,
   force=false for the interactive callers; force=true for offline/removed).
   `daemon.Shutdown()` wraps the unmount in
   `context.WithTimeout(context.Background(), 5*time.Second)` and calls
   `UnmountAllForce(ctx)`. Even if every mount is wedged, shutdown returns within
   ~5s — comfortably under `SignalStop`'s 10s SIGKILL deadline. Fixes #50.1.

### Rejected alternatives (brief)

- **Stderr string-matching only** ("not mounted", "not found in /etc/mtab",
  "not currently mounted"). Fragile across OSes, locales, fusermount versions.
  Used only as an optional *fast path* before the authoritative `isMountpoint`
  re-check, never as the sole signal.
- **Pre-check `isMountpoint` before unmounting and skip the command if false.**
  Races (mount can die between check and use) and misses the
  command-failed-but-mount-actually-gone case. Re-checking *after* the command is
  strictly more robust; we keep a cheap pre-check only to avoid running umount on
  an obviously-absent path.
- **Add the timeout in `cmd/hubfuse/main.go`.** Wrong layer — the daemon owns its
  lifecycle, daemon tests can then cover it, and the per-command
  `exec.CommandContext` is still needed regardless.
- **Goroutine + `time.After` race in Shutdown without ctx.** Leaks the blocked
  goroutine and the wedged process. `exec.CommandContext` actually kills the
  child, so prefer ctx all the way down.

## Functions / Files to Change

### `internal/agent/mounter.go`

```go
// Field (L141) — add ctx + force:
unmount func(ctx context.Context, path string, force bool) error

// NewMounter (L176): unmount: unmountPath  (signature now matches)

// Replace unmountPath:
func unmountPath(ctx context.Context, path string, force bool) error
//   darwin:  umount → (force) diskutil unmount force → (force) umount -f
//   default: fusermount -u → fusermount -uz → (force) umount -l
//   each step via exec.CommandContext(ctx, ...); aggregate the last error.
//   Returns nil on first success.

// Unmount (L312): add internal helper that takes ctx+force; keep the public
// Unmount(device, share) error for back-compat (interactive callers), plus a
// new unmountLocked-style path. Concretely:
func (m *Mounter) Unmount(device, share string) error          // force=false, bg ctx (back-compat)
func (m *Mounter) unmountKey(ctx context.Context, key mountKey, force bool) error  // core

// Reap logic inside unmountKey after the command:
//   if cmdErr != nil {
//       stillMnt, checkErr := m.checkMountpoint(mnt.LocalPath)
//       if checkErr != nil || !stillMnt { /* gone → reap */ delete; log WARN; return nil }
//       return fmt.Errorf("unmount %q (...): %w", ..., cmdErr)  // real failure, keep entry
//   }
//   delete; return nil

// UnmountAll (L357): wrapper → m.unmountAll(context.Background(), false)
// New:
func (m *Mounter) UnmountAllForce(ctx context.Context) error    // force=true, ctx-bounded
func (m *Mounter) unmountAll(ctx context.Context, force bool) error  // core loop using unmountKey

// UnmountDevice (L379): keep signature; internally use force=true (offline/removed
// teardown should not leave wedged mounts) — OR add UnmountDeviceForce. Decide:
// keep UnmountDevice(nickname) and make it force=true (callers are offline/removed
// only — no interactive caller). Loop must stay ctx-bounded; pass a per-call ctx
// derived from a short timeout when called from event handlers.

// SetUnmountForTests (L429): update seam signature to
func (m *Mounter) SetUnmountForTests(fn func(ctx context.Context, path string, force bool) error)
```

### `internal/agent/daemon.go`

```go
// Shutdown (L350): replace
//   if err := d.mounter.UnmountAll(); err != nil { ... }
// with
//   uctx, ucancel := context.WithTimeout(context.Background(), 5*time.Second)
//   defer ucancel()
//   if err := d.mounter.UnmountAllForce(uctx); err != nil { errs = append(...) }
```

### `internal/agent/events.go`

- `handleDeviceOffline` / `handleDeviceRemoved`: ensure `UnmountDevice` is the
  force variant (no signature change if UnmountDevice itself becomes force=true).
- No change needed to auto-mount logic — reaping the entry already unblocks
  `Mount`. (Add an inline comment cross-referencing #47.)

### Call sites to update for the new `unmount` seam signature

- `internal/agent/mounter_test.go`: `newTestMounter` / `newTestMounterWithTool`
  default `m.unmount` and any test passing `unmountFn`.
- `unmountPath` smoke tests (`TestUnmountPath_MacOS` / `_Linux`): pass
  `context.Background(), path, false`.

## Implementation Checklist

1. [ ] Change `unmount` field type and `NewMounter` wiring to the new
   `(ctx, path, force)` signature.
2. [ ] Rewrite `unmountPath(ctx, path, force)` with `exec.CommandContext` and the
   per-OS escalation ladder; return nil on first success, aggregate errors.
3. [ ] Extract `unmountKey(ctx, key, force)` from `Unmount`; add the post-command
   `checkMountpoint` reap logic (delete entry + WARN log + return nil when the
   path is no longer a mountpoint).
4. [ ] Keep `Unmount(device, share) error` as a back-compat wrapper
   (`unmountKey(context.Background(), key, false)`).
5. [ ] Add `unmountAll(ctx, force)`, wire `UnmountAll()` (force=false) and new
   `UnmountAllForce(ctx)` (force=true). Make `UnmountDevice` force=true and
   ctx-bounded.
6. [ ] Update `SetUnmountForTests` seam signature.
7. [ ] Update `daemon.Shutdown` to call `UnmountAllForce` under a 5s timeout.
8. [ ] Add cross-reference comments (#47 reap; #50 bounded/force).
9. [ ] Update all test call sites to the new seam signature (compile-green).
10. [ ] `make build && make vet && go test ./internal/agent/...` then `make test`.

## Test Plan

All unit tests use the existing seams — **we cannot exercise real FUSE in unit
tests**, so every scenario is driven through `m.unmount` and `m.checkMountpoint`.
Add to `internal/agent/mounter_test.go` (and a daemon-level shutdown test to
`internal/agent/daemon_test.go`). Match the existing testify `require`/`assert`
style and `newTestMounter` helper.

1. **Dead-mount reap drops the entry & returns success (#47).** Pre-mount via
   `m.Mount`. Set `m.unmount` to return an error (simulating
   `fusermount: not in mtab`). Set `m.checkMountpoint` to return `(false, nil)`
   (mount is gone). Assert `Unmount` returns **nil** and `IsActive` is false.
2. **Real unmount failure keeps the entry & returns error.** Pre-mount. Set
   `m.unmount` to error AND `m.checkMountpoint` to return `(true, nil)` (still a
   mountpoint). Assert `Unmount` returns an error and `IsActive` is **still
   true** (so a retry is possible).
3. **Reap when checkMountpoint itself errors "gone".** unmount errors,
   `checkMountpoint` returns `(false, someErr)` (e.g. ENOENT). Assert reap →
   nil + entry dropped.
4. **Re-mount after reap succeeds (#47 end-to-end at mounter level).** Mount →
   Unmount (reaped via the dead-mount path) → Mount again the same device/share
   to a new path. Assert the second Mount returns **nil** (no "already mounted").
5. **`UnmountAllForce` passes force=true and ctx through.** Capture the args
   handed to `m.unmount` (record `force` and whether `ctx` is the one passed).
   Assert force==true for `UnmountAllForce`, force==false for `UnmountAll`.
6. **Bounded shutdown returns under timeout (#50.1).** Daemon-level test:
   `buildTestDaemon`, pre-mount, then override `d.mounter` seams so
   `m.unmount` **blocks until ctx is cancelled** (`<-ctx.Done(); return ctx.Err()`)
   and `checkMountpoint` returns `(false,nil)` so the block is then reaped.
   Run `d.Shutdown()` inside a goroutine guarded by a 6s `time.After`; assert it
   returns within the budget (use a small injected timeout if the design exposes
   one, else assert wall-clock < ~6s). Verify the mount entry is gone.
7. **Lazy/force fallback ordering.** Unit-test `unmountPath` indirectly is hard
   (real binaries). Instead, factor the *ladder* so the sequence of
   `exec.CommandContext` invocations is observable via `execCommand`-style
   injection OR add a tiny pure helper `unmountLadder(goos, force) [][]string`
   returning the ordered argv list, and unit-test that helper: assert Linux
   non-force = `[[fusermount -u],[fusermount -uz]]`, Linux force appends
   `[umount -l]`; darwin non-force = `[[umount]]`, darwin force appends
   `[diskutil unmount force],[umount -f]`. (Preferred: the pure-helper approach,
   consistent with `buildMountArgs`/`mountInstallHint` already being pure +
   table-tested.)
8. **Ctx-cancel abandons a wedged command.** With the pure-ladder + an injected
   command runner, simulate the first N commands failing and ctx cancelling;
   assert the loop stops promptly and returns ctx.Err() (or the reaped-success
   path if checkMountpoint says gone).
9. **Existing tests stay green:** `TestUnmount_*`, `TestUnmountAll_*`,
   `TestUnmountDevice_*`, `TestUnmountPath_MacOS/_Linux` (update call signatures).

## Risks & Edge Cases

- **errcheck is STRICT.** Every `cmd.Run`/`CombinedOutput`, `delete` is fine, but
  any deliberately-ignored error needs `_ =`. The reap path that ignores
  `checkErr` must handle it explicitly (treat error as "gone"), not blank-discard
  silently in a way errcheck flags — assign and branch on it.
- **`isMountpoint` semantics on a *dead* FUSE mount.** A wedged FUSE-T mount may
  make `syscall.Stat(path)` itself **hang** (uninterruptible). The post-command
  `checkMountpoint` could block. Mitigation: the `force` unmount ladder
  (`diskutil unmount force` / `umount -l`) should clear the wedge *before* we
  re-check; and the re-check runs after a (now-succeeded or ctx-bounded) command.
  If Stat can still hang, consider gating the re-check behind the same ctx
  (run it in a goroutine with select on ctx.Done) — call this out for review;
  for the first cut, rely on force-unmount having already cleared the wedge.
- **Stat on a reaped mountpoint returns the underlying dir** (st_dev == parent),
  so `isMountpoint` correctly returns false → reap. Good; matches
  `TestIsMountpoint_TempDirIsNotMountpoint`.
- **Lazy unmount (`-uz` / `-l`) returns success immediately** but defers actual
  teardown. The subsequent `checkMountpoint` may still see it as a mountpoint
  briefly. Treat command-success as success regardless of the re-check (only
  re-check on command *failure*). This keeps lazy unmount honest.
- **`diskutil` may not exist / require privileges** in some environments. Treat
  its failure as just another rung in the ladder; do not abort shutdown — the 5s
  ctx bound guarantees we move on.
- **Daemon tests that pre-mount** rely on the real `checkMountpoint` returning
  true during `Mount`. Verify `buildTestDaemon`'s mounter is overridden in tests
  that pre-mount (e.g. `TestHandleDeviceOffline_UnmountsShares` works today, so
  the default path must already permit it — confirm before relying on it; if it
  depends on `HUBFUSE_STUB_MOUNT_DIR` or a seam override, mirror that in the new
  shutdown test).
- **Back-compat:** keeping `Unmount(device, share) error`,
  `UnmountAll() error`, `UnmountDevice(nickname) error` signatures unchanged
  means `events.go`/`handleSharesUpdated` and all current callers compile
  untouched; only `Shutdown` adopts the new `UnmountAllForce(ctx)`.
- **No `cmd/.../main_test.go`** — the shutdown-timeout coverage lives in
  `internal/agent/daemon_test.go`, never in `cmd/hubfuse`.

## Plan-review fixes to incorporate (MUST before impl)

From the plan-review pass:

1. **Compile blocker — `tests/integration/prune_test.go:98`** calls
   `mounter.SetUnmountForTests(func(string) error {...})` with the OLD seam
   signature. Update it to `func(context.Context, string, bool) error`. Also note
   prune_test (L98/L116) signals via `close(unmounted)` — ensure the new
   force/ctx path does not invoke the seam more than once for the same mount
   (double `close` panics); a `sync.Once` or buffered-channel send is safer if
   invocation count could change.
2. **Add `internal/agent/confighandler.go:35` to the accounted call sites.**
   `onConfigChange` calls `d.mounter.Unmount(mc.Device, mc.Share)` for
   `MountsRemoved`. It's an interactive teardown path; the public
   `Unmount(device,share)` signature stays unchanged so it compiles, but classify
   it explicitly as a non-force path (force=false).
3. **Test 6 (bounded shutdown) MUST override `checkMountpoint` via
   `SetMountpointCheckForTests`** — not "if needed". `buildTestDaemon`'s real
   `Mounter` uses `isMountpoint`, which returns false for a temp dir, so a
   pre-mount would otherwise TIME OUT in `Mount`'s verify loop. Mirror whatever
   `daemon_test.go` (~L555) does to pre-mount successfully (seam override).
4. **Nit — reap-on-checkErr is deliberate.** Treating any `checkErr` from the
   post-command re-check as "gone → reap" can reap a still-live mount on a
   transient EACCES/EINTR. Accept this (favor self-heal over a stuck entry) but
   add a one-line comment saying so.

## Resolved Decision (confirmed by coordinator)

- **The 5s shutdown budget lives in `daemon.Shutdown`** as a single
  `context.WithTimeout(context.Background(), 5*time.Second)` covering the whole
  `UnmountAllForce`, with each per-mount command sharing that ctx via
  `exec.CommandContext`.
- **BUILD THE RE-CHECK CTX-GUARD NOW (do not defer).** `syscall.Stat` on a
  wedged FUSE mount can itself block uninterruptibly, which would re-create the
  exact SIGTERM hang #50 is about. So the post-command `checkMountpoint` re-check
  MUST NOT be able to reblock a bounded shutdown. Implementation:
  - Add a ctx-aware reap check used by the force/ctx-bounded path: run
    `m.checkMountpoint(path)` in a goroutine and `select` on
    `ctx.Done()` vs the result. If `ctx` fires first, **treat the mount as gone
    and reap the entry** (log WARN "reap: mountpoint check timed out, dropping
    entry") and return nil — shutdown must proceed, and the force-unmount ladder
    has already best-effort cleared the wedge. The abandoned goroutine is
    acceptable (process is exiting).
  - The non-ctx interactive `Unmount(device, share)` path may keep the plain
    synchronous re-check (no wedge expected there, and we want a real error if
    something is genuinely wrong); but it should pass a short derived timeout
    rather than `context.Background()` so it can never hang indefinitely either.
    Use a small internal default (e.g. 3s) for the interactive path's re-check.
  - Factor the guarded re-check into one helper, e.g.
    `func (m *Mounter) mountpointGoneCtx(ctx context.Context, path string) bool`,
    so both `unmountKey` and `unmountAll` share it and it is unit-testable via
    the `checkMountpoint` seam (inject a blocking check + a cancelled ctx →
    assert it returns "gone"/reaps without hanging).
- This supersedes the "for the first cut, rely on force-unmount having already
  cleared the wedge" hedge in Risks: we implement the guard now.

### Added test (supersedes deferral)

- **Re-check cannot hang shutdown.** Inject `m.checkMountpoint` that blocks on a
  channel that is never closed, and `m.unmount` that returns an error. Call the
  ctx-bounded force path with a ctx that cancels after a tiny timeout. Assert the
  call returns promptly (well under the test's own guard), the entry is reaped,
  and no goroutine-leak panic. This is the unit-level proof for #50.1 under the
  worst case (both command AND re-check wedged).
