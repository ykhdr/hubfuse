# Codebase Cleanup — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development.

**Goal:** Land all 36 findings from the 2026-04-16 /simplify audit in one PR.
De-duplicate helpers, plug one auth-bypass, straighten types, delete dead
code, drop narrative comments.

**Architecture:** Bundle related fixes into sequential tasks. Start with
additive extractions (new helpers) before touching call sites, so each
commit builds and tests stay green.

**Tech Stack:** Go 1.25. No new dependencies.

---

## Audit summary

The full enumerated list (HIGH/MED/LOW) is preserved in the chat that
produced this plan; items are referenced below by their audit number
(#1…#36) and not re-transcribed. Treat the audit as the "spec".

## Bundles (execute in order)

### Bundle A — Extract shared helpers to `internal/common/`
Items: **#1** (expandHome×4), **#12** (paths constants), **#20** (PEM
encode helpers), **#21** (log flags helper).

- Add `internal/common/paths.go` with `ExpandHome(path)` and named
  constants for every hubfuse-relative filename currently duplicated
  (`TLSDir="tls"`, `KeysDir="keys"`, `KnownDevicesDir="known_devices"`,
  `IdentityFile="device.json"`, `DBFile="hubfuse.db"`,
  `CAFile="ca.crt"`, `CAKeyFile="ca.key"`, `ServerCertFile="server.crt"`,
  `ServerKeyFile="server.key"`, `ClientCertFile="client.crt"`,
  `ClientKeyFile="client.key"`, `HubPIDFile="hubfuse-hub.pid"`,
  `AgentPIDFile="hubfuse.pid"`, `HubLogFile="hub.log"`,
  `AgentLogFile="agent.log"`, `HubDataDir="~/.hubfuse-hub"`,
  `AgentDataDir="~/.hubfuse"`).
- Extend `internal/common/tls.go` with `EncodeCACertPEM(cert) []byte`
  and `EncodeCAKeyPEM(key) []byte` (using the file-private
  `pemTypeCert` / `pemTypeKey` constants).
- Add `common.RegisterLogFlags(cmd *cobra.Command) *LoggerOptions` that
  wires `--log-file`, `--log-level`, `--verbose` onto a cobra command.

### Bundle B — Extract PID-file command helpers to `internal/common/daemonize/`
Items: **#2** (readPID dup), **#3** (stop/status twins).

- `ReadPID(path) (int, error)` (just `strconv.Atoi(strings.TrimSpace(...))`,
  with clear error).
- `SignalStop(pidPath, name string) error` — the body of both stopCmds.
  Returns error; caller prints success line. `name` is the process
  noun (`"agent"` / `"hub"`) for the message.
- `ReportStatus(pidPath, name string)` — the body of both statusCmds;
  prints to stdout, returns nil so cmd can propagate.

### Bundle C — Consolidate test-hub bootstrap
Items: **#4** (`startTestHub` ×3).

- New package `internal/hub/hubtest` with
  `StartTestHub(t *testing.T, opts ...Option) *Harness`.
- `Harness` exposes `Addr`, `CAPEM`, `ClientCreds(deviceID) credentials.TransportCredentials`,
  `Store`, `Registry`, `Cleanup` (registered via `t.Cleanup`).
- Unifies the hub startup code now duplicated between
  `tests/integration/integration_test.go`,
  `tests/integration/reconnect_test.go`, and
  `internal/hub/server_test.go`.

### Bundle D — Use the extractions
Items: delete the duplicates in `cmd/hubfuse/main.go`,
`cmd/hubfuse-hub/main.go`, `internal/hub/hub.go`,
`internal/agent/config/config.go` (keep `ExpandTilde` as a thin alias
if needed for the KDL layer). Rewire tests to `hubtest.StartTestHub`.
Use the new paths constants everywhere a hubfuse-relative filename
literal still appears.

### Bundle E — Hub security + correctness
Items: **#5** N+1 `GetShares`, **#6** `DeleteExpiredInvites` scheduling,
**#7** Subscribe auth bypass (use `common.ExtractDeviceID(ctx)` instead
of `req.DeviceId`; same for `ConfirmPairing`), **#15** double cert load,
**#16** heartbeat SQL per tick, **#35** blocking RSA gen in NewHub,
**#36** redundant GetDevice in MarkOffline.

Details:
- `store.GetSharesForDevices(ctx, ids []string) (map[string][]*Share, error)`
  batching the current per-device `GetShares`. Rewire `Register` and
  `ListDevices` handlers.
- Invite pruning: piggy-back on the heartbeat monitor's existing
  ticker. Once per minute (i.e. every 6 ticks) call
  `store.DeleteExpiredInvites(ctx)`; log count.
- Subscribe: drop `req.DeviceId` lookup; use the mTLS CN. Mark the
  proto field as `reserved 1` for a future breaking change note —
  **do not** regenerate proto to remove it in this PR (breaks old
  clients); just ignore it server-side and document the deprecation
  in the proto comment. Same for `ConfirmPairing`.
- `loadOrGenerateCerts` currently re-reads files that `NewHub` also
  opens later via `LoadTLSServerConfig`. Return the built
  `*tls.Config` from `loadOrGenerateCerts` and reuse it. Moves cert
  load fully into `NewHub` (no change from caller POV).
- Heartbeat in-memory: add `Registry.MarkHeartbeat(deviceID)` that
  updates an in-memory `lastSeen map[string]time.Time`; the stale
  detector reads from it. Still flush to `store.UpdateHeartbeat` but
  only when the monitor demotes status (i.e., in `MarkOffline` path).
- RSA downshift: CA key goes from 4096 → 3072 bits (comfortable
  margin; halves generation time). Applies only to fresh generation;
  existing 4096-bit keys keep working.
- `MarkOffline` takes the `*Device` already in hand instead of
  re-issuing `GetDevice`.

### Bundle F — Typed enums and renames
Items: **#8** stringly-typed, **#9** `DeviceInfo` rename, **#10**
`HubConfig` → `hub.Config`, **#19** `HubConfig.LogLevel` → `slog.Level`,
**#30** redundant `DeviceId` in RPCs.

- `internal/hub/store/models.go`:
  `type DeviceStatus string` with `StatusOnline`, `StatusOffline`;
  `type Permission string` with `PermRO`, `PermRW`. Replace literals
  at every call site (`registry.go`, `pairing.go`, `sqlite.go`,
  `server.go`, `cmd/hubfuse/main.go`).
- Rename `agent.DeviceInfo` → `agent.OnlineDevice`; field
  `knownDevices` → `onlineDevices`; function
  `protoToDeviceInfo` → `protoToOnlineDevice`.
- Rename `hub.HubConfig` → `hub.Config`. Cascade through callers.
- `hub.Config.LogLevel` becomes `slog.Level` parsed in cmd via
  `common.ParseLogLevel`.
- Keep proto fields (see Bundle E note).

### Bundle G — Encapsulation + small refactors
Items: **#11** `OnReady` in config, **#13** leaky Store, **#14** mount
struct key, **#17** `Daemon.Run` split, **#18** `isPaired`
canonicalization, **#22** keys file constants export, **#28**
`writeConfig` → `config.Save`.

- `Hub.OnReady` moves off the struct into `Config.OnReady` (set
  before `NewHub` returns, read once in `Start`). Same for
  `agent.Daemon`.
- Add `Registry.GetShares(ctx, deviceID)` and
  `Registry.ListDevicesWithShares(ctx)`; server uses those instead of
  reaching into `registry.store.*`.
- Mounter `activeMounts` keyed by `struct{Device, Share string}`;
  drop the `strings.SplitN(key, ":", 2)` helpers.
- `Daemon.Run` split into `connect(ctx)`, `startSSH(ctx)`,
  `registerAndSubscribe(ctx)`, `runServices(ctx)`. Drops the
  step-numbered comments.
- `isPaired` reads **only** by device_id. `handlePairingCompleted`
  writes **only** by device_id. The nickname-keyed file is removed
  (best-effort migrate: on boot, if both forms exist, rename
  nickname → id and remove the old one; if only nickname exists,
  rename). Simpler: stop writing nickname-keyed file; let the id
  form be canonical going forward. Existing nickname files become
  dead on next pairing event and can be manually cleaned.
- Export `PrivateKeyFile`/`PublicKeyFile` from `internal/agent/keys.go`
  (already there as unexported); drop the redeclarations in
  `cmd/hubfuse/main.go`.
- Move `writeConfig` (KDL emitter) from `cmd/hubfuse/main.go` into
  `internal/agent/config/config.go` as `Save(path, *Config)`.

### Bundle H — Polish
Items: **#23** strip narrative comments, **#24** delete empty
`internal/agent/agent.go`, **#25** unused `GenerateDeviceID`, **#26**
dead `ListPairedDevices`, **#27** `tlsConfigFromPEM` → testutil,
**#29** `BroadcastAll`, **#31** `heartbeatInterval` const, **#32**
`Registry.Subscribe` leak on reconnect, **#33** `publicKeyCallback`
cache, **#34** invite biased mod.

- Delete narrative/step-numbered comments flagged in the audit.
- Remove empty `internal/agent/agent.go`.
- Delete unused `GenerateDeviceID` (cmd inlines `uuid.New().String()`);
  or call the helper from cmd — pick one, prefer keeping the helper
  and using it.
- Delete `ListPairedDevices` if still unused after Bundle G.
- Move `tlsConfigFromPEM` from `internal/hub/hub.go` to a new
  `internal/hub/testutil_test.go` (or inline into the call sites).
- `Registry.BroadcastAll(event)` as a separate method; `Broadcast`
  keeps the exclude-string signature for targeted broadcasts.
- `heartbeatInterval = 10 * time.Second` hoisted to a named constant
  at package scope near other timing constants.
- Registry.Subscribe closes/drains the previous channel for that
  device_id before overwriting.
- SSH server caches `ssh.Marshal(key)` bytes keyed on the raw ssh
  key when `UpdateAllowedKeys` is called; lookup becomes a
  `map[string]bool`.
- Invite-code generator uses rejection sampling:
  `for b := range bytes { if b < 248 { ... } }` (248 = 36×6, largest
  multiple of 36 ≤ 255).

## Execution order

Bundles are declared in dependency order:

- A → must land before D.
- B → must land before D (D removes cmd duplicates in favour of B).
- C → independent of A/B but should land before D if tests get
  refactored too.
- D → rewires call sites.
- E through H → each landable independently, but after D.

Each bundle lands as its own commit(s). Run `go test ./... -count=1`
and `golangci-lint run ./...` after every bundle. Full suite must stay
green for every commit.

## Testing strategy

No new tests for mechanical extractions (A, B, C, D). Unit tests added
where new logic lands (E: batched `GetSharesForDevices`; E: heartbeat
in-memory behaviour; G: Registry.GetShares/ListDevicesWithShares).
Existing tests must continue to pass unchanged, except where they
move into `hubtest` or adopt the new helpers.

## Rollout

Single PR on `feature/codebase-cleanup`. Manual smoke test after E
(Subscribe auth fix — verify pairing still works end-to-end). PR
title: "Codebase cleanup: extract helpers, fix Subscribe auth, tidy
types/comments".
