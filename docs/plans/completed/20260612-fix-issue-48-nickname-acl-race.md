# Fix #48 — nickname ACL race denies an authorized peer

## Problem statement

A share whose `allowed-devices` lists a **nickname** can reject an otherwise-
authorized peer with `Permission denied` at mount time, when the sharer's agent
has not yet resolved that peer's `device_id → nickname` mapping. This happens
right after the sharer's agent restarts (or right after pairing) — before the
hub's `DeviceOnline` event for the peer has been processed.

Observed: with `allowed-devices "server"`, after restarting the sharer's agent,
the peer's mount failed during initial directory access:
`sshfs: hubfuse@<ip>:macshare: Permission denied`. Switching to
`allowed-devices "all"` worked reliably (it bypasses nickname resolution).

The mount-by-nickname configuration *for outbound mounts* is unaffected; this is
purely an **inbound SFTP ACL** problem on the sharing side.

## Root-cause confirmation (file:line)

The ACL match depends on a resolver that is populated **only from live online
state**, so it is empty for a peer that is paired but not yet seen online.

- `internal/agent/sharesacl.go:35-54` — `ShareACL.Decide(deviceID, resolver)`:
  - `AllowAll` short-circuits to allow (line 36-38) — why `"all"` always works.
  - Otherwise it resolves a nickname (line 42-47); if `resolver.NicknameForDeviceID`
    returns `ok=false`, `nickname` stays `""`.
  - The match loop (line 48-52) allows when `tok == deviceID` **or**
    (`nickname != "" && tok == nickname`). When the nickname is unresolved, a
    nickname token can never match, so an explicitly-listed peer is **denied**.
- `internal/agent/daemon.go:216-223` — `(*Daemon).NicknameForDeviceID` is the
  only `DeviceResolver` implementation. It reads `d.onlineDevices[id]` and returns
  `ok=false` whenever the peer is not currently in that map (right after restart,
  before `processInitialDevices`/`handleDeviceOnline` repopulates it).
- `internal/agent/daemon.go:382-407` (`processInitialDevices`) and
  `internal/agent/events.go:35-73` (`handleDeviceOnline`) are the **only** writers
  of `onlineDevices`. Both require the peer to be currently online and reported by
  the hub. There is a window after the sharer restarts where the SSH server is up
  and accepting connections (`daemon.go:232` `startSSH` runs *before*
  `registerAndSubscribe` at `daemon.go:241`) but `onlineDevices` is still empty.
- Deny surfaces per-operation in the SFTP hot path:
  - `internal/agent/sftphandler.go:41` — `findShare` calls `Decide` on every lookup.
  - `internal/agent/sftphandler.go:73,97,267` — `resolveReadReal`/`resolveWriteReal`/
    `Filelist` turn `!dec.Allow` into `sftp.ErrSSHFxPermissionDenied` (`denied()`,
    line 61). `Decide` is evaluated **per request**, not once per session, because
    `newACLHandlers` (sshserver.go:268) re-reads a fresh ACL snapshot each op.
  - The peer hits this on its very first directory op (List/Stat of the share root
    or share dir), which is why the whole mount fails.

**The authenticated identity is always known.** The peer's `device_id` is
established by the pinned-public-key SSH auth callback
(`internal/agent/sshserver.go:71-81`) and propagated to the handler via
`Permissions.Extensions["hubfuse-device-id"]` (`sshserver.go:194`,
`sftphandler.go:18`). So at `Decide` time we always know *which paired device*
is connecting — only the nickname label is missing.

**There is currently no offline source of device_id → nickname.** Confirmed:
- `internal/agent/keys.go:96-130` — `known_devices/<device_id>.pub` stores only
  the public key. No nickname.
- `proto/hubfuse.proto:170-173` — `PairingCompletedEvent` carries
  `peer_device_id` + `peer_public_key`, **no nickname**. So
  `events.go:169-206 handlePairingCompleted` cannot learn the nickname today.
- BUT `proto/hubfuse.proto:196-202` — `ConfirmPairingResponse.peer_nickname` IS
  returned to the confirming side (`client.go:193-204`, surfaced in
  `cmd/hubfuse/main.go:524-541` but discarded after printing).
- `proto/hubfuse.proto:206-210` — `ListDevices` returns **all** registered
  devices (`DeviceInfo` with `nickname`, online or not) — a hub source that does
  not depend on the peer being online right now.

## Plan-review fixes to incorporate (MUST before impl)

1. **Concurrency — the daemon path MUST NOT use `SetNickname`'s load-modify-save.**
   Two events (`handleDeviceOnline` + `processInitialDevices`, or two onlines)
   racing independent load+save would lose writes / interleave. Correct design:
   - `d.nicknames` (in-memory map) is the single authoritative copy, mutated only
     under `d.mu`.
   - `rememberNickname(deviceID, nickname)`: take `d.mu.Lock()`, skip if
     `nickname == ""` or unchanged, else update `d.nicknames[deviceID]`, then take
     a **snapshot copy** of the map, `Unlock()`, and ONLY THEN call
     `SaveNicknames(dir, snapshot)` (atomic temp+rename) — disk I/O happens OUTSIDE
     the lock, and the persisted file is always written as a whole map (no
     concurrent load-modify-save).
   - Keep the standalone `SetNickname` (load-modify-save) ONLY for the CLI
     `pairConfirmCmd` path, which is a one-shot command with no in-daemon map and
     no concurrency. The daemon never calls `SetNickname`.
2. **Make the load-before-serve ordering an explicit invariant, not prose.** Load
   the disk cache into `d.nicknames` in `NewDaemon` (which is where `sshServer` is
   built and `SetDeviceResolver(d)` is wired, before `startSSH`). Add an assertion
   /comment that `d.nicknames` is populated before `startSSH` can serve the first
   SFTP op. Do NOT load it lazily in `Run` after `startSSH`.
3. **(nit) Keep `seedNicknamesFromHub` simple:** one `ListDevices` call, merge
   nicknames for paired devices into `d.nicknames` under lock, one atomic flush.
   It is the mechanism that populates nicknames for peers paired by the OTHER side
   (our `handlePairingCompleted` gets no nickname today) and self-heals renames —
   so keep it, but don't let it grow beyond merge+flush.

## Coordinator-confirmed decisions

1. **Proto change deferred to a follow-up.** This PR is **agent-only**: seed from
   the existing `ConfirmPairingResponse.peer_nickname` + `ListDevices`, no
   `make proto-gen`, no hub change. Adding `peer_nickname` to
   `PairingCompletedEvent` is a separate, optional issue. Confirmed.
2. **Persistence: JSON at `known_devices/nicknames.json`.** Confirmed — symmetric
   with the existing `known_devices/<id>.pub` files; not folded into a store (the
   agent persists to files, the SQLite store is hub-only).
3. **VERIFY before impl: the agent client wrapper exposes `ListDevices`.** The
   plan's `seedNicknamesFromHub` calls `ListDevices`. Confirm `internal/agent/client.go`
   already has a `ListDevices` method (or the gRPC stub is reachable). If NOT, add
   a thin client wrapper method for it — still agent-side, no proto change. The
   impl must confirm this rather than assume.
4. **Persistence is load-bearing for the ordering fix.** The win over an
   in-memory-only `ListDevices` seed is precisely that the map is warm from the
   PRIOR run BEFORE `startSSH` serves — an in-memory seed from `ListDevices` would
   land after `startSSH` (serve-before-warm) and after a hub round-trip. Keep the
   `NewDaemon` disk load strictly before `startSSH`.

## Chosen approach

**Persist a `device_id → nickname` map on disk and consult it as a fallback
inside the resolver, after the live `onlineDevices` lookup.** The map is a
durable record of *paired/known* peers, written whenever the agent learns a
nickname from an authoritative hub source, and loaded at startup before the SSH
server begins serving. The resolver becomes:

1. live `onlineDevices` (authoritative when the peer is online), then
2. the persisted paired-nickname cache (covers the restart / pre-online window).

This eliminates the race at its source: for any *already-paired* peer, the
nickname is knowable from persisted state, so `NicknameForDeviceID` no longer
returns `ok=false` during the online-gap window.

### Why this approach

- It removes the race rather than masking it. The nickname for a paired peer is
  genuinely knowable offline; the bug is that the agent threw that knowledge
  away. We persist it instead.
- No hot-path blocking. The SFTP `Decide` path stays synchronous and
  non-blocking — it reads an in-memory map (seeded from disk at startup), never a
  network round-trip.
- No new proto field strictly required for the primary fix (we can seed from
  `ListDevices` + `ConfirmPairingResponse`), keeping blast radius small. Adding
  `nickname` to `PairingCompletedEvent` is an optional follow-up (see Risks).

### Data flow for populating the cache

- **At startup** (`Run`, before `startSSH` begins serving — see Ordering below):
  load the persisted nickname map into the daemon; additionally call
  `ListDevices` after connect and merge hub-reported nicknames for any paired
  device into the cache, then persist. This covers "restarted after a prior
  pairing."
- **At pairing-confirm time** (confirmer side): `cmd/hubfuse/main.go:524-541`
  already receives `peerNickname`; persist it alongside the key so a confirmer
  that paired while its daemon was down still has the mapping on next start.
- **On `handlePairingCompleted`** (the side that *requested* pairing): the event
  lacks a nickname today. Either (a) backfill via the next `ListDevices`/
  `DeviceOnline`, or (b) add `peer_nickname` to `PairingCompletedEvent` (optional
  follow-up). The startup `ListDevices` merge (above) already closes this for the
  restart case; the live-pairing case is already handled because
  `handlePairingCompleted` only matters once the peer is online (and then
  `onlineDevices` has the nickname anyway).
- **On `handleDeviceOnline` / `processInitialDevices`**: opportunistically write
  the freshest `device_id → nickname` into the persisted cache so it survives the
  next restart.

### Persistence format and location

A single JSON file under the agent data dir, e.g.
`~/.hubfuse/known_devices/nicknames.json` (sibling of the `*.pub` files), mapping
`device_id → nickname`. JSON keeps it human-inspectable and trivially testable.
Writes go through a small loader/saver in a new `internal/agent/nicknames.go`
(mirrors `keys.go`'s file-helper style). `device_id` keys are validated via the
existing `validateDeviceID` (`keys.go:75-91`) before use as a map key / on read.

### Ordering fix (important)

Today `startSSH` (daemon.go:232) runs before `registerAndSubscribe`
(daemon.go:241), so the SSH server can accept a peer before any nicknames are
loaded. The persisted cache must be **loaded into the daemon before
`startSSH`** so the resolver is warm the instant the first SFTP op arrives. The
`ListDevices` merge can happen after connect but should also be best-effort
(non-fatal if the hub is briefly unreachable — the persisted cache already
covers the common case).

## Rejected alternatives

- **(b) "Just document listing a `device_id` in `allowed-devices`."** Already
  works (`Decide` matches raw `device_id`, confirmed by
  `sharesacl_test.go:34-38`). Good as a documented workaround and worth adding to
  docs, but it is **not** the fix: the issue is specifically about nickname
  tokens, which are the ergonomic, advertised way to write ACLs. Demoting users
  to opaque device_ids is a UX regression, not a fix.
- **(c) Soft-retry / re-resolve loop inside `Decide` during the unresolved
  window.** Rejected: it puts latency and a potential network call in the SFTP
  hot path (`Decide` runs per-op), is hard to bound correctly, and still fails if
  the peer simply never appears in `onlineDevices` quickly. The persisted-cache
  approach is strictly simpler and deterministic.
- **Block SSH serving until `onlineDevices` is populated.** Rejected: it would
  delay *all* inbound access (including raw device_id and `"all"` shares) on every
  restart and couples serving to event timing. The cache decouples them.
- **Fail-open during the unresolved window (allow if nickname unknown).**
  Rejected on security grounds — see below.

## Security argument (no fail-open)

The fix only ever **adds a new authoritative source** for an *already-paired*
peer's nickname; it never relaxes the deny decision. An unresolved nickname still
yields `nickname == ""`, and a non-matching token still denies (`sharesacl.go:48-53`
is unchanged in spirit). Specifically:

- The cache is keyed by `device_id`, which is the **mTLS/SSH-authenticated**
  identity — an attacker cannot inject a nickname for a device_id they cannot
  authenticate as (the SSH key pin at `sshserver.go:71-81` gates the connection
  first; an unpaired device never reaches `Decide`).
- The cache only contains nicknames the hub itself reported (`ListDevices`,
  `DeviceOnline`, `ConfirmPairing`) for devices this agent has paired with. It
  cannot manufacture membership in `allowed-devices` for a device the operator
  did not list — it only supplies the *label* the operator already used.
- We do **not** add an "allow if unresolved" branch. A device whose nickname is
  unknown and whose device_id is not listed remains denied. The change strictly
  shrinks false-denials of authorized peers; it does not widen the allow set for
  unauthorized ones.

One-line: an unresolved nickname still denies — we only persist the genuine,
hub-authoritative `device_id → nickname` of already-paired peers so an
authorized peer is not denied during the online-gap window.

## Files & functions to change (with signatures)

### New — `internal/agent/nicknames.go`
A tiny persisted-map helper, styled after `keys.go`.

```go
// NicknameStore is a persisted device_id -> nickname map, used as an offline
// fallback by the DeviceResolver so paired peers resolve before they appear
// online.
// File: <knownDevicesDir>/nicknames.json

func LoadNicknames(knownDevicesDir string) (map[string]string, error)
// reads nicknames.json; returns an empty map (nil error) if absent.

func SaveNicknames(knownDevicesDir string, m map[string]string) error
// atomic write (temp + rename), 0644, validates each device_id key.

func SetNickname(knownDevicesDir, deviceID, nickname string) error
// load-modify-save convenience; no-op if nickname == "".
```

### `internal/agent/daemon.go`
- Add a field to `Daemon` (around line 49):
  `nicknames map[string]string // device_id -> nickname, paired-peer fallback`
  guarded by the existing `d.mu`.
- `NewDaemon` (~line 60): load the persisted map into `d.nicknames` (best-effort;
  log + empty on error).
- `(*Daemon).NicknameForDeviceID(id string) (string, bool)` (line 216): keep the
  live `onlineDevices` lookup first; on miss, fall back to `d.nicknames[id]`
  (return `ok=true` only when the value is non-empty). Holds `d.mu.RLock()`.
- `processInitialDevices` (line 382) and `handleDeviceOnline` (events.go:35):
  after updating `onlineDevices`, also record/persist `device_id → nickname` into
  the cache (helper `d.rememberNickname(deviceID, nickname)`).
- `Run` (line 227): after `connect` and before serving is "warm", call a new
  best-effort `d.seedNicknamesFromHub(ctx)` that invokes `ListDevices` and merges
  nicknames for paired devices. Ensure the persisted map is loaded **before**
  `startSSH`. (Loading in `NewDaemon` satisfies this; the hub seed is additive.)
- New helper:
  `func (d *Daemon) rememberNickname(deviceID, nickname string)` — updates
  `d.nicknames` under lock and persists via `SetNickname` (errors logged, not
  fatal).

### `cmd/hubfuse/main.go`
- `pairConfirmCmd` (line 524-541): after `SavePeerPublicKey`, also persist the
  nickname: `agent.SetNickname(knownDevicesDir, peerDeviceID, peerNickname)`
  (guarded by `peerNickname != ""`). errcheck-clean (handle the returned error).

### Optional follow-up (NOT required for the fix)
- `proto/hubfuse.proto:170-173`: add `string peer_nickname = 3;` to
  `PairingCompletedEvent` and populate it on the hub so the *requesting* side can
  persist the nickname immediately on `handlePairingCompleted` without waiting for
  `ListDevices`/`DeviceOnline`. Requires `make proto-gen` and a hub-side change;
  scope it separately to keep this PR focused on the agent.

## Implementation checklist

1. [ ] Add `internal/agent/nicknames.go` with `LoadNicknames` / `SaveNicknames`
       / `SetNickname` (atomic write, validate device_id, empty-map on absent).
2. [ ] Add unit tests `internal/agent/nicknames_test.go` (round-trip, absent
       file → empty, bad device_id rejected, atomic overwrite).
3. [ ] Add `nicknames` field to `Daemon`; load it in `NewDaemon`.
4. [ ] Extend `NicknameForDeviceID` with the persisted-cache fallback.
5. [ ] Add `rememberNickname` and wire it into `processInitialDevices` and
       `handleDeviceOnline`.
6. [ ] Add `seedNicknamesFromHub` (best-effort `ListDevices` merge) and call it
       in `Run` after connect; confirm persisted load happens before `startSSH`.
7. [ ] Persist nickname in `pairConfirmCmd` (errcheck-clean).
8. [ ] Update `sharesacl_test.go` with the regression test (below).
9. [ ] Update docs (`README.md` ACL section): nicknames in `allowed-devices`
       resolve even before the peer is online; note device_id is also accepted.
10. [ ] `make vet && make test` green; golangci-lint errcheck clean.

## Test plan

### Regression unit test (the core of the fix) — `sharesacl_test.go`
The existing `stubResolver` (line 9-14) already returns `ok=false` for unknown
device_ids — perfect for simulating the unresolved window. Add a test that
encodes the *fixed* contract once the persisted fallback is wired through the
resolver. Because `Decide` takes a `DeviceResolver`, the regression is best
expressed as: **given a resolver that DOES know the paired peer's nickname (the
fixed state), an allowed-by-nickname peer is allowed; an unlisted peer is still
denied even though the resolver has an entry for it.**

```go
func TestShareACL_Decide_NicknameResolvedFromPairedState(t *testing.T) {
    acl := ShareACL{Alias: "macshare", Path: "/tmp/m", AllowedDevices: []string{"server"}}
    // Fixed state: resolver knows the paired peer's nickname even though the
    // peer was not in onlineDevices (here modelled by the persisted-fallback
    // stub returning ok=true).
    r := stubResolver{"dev-server": "server"}
    d := acl.Decide("dev-server", r)
    assert.True(t, d.Allow, "authorized peer must be allowed once nickname resolves from paired state")
}

func TestShareACL_Decide_UnresolvedNicknameStillDeniesUnlisted(t *testing.T) {
    acl := ShareACL{Alias: "macshare", Path: "/tmp/m", AllowedDevices: []string{"server"}}
    // Unlisted peer; resolver reports ok=false (unresolved window). Must DENY.
    d := acl.Decide("dev-attacker", stubResolver{})
    assert.False(t, d.Allow, "unlisted peer with unresolved nickname must stay denied (no fail-open)")
}

func TestShareACL_Decide_UnresolvedNicknameDeniesEvenIfResolverKnowsOther(t *testing.T) {
    acl := ShareACL{Alias: "macshare", Path: "/tmp/m", AllowedDevices: []string{"server"}}
    // Resolver resolves the connecting device to a DIFFERENT nickname → deny.
    d := acl.Decide("dev-x", stubResolver{"dev-x": "laptop"})
    assert.False(t, d.Allow)
}
```

These assert both halves: authorized-peer-now-allowed AND
unlisted-peer-still-denied (the security guarantee).

### Resolver-level test (proves the race is actually closed) — `nicknames_test.go` / a daemon-level test
Drive `NicknameForDeviceID` directly to prove the offline path resolves:

- Construct a `Daemon` (or a minimal struct exposing the same method) with an
  **empty** `onlineDevices` and a persisted nicknames map containing
  `dev-server → "server"`. Assert `NicknameForDeviceID("dev-server")` returns
  `("server", true)` — i.e. resolves *without* the peer being online.
- Assert `NicknameForDeviceID("unknown")` returns `("", false)`.

Note: keep this in `internal/agent` (per `feedback_no_cmd_main_test`: no
`cmd/.../main_test.go`). If the full `Daemon` is awkward to construct, factor the
two-tier lookup into a small helper that both the `Daemon` method and the test
call.

### nicknames.go round-trip tests
Round-trip, absent-file→empty-map, invalid device_id rejected, overwrite is
atomic and durable. Use `t.TempDir()` and isolate `HOME` per
`feedback_test_setup_gotchas` if any helper expands `~`.

### Manual / integration sanity (optional)
Reproduce the original report against an in-process hub: register two agents,
pair them, restart the sharer's daemon, and confirm an immediate List of the
share by the peer succeeds (no `EACCES`) before any `DeviceOnline` is processed,
with `allowed-devices` set to the peer's **nickname**.

## Risks & edge cases

- **Stale nickname after a peer renames** (`Rename` RPC, proto:74-83). The
  persisted cache could hold an old nickname. Mitigation: `handleDeviceOnline`
  and the startup `ListDevices` merge overwrite with the hub's current value, so
  the cache self-heals on the next online event. A renamed peer that is offline
  during the window resolves to its *previous* nickname — acceptable, and no worse
  than today (today it resolves to nothing). If the operator changed
  `allowed-devices` to the new nickname, the peer would be denied until refresh —
  document that nickname changes propagate on next online/ListDevices.
- **errcheck strict**: every `SaveNicknames`/`SetNickname`/`ListDevices` call must
  handle its error (log-and-continue for best-effort paths). No naked `_ =` unless
  justified.
- **Concurrency**: `d.nicknames` shares `d.mu` with `onlineDevices`; the resolver
  takes `RLock`, writers take `Lock`. Disk writes should happen *outside* the lock
  (snapshot under lock, write after) to avoid holding the mutex across I/O.
- **`"all"` and raw `device_id` tokens unchanged**: `Decide`'s `AllowAll`
  short-circuit (line 36) and `tok == deviceID` branch (line 49) are untouched —
  backward compatibility preserved; existing tests
  (`sharesacl_test.go:16-51`) must stay green.
- **Atomic write**: use temp-file + `os.Rename` in the same dir to avoid a
  truncated `nicknames.json` on crash.
- **Empty/absent file** must yield an empty map with nil error (first run, never
  paired) — do not treat as fatal.
- **Hub unreachable at startup**: `seedNicknamesFromHub` is best-effort; the
  persisted cache (loaded in `NewDaemon`) already covers the reported scenario.

## Conventions

Go 1.25; testify (`assert`); golangci-lint errcheck **strict**; no
`cmd/*/main_test.go` (test in `internal/agent` or `tests/integration`); follow
`keys.go` file-helper style for `nicknames.go`; KDL config and Cobra CLI patterns
unchanged.
