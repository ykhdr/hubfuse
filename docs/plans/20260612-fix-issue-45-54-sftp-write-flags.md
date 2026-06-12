# Fix issues #45 + #54: SFTP write-open pflags mistranslation corrupts and loses data

## Overview

- **Bug A (#54, append loss)**: `openFlagsForRequest` maps the client's SFTP
  `APPEND` pflag to `os.O_APPEND`. pkg/sftp's request server delivers every
  write via `io.WriterAt.WriteAt(data, offset)`, and Go's `os.File.WriteAt`
  always fails on `O_APPEND` handles
  (`os: invalid use of WriteAt on file opened with O_APPEND`). Every write
  into an append-opened handle errors with `SSH_FX_FAILURE`; sshfs clients
  report `rc=0` (bash ignores deferred write/close errors) and the data
  silently vanishes.
- **Bug B (#45, NUL-prefix corruption)**: the bare-write fallback
  `if !p.Creat && !p.Trunc && !p.Excl && !p.Append { flags |= os.O_CREATE |
  os.O_TRUNC }` fires for legitimate non-truncating write-opens (pflags
  `READ|WRITE` or `WRITE` — exactly what the FUSE-T/NFS path and any
  pwrite-style client sends), truncating the existing file; the subsequent
  offset write leaves a NUL prefix.
- Both bugs proven live against the running server (no FUSE involved):
  paramiko `r+` open + `write("BBB\n")` at offset 4 on a seeded `AAA\n` file
  produced literal `\0\0\0\0BBB\n` on disk — the exact #45 signature; paramiko
  append-mode write failed with the exact Go `WriteAt` error (#54); sshfs
  debug trace showed the WRITE status failure.

## Context (from discovery)

- Single production site: `openFlagsForRequest`
  (`internal/agent/sftphandler.go:115-147`, on the fix/issue-46 base) used
  only by `Filewrite` (`sftphandler.go:196`).
- `sftp.Request.Flags` is an exported `uint32`; tests can set raw pflag bits.
  SFTP pflag bit values: READ=0x1, WRITE=0x2, APPEND=0x4, CREAT=0x8,
  TRUNC=0x10, EXCL=0x20.
- pkg/sftp v1.13.10 **client** (`client.go:2263`, `toPflags`) maps
  `os.O_APPEND` → APPEND pflag but tracks file offsets **from 0** (no
  seek-to-EOF on open) — i.e. library clients rely on server-side APPEND
  positioning. OpenSSH's sftp-server likewise ignores offsets on
  append-opened handles and writes at EOF. sshfs, by contrast, always sends
  correct kernel-computed offsets.
- `internal/agent/sshserver_test.go` has an SSH-dial harness but no SFTP
  client harness; per YAGNI no new e2e infrastructure in this plan.
- Branch `fix/issue-45-54-sftp-write-flags` is stacked on
  `fix/issue-46-stat-no-such-file` (PR #55) — same file, independent hunks.

## Design decision (brainstormed)

Honor the client's pflags exactly, and implement APPEND the way the
reference server (OpenSSH sftp-server) does:

1. Access bits unchanged: `Read && Write` → `O_RDWR`; `Write` → `O_WRONLY`;
   degenerate write-class default → `O_WRONLY`.
2. `Creat`/`Trunc`/`Excl` map 1:1 — **no implicit `O_TRUNC`/`O_CREATE`** for
   explicit write-opens. A client that opens an existing file for plain
   write keeps the existing bytes (fixes #45).
3. Keep the legacy `O_CREATE|O_TRUNC` fallback **only** for the fully
   degenerate case (neither Read nor Write pflag set — the "bare Put"
   contract some minimal clients use). Any explicit pflags are honored
   literally.
4. APPEND handles: open **with** `O_APPEND`, but return an
   `appendOnlyWriter` wrapper whose `WriteAt(p, off)` ignores the client
   offset and performs a mutex-serialized `f.Write(p)` (kernel-atomic
   EOF append). This matches SFTP spec semantics (`SSH_FXF_APPEND`: writes
   always land at EOF) and OpenSSH behavior, and supports **both** client
   classes: offset-correct streamers (sshfs) and offset-from-0 library
   clients (pkg/sftp). The wrapper must forward `Close` (pkg/sftp closes
   handles via `io.Closer` when implemented).

Rejected alternatives:
- *Drop `O_APPEND`, trust client offsets*: fixes sshfs but silently
  corrupts pkg/sftp-style clients that send offset 0 and rely on the
  APPEND pflag — worse than the current loud failure.
- *EOF-forcing without `O_APPEND` at the fd level*: loses kernel append
  atomicity against concurrent local writers for no benefit.

Accepted trade-off: concurrent out-of-order WRITE packets on one append
handle land in arrival order, not offset order — identical to OpenSSH
sftp-server; single-streamer appends (the real-world case) are unaffected.

## Development Approach

- **Testing approach**: TDD — failing tests first (red), then the fix
  (green).
- Small focused changes; every task carries tests; all tests pass before
  the next task; update this plan if scope changes.

## Testing Strategy

Unit tests in `internal/agent/sftphandler_test.go` (testify,
`mkACLHandlers`, `newRequest`, set `req.Flags` raw bits, `t.TempDir()`).
Live two-machine verification happens after implementation as a separate
session task (see Post-Completion).

## Implementation Steps

### Task 1: rework openFlagsForRequest + appendOnlyWriter

**Files:**
- Modify: `internal/agent/sftphandler.go`
- Modify: `internal/agent/sftphandler_test.go`

- [ ] write failing tests (TDD red):
  - `r+` offset write preserves prefix: seed `AAA\n`; `Filewrite` with
    `Flags=READ|WRITE`; `WriteAt("BBB\n", 4)`; close → file bytes exactly
    `AAA\nBBB\n` (red on base: `\0\0\0\0BBB\n`)
  - plain `WRITE`-only open of existing file does not truncate: seed
    content, `Flags=WRITE`, `WriteAt` at end offset → prefix intact
  - append flow (offset-correct client): seed `AAA\n`;
    `Flags=WRITE|CREAT|APPEND` → `WriteAt("BBB\n", 4)` succeeds (red on
    base: WriteAt error) → file `AAA\nBBB\n`
  - append flow (offset-zero client): same handle semantics — seed
    `AAA\n`; `Flags=WRITE|APPEND`; `WriteAt("BBB\n", 0)` → bytes land at
    EOF → `AAA\nBBB\n` (documents spec/OpenSSH semantics: offsets ignored
    on append handles)
  - upload semantics preserved: seed old content;
    `Flags=WRITE|CREAT|TRUNC`; `WriteAt` at 0 → file replaced exactly
  - `WRITE` without `CREAT` on nonexistent file → `errors.Is(err,
    fs.ErrNotExist)` (raw os error; statusFromError → NO_SUCH_FILE on the
    wire)
  - `EXCL` honored: existing file; `Flags=WRITE|CREAT|EXCL` →
    `errors.Is(err, fs.ErrExist)`
  - degenerate empty pflags (`Flags=0`) → legacy fallback still
    creates+truncates (documents why the fallback is kept)
- [ ] run `go test ./internal/agent/ -run TestACLHandlers -count=1` —
  new tests fail as predicted (red)
- [ ] rewrite `openFlagsForRequest`: honor pflags exactly; fallback
  `O_CREATE|O_TRUNC` only when neither Read nor Write is set; keep
  `O_APPEND` for append opens; update the doc comment to state the
  WriteAt constraint and the OpenSSH-equivalent append semantics
- [ ] add `appendOnlyWriter` (struct with `*os.File` + `sync.Mutex`):
  `WriteAt` ignores the offset, serializes `f.Write(p)`; forwards
  `Close`; doc comment explains why offsets are deliberately ignored
  (SSH_FXF_APPEND spec semantics, OpenSSH parity, pkg/sftp-client
  offset-0 behavior)
- [ ] return `appendOnlyWriter` from `Filewrite` when `p.Append` is set;
  verify pkg/sftp request server closes handles via `io.Closer`
- [ ] run `go test ./internal/agent/ -run TestACLHandlers -count=1` —
  all green

### Task 2: verify acceptance criteria

- [ ] `make build`, `make vet`, `make test` all pass
- [ ] re-read #45 and #54 symptom lists; confirm each maps to a covered
  code path (append via sshfs, offset write via NFS/FUSE-T path, plain
  upload, create-new)

### Task 3: documentation and plan archival

- [ ] README "Share access control" section: no change needed (behavior
  now matches what it already documents); confirm
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion

**Live two-machine verification** (session task, both directions):
- sshfs direction (Linux → Mac share): re-run #54 repro — `>>` append must
  persist; truncate-write and create/mkdir (#46) must keep working.
- FUSE-T direction (Mac `mount-tool "fuse-t"` → server share at
  `~/cloud/server`, fixed build on the **server** side): re-run the exact
  #45 repro (`printf AAA >f; printf BBB >>f`) — must yield `AAA\nBBB\n`
  with no NULs and correct stat size.
- paramiko probe (r+ offset write + append mode) re-run against the fixed
  server — both must round-trip.

**External follow-ups:**
- PR closing #45 and #54 (stacked on PR #55), with live evidence.
- Update issue #45: root cause was hubfuse-side; FUSE-T `backend=smb`
  workaround not needed (keep as a note for genuinely upstream NFS quirks).
