# Add `share allow` / `share deny` CLI to manage a share's allowed devices (issue #63)

> **Status: completed** (branch `feature/issue-63`).
> Implemented pure `*Config` helpers `AllowDevices`/`DenyDevices`/`AllowsAll`
> (`internal/agent/config/shares.go`), thin cobra commands `shareAllowCmd`/
> `shareDenyCmd` + `splitDeviceArgs` (`main.go`), unit tests for both layers,
> and README docs. Auto plan-review (APPROVE WITH CHANGES → fixed comma-arg
> parity, deny-vs-`all` notice, case-sensitivity → re-review APPROVE) and an
> auto code-review (LGTM; both LOW findings — duplicate-stored-token removal and
> the all-share notice on a no-op deny — fixed) were applied. `make build`/
> `vet`/`test-unit` pass. Live-daemon hot-reload verification remains manual
> (see Post-Completion).

## Overview
- Add two subcommands under the existing `share` group so an existing share's `allowed-devices` list can be mutated in place, without hand-editing `config.kdl` or a `remove`+`add` round-trip that loses the path/permissions.
  - `hubfuse share allow <alias> <device>...` — add device nickname(s)/token(s) to `AllowedDevices`.
  - `hubfuse share deny  <alias> <device>...` — remove device nickname(s)/token(s) from `AllowedDevices`.
- Solves the gap noted in the issue: today the allowlist is settable only at `share add` time, and the duplicate-alias guard (`main.go:770`) blocks re-running `add` to update it.
- Integrates by reusing the load → mutate → `config.Save()` pattern from `shareAddCmd`/`shareRemoveCmd`. The running daemon picks up the change live via the existing fsnotify hot-reload watcher; no restart, no new runtime code.

## Context (from discovery)
- Files/components involved:
  - `cmd/hubfuse/main.go` — Cobra tree. `shareCmd` group wired at L49–53 (`shareCmd.AddCommand(shareAddCmd(), shareRemoveCmd(), shareListCmd())`); `shareAddCmd` L744–801; `shareRemoveCmd` L803–842; `shareListCmd` L844–875; `loadConfig` L1042–1053 (returns `DefaultConfig()` when the file is absent).
  - `internal/agent/config/config.go` — `ShareConfig{Path, Alias, Permissions, AllowedDevices []string}` (L44–50); `Config{... Shares []ShareConfig ...}` (L16–22); `Save` (L280–336) serialises an `allowed-devices` grandchild node only when the list is non-empty; `Load` (L72) + `parseSharesBlock` (L175) read it back.
  - `internal/agent/sharesacl.go` — `ShareACLsFromConfig` (L69) lifts the reserved `"all"` token into `AllowAll` and drops it from the token list; `Decide` (L35) matches a token against device_id or resolved nickname. **No change needed** — confirms `"all"` is just a normal stored token.
  - `internal/agent/config/watcher.go` — fsnotify hot-reload (per CLAUDE.md). **No change needed** — `config.Save` triggers it.
- Related patterns found: each subcommand is `func xCmd() *cobra.Command` returning a closure; share-mutating commands resolve `dataDir := common.ExpandHome(common.AgentDataDir)`, `cfgPath := filepath.Join(dataDir, common.ConfigFile)`, then `loadConfig` → mutate `cfg.Shares` → `config.Save(cfgPath, cfg)` → `fmt.Printf` a confirmation.
- Dependencies identified: Cobra; `internal/agent/config`; no new third-party deps.

## Development Approach
- **testing approach**: Regular (code first, then tests). The mutation logic is extracted into pure helpers in the `config` package so it is unit-testable without a hub, a daemon, or cobra plumbing (there is no `cmd/hubfuse/main_test.go` and the existing share closures are untested).
- Complete each task fully before the next; small focused changes.
- **Every task includes tests.** Helper logic gets table-driven unit tests plus a Save→Load round-trip; the cobra commands are thin glue gated by `make build`/`make vet`.
- **All tests must pass before starting the next task.**
- Backward compatible: purely additive (two new subcommands, two new helpers).

## Testing Strategy
- **unit tests** (`internal/agent/config/shares_test.go`, new): cover `AllowDevices` and `DenyDevices` for: alias not found → error; add new tokens; dedupe against existing list; dedupe within the input args; no-op when all already present (empty `added`); `deny` removes matching tokens; `deny` reports not-found names; `deny` emptying the list is allowed; `"all"` handled as an ordinary token by both (allow adds it, deny removes it); **case-sensitivity** (`deny "Dev1"` does not remove stored `"dev1"`, returns it as not-found); and a Save→Load round-trip asserting the on-disk `allowed-devices` node reflects the mutation (and the node disappears when the list becomes empty, per `Save` L308).
- **arg-normalisation tests**: `splitDeviceArgs` lives in `main.go`. Since `cmd/hubfuse` has no test file, either (a) add a `cmd/hubfuse/main_test.go` with a focused table test for `splitDeviceArgs` (comma split, trim, drop-empty, all-empty→nil), or (b) keep it but rely on the build gate. **Decision: (a)** — it's a pure function and the one piece of command-layer logic worth locking down.
- **e2e tests**: none — the project has no CLI e2e harness; the daemon hot-reload path is already covered by `config` watcher/diff tests and is not re-exercised here.

## Progress Tracking
- Mark completed items with `[x]` immediately when done.
- Add newly discovered tasks with ➕ prefix; blockers with ⚠️ prefix.
- Keep this plan in sync with actual work.

## Solution Overview
- **Approach (recommended from brainstorm): pure helpers in the `config` package + thin cobra glue.**
- Add to a new `internal/agent/config/shares.go`:
  - `func (c *Config) findShare(alias string) (*ShareConfig, error)` — returns a pointer into `c.Shares` (index, not range-copy) or an error `share %q not found`. Internal helper shared by both.
  - `func (c *Config) AllowDevices(alias string, devices []string) (added []string, err error)` — find share; for each requested token, skip if already in `AllowedDevices` or already added this call (dedupe both ways, preserving input order); append the rest; return the newly-added tokens (empty slice ⇒ no-op).
  - `func (c *Config) DenyDevices(alias string, devices []string) (removed, notFound []string, err error)` — find share; build the requested set; rebuild `AllowedDevices` keeping only tokens not requested for removal (record each kept-out as `removed`); any requested token never present goes to `notFound`. Allows the list to become empty.
- `"all"` is matched and stored as a plain string in the helpers; `ShareACLsFromConfig` lifts it to `AllowAll` later. **But** `deny` must not silently mislead on an allow-all share (see below).
- Cobra commands `shareAllowCmd()` / `shareDenyCmd()` in `main.go` mirror `shareRemoveCmd`: resolve paths → `loadConfig` → **normalise args (comma-split, see below)** → call the helper → `config.Save` → print result. `deny` prints a `warning:` line per not-found name (issue requirement) but still succeeds.
- Register both in the `shareCmd.AddCommand(...)` call at `main.go:53`.

### Review fixes folded in (from plan review)
- **Comma-separated args — consistency with `share add --allow`.** `share add` uses `cobra.StringSliceVar` (`main.go:797`), which splits `--allow a,b,c` into three tokens. Positional args do **not** auto-split, so `share allow x a,b,c` would otherwise store one bogus token `"a,b,c"` that never matches in `Decide`. **Decision: mirror StringSlice semantics** — in both commands, normalise the positional device args by splitting each on `,`, trimming surrounding whitespace, and dropping empties, *before* calling the helper. So `share allow x a,b c` and `share allow x a b c` are equivalent. Helpers stay pure (they receive an already-clean token slice). A normalisation that yields zero tokens (e.g. `share allow x ,`) is an error: `no devices specified`.
- **`deny` on a share that still allows `all`.** `Decide` short-circuits on `AllowAll` (`sharesacl.go:36`), so denying a single device on a share whose list contains `"all"` leaves that device effectively allowed. To avoid a false sense of revocation, `shareDenyCmd` emits, after a successful op, a notice when the share's `AllowedDevices` still contains `"all"` **and** the user denied something other than `"all"`: `note: share %q still allows ALL devices; run "hubfuse share deny %s all" to change that`. `deny x all` itself removes the token normally (no notice). Covered by a dedicated test.
- **Case sensitivity (intentional, documented).** Token matching in `Decide` (`L49`) is exact and case-sensitive; `AllowDevices`/`DenyDevices` therefore compare tokens byte-for-byte (no case folding), consistent with `share add` and `Decide`. A test asserts `deny x Dev1` does **not** remove a stored `dev1` (reported as not-found instead).

## Technical Details
- **Mutation via index, not range copy.** `c.Shares` is `[]ShareConfig` (value structs); `findShare` must return `&c.Shares[i]` so appends/rebuilds mutate the real element. A `for _, s := range` copy would silently drop the change.
- **Dedupe order.** `AllowDevices` preserves existing entries then appends new ones in input order; uses a `map[string]struct{}` seeded from the current list (and grown as it adds) to skip duplicates — including repeats within the same args slice.
- **`DenyDevices` not-found vs removed.** Build `want := set(devices)`. Walk the existing list: tokens in `want` are dropped and appended to `removed`; others are kept. Then any element of `devices` not seen in the original list → `notFound` (dedupe `notFound` so `deny x ghost ghost` warns once).
- **Empty-list persistence.** `Save` (L308) omits the `allowed-devices` node when the slice is empty — so `deny`-ing the last device cleanly removes the node, and an empty `[]string{}` vs `nil` distinction does not matter to `Save`. The round-trip test asserts this.
- **Command output** (mirrors existing `fmt.Printf` style):
  - allow, added ≥1: `allowed [a b] on share %q`
  - allow, none new: `share %q: no changes (already allowed)`
  - deny, removed ≥1: `denied [a] from share %q`
  - deny, removed 0: `share %q: no changes`
  - deny, each not-found: `warning: %q was not in the allow list for share %q`
- **Args:** both use `Args: cobra.MinimumNArgs(2)` (alias + ≥1 device).
- **Arg normalisation helper** (in `main.go`, shared by both commands): `splitDeviceArgs(args []string) []string` — for each arg, `strings.Split(a, ",")`, `strings.TrimSpace` each piece, append non-empty. Returns the flattened clean list. Both commands error with `no devices specified` if the result is empty.
- **deny `all`-notice:** after `DenyDevices` succeeds, if the (post-mutation) share still contains `"all"` and the normalised request did not include `"all"`, print the note line. Re-find the share via the same alias to inspect the resulting `AllowedDevices` (or have the command read it back from `cfg`).

## What Goes Where
- **Implementation Steps** (`[ ]`): config helpers + tests, cobra commands + registration, doc updates — all in-repo.
- **Post-Completion** (no checkboxes): optional manual check against a live daemon to observe hot-reload re-applying the ACL.

## Implementation Steps

### Task 1: Add `AllowDevices` / `DenyDevices` helpers + unit tests
**Files:**
- Create: `internal/agent/config/shares.go`
- Create: `internal/agent/config/shares_test.go`

- [x] add `findShare(alias) (*ShareConfig, error)` returning `&c.Shares[i]` or `share %q not found`
- [x] add `AllowDevices(alias, devices) (added []string, err error)` with two-way dedupe, input-order preservation
- [x] add `DenyDevices(alias, devices) (removed, notFound []string, err error)` with dedup'd `notFound`, allowing an empty result
- [x] write table-driven tests for every case in Testing Strategy, including the case-sensitivity case and a Save→Load round-trip (write to a `t.TempDir()` file, reload, assert `AllowedDevices`; assert the node is gone after emptying)
- [x] run tests — must pass before next task: `go test ./internal/agent/config/...`

### Task 2: Add `shareAllowCmd` / `shareDenyCmd` (+ arg helper) and register them
**Files:**
- Modify: `cmd/hubfuse/main.go`
- Create: `cmd/hubfuse/main_test.go`

- [x] add `splitDeviceArgs(args []string) []string` (comma-split + trim + drop-empty)
- [x] add `shareAllowCmd()` (`Use: "allow <alias> <device>..."`, `Short`, `Args: cobra.MinimumNArgs(2)`) — load → `devs := splitDeviceArgs(args[1:])` (error `no devices specified` if empty) → `cfg.AllowDevices(args[0], devs)` → `config.Save` → print per output spec
- [x] add `shareDenyCmd()` (`Use: "deny <alias> <device>..."`, `Args: cobra.MinimumNArgs(2)`) — load → `devs := splitDeviceArgs(...)` → `cfg.DenyDevices(...)` → `config.Save` → print removed + a `warning:` line per not-found name → emit the `still allows ALL devices` note when applicable
- [x] register both in `shareCmd.AddCommand(...)` at `main.go:53`, after `shareListCmd()`
- [x] add `cmd/hubfuse/main_test.go` with a `TestSplitDeviceArgs` table test
- [x] run `make build`, `make vet`, and `go test ./cmd/...` — must pass before next task

### Task 3: Verify acceptance criteria
- [x] `share allow <alias> dev` adds; re-running is a no-op (dedup) — covered by unit tests
- [x] `share allow <alias> all` stores the `all` token; daemon would treat it as `AllowAll`
- [x] `share allow <alias> a,b` and `share allow <alias> a b` are equivalent (comma-split)
- [x] `share deny <alias> dev` removes; denying an absent name warns but succeeds
- [x] `share deny <alias> dev` on an `all`-share prints the "still allows ALL devices" note
- [x] unknown alias errors on both commands
- [x] `make test-unit` and `make vet` clean

### Task 4: Documentation + ship
- [x] update `README.md` share-command list to include `allow`/`deny` (if it enumerates subcommands)
- [x] update `CLAUDE.md` only if a new pattern was introduced (none expected)
- [x] move this plan to `docs/plans/completed/`
- [x] commit and push (per CLAUDE.md workflow), then open the PR

## Post-Completion
*Items requiring manual intervention or external systems — informational only*

**Manual verification:**
- With a live daemon: `hubfuse share allow <alias> <peer>`, confirm `agent.log` shows a config reload and the peer can now mount; `hubfuse share deny <alias> <peer>`, confirm access is revoked — all without a daemon restart.
