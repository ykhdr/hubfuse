# Issue #32 — Authenticate `Join` via hub-issued join tokens

## Problem

`Hub.Join` (`internal/hub/server.go:33`) accepts any `(device_id, nickname)` pair
over the open gRPC port and returns a freshly signed client certificate. Anyone
who can reach `:9090` becomes a first-class hub member. The deployment story
("trusted LAN") is not an enforceable invariant: port-forwarding, VPN mesh
misconfigs, or a rogue device on the LAN break it.

## Goal

`Join` requires the caller to present a short-lived, single-use **join token**
issued out-of-band by the hub administrator. Joining without a valid token is
rejected before any state mutation or cert signing.

## Non-goals

- Bootstrap password / shared-secret mode (Issue's option 2)
- Interactive "hub admin approves request" flow (option 3)
- Admin auth/ACL on the `issue-join` CLI itself — it runs on the hub host, so
  OS-level access control is the boundary
- Migration framework for SQLite. We add a new `CREATE TABLE IF NOT EXISTS`
  statement; existing hubs pick it up on next start.

## Design

### Data model

New SQLite table `pending_join_tokens`:

| column        | type       | notes                                           |
|---------------|------------|-------------------------------------------------|
| `token`       | TEXT PK    | `HUB-XXX-YYY` format (same as invite codes)     |
| `expires_at`  | TIMESTAMP  | UTC, enforced atomically in the claim statement |
| `attempts`    | INTEGER    | retained for schema compatibility; unused       |
| `created_at`  | TIMESTAMP  | for audit / expiry sweeping                     |

Token format and alphabet reuse `GenerateInviteCode` from
`internal/hub/pairing.go:149` — 6 random chars across A-Z0-9 with bias-free
rejection sampling. ~2.2 billion tokens; unique-per-TTL. 10-minute TTL matches
pairing invites, configurable via hub config.

### Store interface additions

`internal/hub/store/store.go`:

```go
CreateJoinToken(ctx, *JoinToken) error
GetJoinToken(ctx, token string) (*JoinToken, error)   // diagnostic only
ClaimJoinToken(ctx, token string, now time.Time) (bool, error)
DeleteExpiredJoinTokens(ctx) error                    // sweeper
```

`JoinToken` model parallels `PendingInvite` in `store/models.go`.

`ClaimJoinToken` is the sole authority for Join access. It runs a single
atomic statement:

```sql
DELETE FROM pending_join_tokens WHERE token = ? AND expires_at > ?
```

and returns `(RowsAffected == 1, nil)`. Under concurrent Joins holding the
same token the database guarantees exactly one `true` return. A separate
attempt counter / retry cap is intentionally **not** implemented — single
use by first claim is simpler and more secure (see "Why first-attempt claim"
below).

### Registry / RPC changes

- `Registry.IssueJoinToken(ctx) (string, time.Time, error)` — generates a
  token, stores it, returns the code and expiry timestamp.
- `Registry.Join` now takes an extra `joinToken string` parameter. Validation
  order (before touching devices):
  1. `joinToken != ""` → else `common.ErrInvalidJoinToken`.
  2. `store.ClaimJoinToken(ctx, joinToken, now)` → `true` means access
     granted. `false` → diagnostic `GetJoinToken` to distinguish expired
     (`common.ErrJoinTokenExpired`) from missing/already-claimed
     (`common.ErrInvalidJoinToken`).
  3. Nickname uniqueness check (existing behavior).
  4. Create device, sign cert, return.
- There is no post-success deletion step — the claim already deleted the row.

#### Why first-attempt claim (rather than "delete on success")

Any scheme that keeps the token alive across failures must either (a) lock
the row for the duration of Join (requires a long-lived transaction around
non-DB work such as cert signing), or (b) increment a counter to detect
reuse (leaves a brute-force surface; also race-prone, as Copilot noted).
Consuming on the first claim closes both problems atomically: one DELETE
is the security boundary, everything after is best-effort. A Join that
fails post-claim (nickname collision, internal error) simply requires the
operator to issue a fresh token — the failure mode is explicit and local.

### Proto change (`proto/hubfuse.proto`)

```proto
message JoinRequest {
  string device_id = 1;
  string nickname  = 2;
  string join_token = 3;  // NEW, required
}
```

Adding a field is wire-compatible for already-deployed clients but
**server-side we reject empty `join_token`** — old clients break intentionally
(this is a security fix, not a compatibility one). Bump
`common.ProtocolVersion` so mismatched versions surface a clear error in
`Register`.

### Hub CLI

New subcommand under `cmd/hubfuse-hub/`:

```
hubfuse-hub issue-join [--data-dir DIR] [--ttl 10m]
```

Opens the hub's SQLite store directly (same `data-dir` default as `start`),
inserts a token, prints the code on stdout (one line, no decoration — pipe-
friendly), and exits. Intended to run on the hub host while `start` is running;
SQLite WAL mode supports concurrent readers/writers.

Also print a human-friendly hint to stderr:

```
Share this token with the joining device. Expires at 2026-04-22T15:23:04Z.
```

### Hub config (KDL, `configfile.go`)

New optional node:

```kdl
join-token-ttl "10m"
```

Defaults to `10m` when absent. Plumbed into `Registry` via `Hub` orchestrator.

### Agent CLI (`cmd/hubfuse/main.go`)

```
hubfuse join <hub-address> --token HUB-XXX-YYY
```

`--token` is required. When absent, `cobra` flag machinery errors out.
`hubClient.Join` is extended with the token argument.

### Error surface (`internal/common/errors.go`)

New sentinel errors:

- `ErrInvalidJoinToken`   ("invalid join token")
- `ErrJoinTokenExpired`   ("join token expired")
- `ErrJoinTokenConsumed`  — not needed; consumed tokens return `ErrInvalidJoinToken`

Reuse `ErrMaxAttemptsExceeded`.

### Expiry sweep

`Hub.Start` already launches a heartbeat loop. Add a ticker (60s) that calls
`store.DeleteExpiredJoinTokens`. Keeps the table bounded even if tokens are
never redeemed.

## Test plan

Unit (`internal/hub/`):

- `Registry.IssueJoinToken` stores a unique token with correct TTL.
- `Registry.Join` accepts a valid token, then rejects it on second use
  (`ErrInvalidJoinToken`).
- Expired token → `ErrJoinTokenExpired`.
- 5 failed attempts → `ErrMaxAttemptsExceeded`.
- Empty `joinToken` → `ErrInvalidJoinToken`.

Store (`internal/hub/store/sqlite_test.go`):

- Round-trip `CreateJoinToken` / `GetJoinToken`.
- `DeleteExpiredJoinTokens` only deletes rows with `expires_at < now`.

CLI (`cmd/hubfuse-hub/`):

- `issue-join` prints a token matching `^HUB-[A-Z0-9]{3}-[A-Z0-9]{3}$` and the
  token is queryable from the store.

Integration (`tests/integration/`):

- Happy path: issue token → agent `Join` with token succeeds → token is gone
  from store.
- Agent `Join` with empty token / wrong token → RPC returns `Success=false`.
- Second `Join` reusing a consumed token → fails.

Scenario (`tests/scenarios/`, if stage-2 fixtures fit):

- `join-token.txtar` — run `hubfuse-hub start`, `hubfuse-hub issue-join`,
  capture token, run `hubfuse join --token $TOKEN`, assert success.

## README update

Add a "Joining a device" subsection stating that Join requires a hub-issued
token and showing the two commands. Remove / rephrase the "unauthenticated
Join" language.

## Risk / blast radius

- Breaking change for anyone running the master build pre-merge. Agent & hub
  must be upgraded together. Call this out in commit message and release
  notes.
- No data migration hazard — only a new table.
