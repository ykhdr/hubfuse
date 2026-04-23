# Join-Token Authentication Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Authenticate `Hub.Join` with short-lived, single-use hub-issued tokens so that only an operator holding console access to the hub can admit a new device. Closes issue #32.

**Architecture:** Mirror the existing pairing invite flow (`internal/hub/pairing.go`). Add a `pending_join_tokens` SQLite table, `Registry.IssueJoinToken` / gated `Registry.Join`, a `hubfuse-hub issue-join` CLI that writes directly to the store, and a required `--token` flag on `hubfuse join`.

**Tech Stack:** Go 1.25, gRPC, SQLite (`modernc.org/sqlite`), Cobra, KDL config, testify.

**Spec:** `docs/superpowers/specs/2026-04-22-issue32-join-token-auth-design.md`

> **Implementation note (post-review):** The final Store API landed simpler
> than this plan prescribed. `IncrementJoinTokenAttempts` and `DeleteJoinToken`
> were replaced by a single atomic `ClaimJoinToken(ctx, token, now) (bool, error)`
> — one conditional `DELETE` with `RowsAffected == 1` is now the only access
> gate. The 5-attempt retry cap was dropped; the token is single-use by first
> claim. `DeleteExpiredJoinTokens` returns `error` (no count). The tasks below
> describe the original path for historical context; the spec has the final
> design.

---

## File Structure

Create:
- `internal/hub/join_token.go` — `Registry.IssueJoinToken`, token validation helper
- `cmd/hubfuse-hub/issue_join.go` — CLI subcommand
- `tests/integration/join_token_test.go` — integration coverage

Modify:
- `proto/hubfuse.proto` — add `join_token` field to `JoinRequest`
- `proto/hubfuse.pb.go` — regenerated (do not hand-edit; run `make proto-gen`)
- `internal/common/errors.go` — new sentinels
- `internal/hub/store/models.go` — `JoinToken` struct
- `internal/hub/store/store.go` — new interface methods
- `internal/hub/store/sqlite.go` — schema + method implementations
- `internal/hub/store/sqlite_test.go` — store unit tests
- `internal/hub/registry.go` — gate `Join` on token; plumb store helpers
- `internal/hub/server.go` — pass `req.JoinToken` through to registry
- `internal/hub/hub.go` — start expired-token sweeper
- `internal/hub/configfile.go` — `join-token-ttl` KDL node
- `cmd/hubfuse-hub/main.go` — register `issue-join` subcommand
- `cmd/hubfuse-hub/config_resolve.go` — resolve TTL
- `cmd/hubfuse/main.go` — add `--token` flag to `join` subcommand
- `internal/agent/client.go` — `Join` takes `joinToken string`
- `tests/integration/join_test.go` — update existing tests to pass tokens
- `tests/scenarios/join_pair_test.go` (+ helpers) — update for token flow
- `README.md` — document the flow

---

## Task 1: Proto change + error sentinels

**Files:**
- Modify: `proto/hubfuse.proto`
- Regenerate: `proto/hubfuse.pb.go`
- Modify: `internal/common/errors.go`

- [ ] **Step 1: Edit `proto/hubfuse.proto` JoinRequest**

Replace the `JoinRequest` block:

```proto
message JoinRequest {
  string device_id = 1;
  string nickname = 2;
  string join_token = 3;
}
```

- [ ] **Step 2: Regenerate proto**

Run: `make proto-gen`
Expected: `proto/hubfuse.pb.go` regenerates with `JoinToken` field.

- [ ] **Step 3: Add sentinel errors**

Append to `internal/common/errors.go` (keep same `errors.New` style used there):

```go
var ErrInvalidJoinToken = errors.New("invalid join token")
var ErrJoinTokenExpired = errors.New("join token expired")
```

- [ ] **Step 4: Build**

Run: `make vet && make build`
Expected: passes.

- [ ] **Step 5: Commit**

```bash
git add proto/hubfuse.proto proto/hubfuse.pb.go internal/common/errors.go
git commit -m "proto: add join_token field and join-token error sentinels (#32)"
```

---

## Task 2: Store layer — model, schema, CRUD

**Files:**
- Modify: `internal/hub/store/models.go`
- Modify: `internal/hub/store/store.go`
- Modify: `internal/hub/store/sqlite.go`
- Test: `internal/hub/store/sqlite_test.go`

- [ ] **Step 1: Add model (`models.go`)**

```go
// JoinToken is a single-use token that authorises a device to call Join.
type JoinToken struct {
    Token     string
    ExpiresAt time.Time
    Attempts  int
    CreatedAt time.Time
}
```

- [ ] **Step 2: Extend Store interface (`store.go`)**

Add to the interface, grouped near invite methods:

```go
CreateJoinToken(ctx context.Context, t *JoinToken) error
GetJoinToken(ctx context.Context, token string) (*JoinToken, error)
IncrementJoinTokenAttempts(ctx context.Context, token string) error
DeleteJoinToken(ctx context.Context, token string) error
DeleteExpiredJoinTokens(ctx context.Context) (int, error)
```

- [ ] **Step 3: Extend SQLite schema (`sqlite.go`)**

In the embedded schema string (after `pending_invites`):

```sql
CREATE TABLE IF NOT EXISTS pending_join_tokens (
    token TEXT PRIMARY KEY,
    expires_at TIMESTAMP NOT NULL,
    attempts INTEGER NOT NULL DEFAULT 0,
    created_at TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_join_tokens_expires_at
    ON pending_join_tokens(expires_at);
```

- [ ] **Step 4: Implement the five store methods in `sqlite.go`**

Follow the style of the existing `CreateInvite` / `GetInvite` / `IncrementInviteAttempts` / `DeleteInvite` / `DeleteExpiredInvites`. `DeleteExpiredJoinTokens` must return `(int, error)` with the number of deleted rows (use `res.RowsAffected()`).

- [ ] **Step 5: Write unit tests (`sqlite_test.go`)**

Add TestJoinTokenCRUD and TestDeleteExpiredJoinTokens. Create a token with `ExpiresAt: time.Now().Add(time.Minute)`, round-trip via Get, increment attempts, delete. Expiry test: insert one expired, one live; call `DeleteExpiredJoinTokens`, assert count == 1 and only the live one remains.

- [ ] **Step 6: Run tests**

Run: `go test ./internal/hub/store/... -run TestJoinToken -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/hub/store/
git commit -m "store: add pending_join_tokens table and CRUD (#32)"
```

---

## Task 3: Registry — IssueJoinToken and gate Join

**Files:**
- Create: `internal/hub/join_token.go`
- Modify: `internal/hub/registry.go`
- Modify: `internal/hub/server.go`
- Test: `internal/hub/join_token_test.go`

- [ ] **Step 1: Create `internal/hub/join_token.go`**

```go
package hub

import (
    "context"
    "time"

    "github.com/ykhdr/hubfuse/internal/common"
    "github.com/ykhdr/hubfuse/internal/hub/store"
)

const (
    defaultJoinTokenTTL   = 10 * time.Minute
    maxJoinTokenAttempts  = 5
)

// IssueJoinToken creates a single-use token authorising the next Join call.
func (r *Registry) IssueJoinToken(ctx context.Context) (string, time.Time, error) {
    code := GenerateInviteCode()
    now := time.Now()
    expiresAt := now.Add(r.joinTokenTTL)
    t := &store.JoinToken{
        Token:     code,
        ExpiresAt: expiresAt,
        CreatedAt: now,
    }
    if err := r.store.CreateJoinToken(ctx, t); err != nil {
        return "", time.Time{}, err
    }
    return code, expiresAt, nil
}

// consumeJoinToken validates and atomically consumes a token. It returns nil
// on success; on any validation failure it returns a sentinel error and leaves
// the token in place (with attempts incremented where appropriate).
func (r *Registry) consumeJoinToken(ctx context.Context, token string) error {
    if token == "" {
        return common.ErrInvalidJoinToken
    }
    t, err := r.store.GetJoinToken(ctx, token)
    if err != nil {
        return common.ErrInvalidJoinToken
    }
    if time.Now().After(t.ExpiresAt) {
        return common.ErrJoinTokenExpired
    }
    if t.Attempts >= maxJoinTokenAttempts {
        return common.ErrMaxAttemptsExceeded
    }
    if err := r.store.IncrementJoinTokenAttempts(ctx, token); err != nil {
        return err
    }
    return nil
}
```

- [ ] **Step 2: Add `joinTokenTTL` to Registry**

In `internal/hub/registry.go`, add field `joinTokenTTL time.Duration` to the struct, and accept it in `NewRegistry` as a new final parameter `joinTokenTTL time.Duration`. Update callers in `hub.go` to pass a value (resolved in Task 4; for now, default to `defaultJoinTokenTTL`).

- [ ] **Step 3: Gate `Registry.Join`**

Change the signature to:

```go
func (r *Registry) Join(ctx context.Context, deviceID, nickname, ip, joinToken string) (certPEM, keyPEM, caCertPEM []byte, err error)
```

First call `r.consumeJoinToken(ctx, joinToken)`. Return immediately on error.

After successful cert issuance and device creation, call `r.store.DeleteJoinToken(ctx, joinToken)` (log & ignore error — cert is already issued; expiry sweeper will clean up).

- [ ] **Step 4: Update `server.go` Join**

Pass `req.JoinToken` through to `r.registry.Join`. No other changes.

- [ ] **Step 5: Unit tests (`internal/hub/join_token_test.go`)**

Write tests using an in-memory SQLite store (use existing test helper if present; otherwise `store.NewSQLite(":memory:")`):

- `TestIssueJoinToken_Unique`: issue two tokens, both match `HUB-[A-Z0-9]{3}-[A-Z0-9]{3}`, both present in store.
- `TestJoin_ValidToken_Consumed`: issue token, call `Join` with it, second `Join` with the same token returns `ErrInvalidJoinToken`.
- `TestJoin_EmptyToken`: `Join(..., "")` returns `ErrInvalidJoinToken`.
- `TestJoin_ExpiredToken`: insert a token with `ExpiresAt` in the past; call `Join`; assert `ErrJoinTokenExpired`.
- `TestJoin_AttemptCap`: 5 failures followed by a 6th call return `ErrMaxAttemptsExceeded` on the 6th (test using a token whose nickname collides → bumps attempts but leaves token).

- [ ] **Step 6: Run**

Run: `go test ./internal/hub/... -run 'TestIssueJoinToken|TestJoin_' -v`
Expected: PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/hub/
git commit -m "hub: gate Join on single-use issued tokens (#32)"
```

---

## Task 4: Config + hub wiring + expiry sweeper

**Files:**
- Modify: `internal/hub/configfile.go`
- Modify: `cmd/hubfuse-hub/config_resolve.go`
- Modify: `internal/hub/hub.go`

- [ ] **Step 1: Config file parsing**

In `internal/hub/configfile.go`, extend `HubConfigFile`:

```go
type HubConfigFile struct {
    DeviceRetention *time.Duration
    JoinTokenTTL    *time.Duration
}
```

In the KDL parse loop, mirror the `device-retention` case for node name `"join-token-ttl"`.

- [ ] **Step 2: Resolve TTL in `cmd/hubfuse-hub/config_resolve.go`**

Where `DeviceRetention` is resolved, add the same CLI-overrides-file-overrides-default ordering for `JoinTokenTTL`. Default: `10 * time.Minute`. No new CLI flag needed — file-only for now.

- [ ] **Step 3: Pass TTL into registry**

In `internal/hub/hub.go`, the resolved config already flows into `NewHub`. Pass `cfg.JoinTokenTTL` to `NewRegistry`.

- [ ] **Step 4: Start expiry sweeper**

Add a goroutine in `Hub.Start` (after heartbeat goroutine) that ticks every 60s and calls `h.store.DeleteExpiredJoinTokens(ctx)`. Log count & errors via `h.logger`. Structure:

```go
go func() {
    t := time.NewTicker(time.Minute)
    defer t.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-t.C:
            n, err := h.store.DeleteExpiredJoinTokens(ctx)
            if err != nil {
                h.logger.Warn("sweep expired join tokens", slog.Any("error", err))
                continue
            }
            if n > 0 {
                h.logger.Debug("swept expired join tokens", slog.Int("count", n))
            }
        }
    }
}()
```

- [ ] **Step 5: Build + vet**

Run: `make vet && make build`
Expected: passes.

- [ ] **Step 6: Commit**

```bash
git add internal/hub/ cmd/hubfuse-hub/
git commit -m "hub: wire join-token-ttl config and expiry sweeper (#32)"
```

---

## Task 5: `hubfuse-hub issue-join` CLI

**Files:**
- Create: `cmd/hubfuse-hub/issue_join.go`
- Modify: `cmd/hubfuse-hub/main.go`

- [ ] **Step 1: Create subcommand**

`cmd/hubfuse-hub/issue_join.go`:

```go
package main

import (
    "context"
    "fmt"
    "time"

    "github.com/spf13/cobra"
    "github.com/ykhdr/hubfuse/internal/hub"
    "github.com/ykhdr/hubfuse/internal/hub/store"
)

func issueJoinCmd() *cobra.Command {
    var dataDir string
    var ttl time.Duration
    cmd := &cobra.Command{
        Use:   "issue-join",
        Short: "Issue a single-use token that authorises one Join call.",
        RunE: func(cmd *cobra.Command, _ []string) error {
            ctx := context.Background()
            resolvedDir, err := resolveDataDir(dataDir)
            if err != nil {
                return err
            }
            s, err := store.OpenSQLite(ctx, sqlitePath(resolvedDir))
            if err != nil {
                return fmt.Errorf("open store: %w", err)
            }
            defer s.Close()

            now := time.Now()
            t := &store.JoinToken{
                Token:     hub.GenerateInviteCode(),
                ExpiresAt: now.Add(ttl),
                CreatedAt: now,
            }
            if err := s.CreateJoinToken(ctx, t); err != nil {
                return fmt.Errorf("create token: %w", err)
            }
            fmt.Fprintln(cmd.OutOrStdout(), t.Token)
            fmt.Fprintf(cmd.ErrOrStderr(),
                "Share this token with the joining device. Expires at %s.\n",
                t.ExpiresAt.UTC().Format(time.RFC3339))
            return nil
        },
    }
    cmd.Flags().StringVar(&dataDir, "data-dir", "", "hub data directory (defaults to ~/.hubfuse-hub)")
    cmd.Flags().DurationVar(&ttl, "ttl", 10*time.Minute, "how long the token is valid")
    return cmd
}
```

Notes:
- Use whatever helper already exists for resolving `--data-dir` (look near `startCmd`). If one does not exist, inline the same defaulting logic used by `startCmd`. `sqlitePath(dir)` should match what `hub.NewHub` uses — reuse the helper or extract it to a small package-level function before this task.
- `store.OpenSQLite` / equivalent constructor: check `internal/hub/store/sqlite.go` and reuse the exact exported constructor already used by the hub.

- [ ] **Step 2: Register the command**

In `cmd/hubfuse-hub/main.go` `rootCmd()`, add `cmd.AddCommand(issueJoinCmd())`.

- [ ] **Step 3: Manual smoke test**

```bash
make build
./hubfuse-hub start --data-dir /tmp/hf-test --listen :9095 &
HUBPID=$!
sleep 1
./hubfuse-hub issue-join --data-dir /tmp/hf-test
kill $HUBPID
```

Expected: stdout prints `HUB-XXX-YYY`, stderr prints the expiry notice.

- [ ] **Step 4: Commit**

```bash
git add cmd/hubfuse-hub/
git commit -m "hub: add issue-join CLI subcommand (#32)"
```

---

## Task 6: Agent side — `--token` flag, client change

**Files:**
- Modify: `internal/agent/client.go`
- Modify: `cmd/hubfuse/main.go`

- [ ] **Step 1: Extend `HubClient.Join` signature**

```go
func (c *HubClient) Join(ctx context.Context, deviceID, nickname, joinToken string) (*pb.JoinResponse, error) {
    return c.client.Join(ctx, &pb.JoinRequest{
        DeviceId:  deviceID,
        Nickname:  nickname,
        JoinToken: joinToken,
    })
}
```

- [ ] **Step 2: Add `--token` flag to `joinCmd`**

In `cmd/hubfuse/main.go` `joinCmd()`:

```go
var joinToken string
// ...
cmd.Flags().StringVar(&joinToken, "token", "", "join token issued by the hub (required)")
_ = cmd.MarkFlagRequired("token")
```

Thread `joinToken` into the `hubClient.Join(ctx, deviceID, nickname, joinToken)` call. Leave the interactive nickname prompt in place.

- [ ] **Step 3: Build + vet**

Run: `make vet && make build`

- [ ] **Step 4: Commit**

```bash
git add internal/agent/ cmd/hubfuse/
git commit -m "agent: require --token on hubfuse join (#32)"
```

---

## Task 7: Integration tests

**Files:**
- Modify: `tests/integration/join_test.go`
- Create: `tests/integration/join_token_test.go`

- [ ] **Step 1: Update existing join tests**

Anywhere `registry.Join(...)` or `client.Join(...)` is called in `tests/integration/join_test.go`, issue a token first:

```go
token, _, err := h.Registry().IssueJoinToken(ctx)
require.NoError(t, err)
resp, err := client.Join(ctx, "dev-1", "alice", token)
```

Add a helper in the same file if this pattern repeats. If `Hub` does not already expose `Registry()`, add a read-only accessor in `internal/hub/hub.go`.

- [ ] **Step 2: New `join_token_test.go`**

Cases:

- `TestJoin_RequiresToken`: call `client.Join(ctx, "dev", "nick", "")` → `resp.Success == false` and `resp.Error` contains "invalid join token".
- `TestJoin_TokenConsumedOnSuccess`: issue token, Join succeeds, second Join with same token fails.
- `TestJoin_ExpiredToken`: insert a token directly via store with `ExpiresAt` in the past, Join returns failure with "expired".
- `TestIssueJoinToken_SweptAfterExpiry`: issue token with very short TTL, wait past expiry, drive the sweep (either via test hook or by calling `store.DeleteExpiredJoinTokens` directly), confirm gone.

Reuse the in-process hub helper from the other integration tests.

- [ ] **Step 3: Run**

Run: `make test-integration`
Expected: PASS (new + existing).

- [ ] **Step 4: Commit**

```bash
git add tests/integration/ internal/hub/hub.go
git commit -m "test(integration): cover join-token gate and reuse (#32)"
```

---

## Task 8: Scenario test

**Files:**
- Modify: `tests/scenarios/helpers.go` (or wherever `StartAgent`/`Join` helpers live)
- Create or modify: `tests/scenarios/join_pair_test.go` and/or a new testdata/txtar file

- [ ] **Step 1: Update test helpers**

If `helpers.go` (or equivalent) wraps `hubfuse join`, add a `--token` pass-through. Helper should accept a token arg; issue via `hubfuse-hub issue-join` inside the helper unless the caller provides one.

Invocation:

```go
out, err := runHubCLI(t, hubDir, "issue-join", "--data-dir", hubDir)
require.NoError(t, err)
token := strings.TrimSpace(out)
// ...
agent.JoinWithToken(t, token)
```

- [ ] **Step 2: Positive scenario**

Existing `TestJoinPair` (or equivalent happy-path scenario) should be updated so it obtains a token via `issue-join` before calling `hubfuse join`, and continues to pass.

- [ ] **Step 3: Negative scenario**

Add `TestJoin_RejectsMissingToken`: call `hubfuse join <hub>` with no `--token` → non-zero exit, stderr mentions "required flag".

- [ ] **Step 4: Run**

Run: `go test ./tests/scenarios/... -timeout 120s`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add tests/scenarios/
git commit -m "test(scenarios): cover issue-join + agent --token flow (#32)"
```

---

## Task 9: README + changelog

**Files:**
- Modify: `README.md`

- [ ] **Step 1: Update Join docs**

Add / replace the device onboarding subsection:

````markdown
### Joining a device

Join is gated by a single-use token. On the hub host:

```bash
hubfuse-hub issue-join
# -> HUB-AB2-9XY
```

On the new device:

```bash
hubfuse join hub.local:9090 --token HUB-AB2-9XY
```

Tokens expire after 10 minutes and are invalidated on first successful use.
````

Remove any stale "unauthenticated" wording.

- [ ] **Step 2: Commit**

```bash
git add README.md
git commit -m "docs(readme): document hubfuse-hub issue-join flow (#32)"
```

---

## Task 10: End-to-end verification + PR

- [ ] **Step 1: Full check**

Run: `make vet && make test && make test-integration`
Expected: all green.

- [ ] **Step 2: Manual smoke**

Same script as Task 5 Step 3, plus:

```bash
./hubfuse join 127.0.0.1:9095 --token <TOKEN>
# succeeds; retry with same token -> fails with "invalid join token"
./hubfuse join 127.0.0.1:9095 --token WRONG-CODE
# fails
```

- [ ] **Step 3: Push + open PR**

```bash
git push -u origin fix/join-token-auth
gh pr create --title "fix(security): authenticate Join via hub-issued tokens (#32)" --body ...
```

PR body mentions breaking change for agent ↔ hub version skew; link the spec + plan.
