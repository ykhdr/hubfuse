# Fix issue #46: stat of nonexistent path must return SSH_FX_NO_SUCH_FILE, not PERMISSION_DENIED

## Overview

- **Problem**: the embedded SFTP server returns `SSH_FX_PERMISSION_DENIED` for
  Lstat/Stat of paths that simply do not exist inside an allowed share. An
  sshfs client performs a kernel lookup (LSTAT) before every create; getting
  EACCES instead of ENOENT means the kernel never obtains a negative dentry, so
  `open(O_CREAT)` and `mkdir` fail client-side with `Permission denied` and no
  `SSH_FXP_OPEN(create)`/`SSH_FXP_MKDIR` is ever sent
  ([issue #46](https://github.com/ykhdr/hubfuse/issues/46)).
- **Fix**: map "path does not exist" resolution failures to
  `sftp.ErrSSHFxNoSuchFile` while keeping every ACL/escape failure as
  `SSH_FX_PERMISSION_DENIED`.
- **Benefit**: create/mkdir/rename-into-share work over sshfs; honest POSIX
  semantics for all SFTP clients.

## Context (from discovery)

- Root cause: `resolveReadReal` (`internal/agent/sftphandler.go:80-83`) and
  `resolveWriteReal` (`internal/agent/sftphandler.go:104-107`) convert **any**
  error from `containedReal`/`containedWritePath` into `denied()`.
  `containedReal` (`internal/agent/sharesacl.go:132`) uses
  `filepath.EvalSymlinks`, which fails with `*fs.PathError` wrapping
  `syscall.ENOENT` when the path (or any component) does not exist yet.
- `pkg/sftp` v1.13.10 maps `os.IsNotExist` errors → `SSH_FX_NO_SUCH_FILE`
  (`server.go:636`, `statusFromError`); the typed sentinel
  `sftp.ErrSSHFxNoSuchFile` is available to handlers.
- Escape attempts (the `filepath.Rel` containment checks) produce custom
  `fmt.Errorf` errors — `errors.Is(err, fs.ErrNotExist)` is false for them, so
  they naturally remain denied.
- Alias/ACL-level failures (unknown share, peer not allowed, read-only share)
  are early returns before path resolution and must remain `denied()` —
  share-alias existence must not leak to unauthorized peers.
- Test conventions in `internal/agent/sftphandler_test.go`: testify
  `assert`/`require`, `mkACLHandlers`, `stubResolver`, `sftp.NewRequest`,
  `t.TempDir()`.

## Design decision (brainstormed)

Chosen: **central error-mapping helper** (option B of the brainstorm).
A `resolveErr(err)` helper next to `denied()`: returns
`sftp.ErrSSHFxNoSuchFile` when `errors.Is(err, fs.ErrNotExist)`, otherwise
`denied()`. Applied at the two `containedReal`/`containedWritePath` error
branches.

Accepted trade-off: a **dangling symlink** leaf (or a symlink chain ending in
a nonexistent target) now reports `NO_SUCH_FILE` instead of
`PERMISSION_DENIED`. This is POSIX-honest — `stat()` of a dangling symlink is
`ENOENT` on a real filesystem — and safe: peers cannot plant symlinks
(`Filecmd` rejects the `Symlink` method unconditionally,
`sftphandler.go:194`), so the theoretical outside-share existence oracle is
limited to symlinks the share owner planted themselves.

Rejected alternatives: (A) extra `os.Lstat`-based disambiguation of dangling
symlinks — extra syscall and branching to defend against a non-threat; (C)
full component-walking resolver with proper Lstat-no-follow semantics —
rewrite disproportionate to the bug.

## Development Approach

- **Testing approach**: TDD — write the failing tests first, watch them fail
  (red), apply the fix, watch them pass (green).
- Complete each task fully before moving to the next; small focused changes.
- Every task includes new/updated tests; all tests must pass before the next
  task starts.
- Update this plan file if scope changes during implementation.

## Testing Strategy

- Unit tests in `internal/agent/sftphandler_test.go` (table follows existing
  per-scenario test functions style).
- No e2e harness in repo for sshfs kernel behavior; live two-machine
  verification (Mac share + Ubuntu sshfs client) happens after implementation
  as a separate task outside this plan (see Post-Completion).

## Implementation Steps

### Task 1: map not-exist resolution errors to SSH_FX_NO_SUCH_FILE

**Files:**
- Modify: `internal/agent/sftphandler.go`
- Modify: `internal/agent/sftphandler_test.go`

- [x] write failing tests (TDD red):
  - `Filelist` `Lstat` of nonexistent leaf in an allowed share →
    `sftp.ErrSSHFxNoSuchFile`
  - `Filelist` `Stat` of nonexistent leaf → `sftp.ErrSSHFxNoSuchFile`
  - `Filelist` `Stat` of path with nonexistent intermediate directory
    (`/docs/sub/x`) → `sftp.ErrSSHFxNoSuchFile`
  - `Fileread` of nonexistent file → `sftp.ErrSSHFxNoSuchFile`
  - `Filecmd` `Mkdir` under a nonexistent parent (`/docs/missing/newdir`) →
    `sftp.ErrSSHFxNoSuchFile`
  - dangling symlink leaf: `Filelist` `Stat` → `sftp.ErrSSHFxNoSuchFile`
    (documents the accepted trade-off)
  - guard regressions: existing-file `Stat` still succeeds; symlink-escape
    to an **existing** outside file still `ErrSSHFxPermissionDenied`; unknown
    alias still denied (already covered, keep passing); RO-share write still
    denied (already covered, keep passing)
- [x] run `go test ./internal/agent/ -run TestACLHandlers` — new tests must
  fail with `ErrSSHFxPermissionDenied` (red)
- [x] add `resolveErr(err error) error` helper next to `denied()` in
  `sftphandler.go`: `errors.Is(err, fs.ErrNotExist)` →
  `sftp.ErrSSHFxNoSuchFile`, otherwise `denied()`; document why alias/ACL
  failures must stay denied (requires new imports `errors` and `io/fs` in
  `sftphandler.go` — currently only `io` is imported)
- [x] replace `return "", denied()` with `return "", resolveErr(err)` at the
  `containedReal` branch of `resolveReadReal` and the `containedWritePath`
  branch of `resolveWriteReal` (only these two call sites)
- [x] run `go test ./internal/agent/ -run TestACLHandlers` — all green

### Task 2: verify acceptance criteria

- [x] `make build` passes
- [x] `make vet` passes
- [x] `make test` (unit + integration) passes
- [x] re-read issue #46 symptom list and confirm each maps to a covered code
  path (create new file, mkdir, in-place writes unaffected, ACL still
  enforced)

### Task 3: documentation and plan archival

- [x] no README/CLAUDE.md changes expected (internal bug fix); confirmed
- [x] move this plan to `docs/plans/completed/`

## Post-Completion

**Live two-machine verification** (separate task in session task list):
- Baseline: reproduce EACCES on the live pair (Ubuntu `~/claude/remote` →
  Mac share) before installing the fix.
- Install the fixed `hubfuse` build on the **Mac** (sharing side runs the
  embedded SFTP server), restart agent, re-run:
  `echo z > ~/claude/remote/newfile.txt && mkdir ~/claude/remote/newdir` →
  both must succeed; verify in-place append/truncate still work; verify a
  read-only share still rejects writes.

**External follow-ups:**
- PR to `master` referencing issue #46 with live-verification evidence.
