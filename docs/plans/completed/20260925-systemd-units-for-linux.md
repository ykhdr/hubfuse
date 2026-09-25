# systemd units so Linux survives a reboot (#117)

> Revised after review. Three of the first draft's claims were measurable and were measured; two
> were false. The PATH rationale asserted the macOS failure mode and does not hold on systemd, and
> `After=network-online.target` is inert in a user manager. Both are corrected below rather than
> shipped as permanent code comments — this repo has already retracted two overclaimed causal
> statements (#74, #78) and a third was one commit away.

## Overview

`hubfuse install-agent` exists for macOS. Linux gets a sentence telling the user to sort it out
(`cmd/hubfuse/launchagent.go:159`):

```
on Linux run the daemon under systemd or start it with "hubfuse start --daemon"
```

Both halves are a shrug. `--daemon` detaches and dies with the machine; "run it under systemd" hands
the user two units to write for two binaries.

Measured on the test bed today: after a reboot neither `hubfuse-hub` nor the Linux agent comes back.
Until someone remembers two commands, a Mac that is working perfectly sees nothing at all — the hub
is the single point of failure for the whole system.

## Context (from discovery)

- `cmd/hubfuse/launchagent.go` is the pattern: a template constant, a path helper, a cobra command,
  `runInstallAgent(out, goos, force)` with **GOOS passed in** so the Linux CI exercises both the
  happy path and the refusal, and a separate `installAgentNextSteps` with its own test.
- **`cmd/hubfuse/launchagent_test.go:85` pins the literal substring `"systemd"`** in the Linux
  refusal (`TestRunInstallAgent_RefusesOffDarwin`). The first draft changed that message and did not
  list the test file. Keeping the word is free — the new message names `install-service`, which
  installs a systemd unit — but the file goes in Task 1's list either way.
- `hubfuse start` and `hubfuse-hub start` both run in the foreground and handle SIGTERM, which is
  what `Type=simple` needs. No `--daemon` in the unit: systemd owns the process.
- `Daemon.Shutdown` calls `d.mounter.UnmountAllForce` (`internal/agent/daemon.go:1838`), so the
  agent unmounts its own FUSE mounts on SIGTERM. That decides KillMode — see below.
- The hub shells out to nothing (no `exec.Command`/`exec.LookPath` anywhere under `internal/hub/`
  or `cmd/hubfuse-hub/`), so its unit needs no PATH.

### Measured on this box, because the first draft guessed

| claim | measurement | verdict |
| --- | --- | --- |
| "a service manager's environment is even emptier than launchd's" | `systemd-run --user --pipe --wait env` → PATH already contains `/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin` | **false** |
| sshfs is somewhere systemd cannot see | `which sshfs` → `/usr/bin/sshfs` | **false** |
| `After=network-online.target` orders the unit | `systemctl --user show network-online.target -p LoadState` → `LoadState=not-found` | **inert** |
| `systemd-analyze` is available for a mechanical check | systemd 249 present | **true** |

## Solution Overview

Two commands, one per binary, both writing **user** units to `~/.config/systemd/user/`:

| binary | unit | ExecStart |
| --- | --- | --- |
| `hubfuse` | `hubfuse-agent.service` | `<abs path> start` |
| `hubfuse-hub` | `hubfuse-hub.service` | `<abs path> start` |

**User units rather than system units**, and not for convenience. Both processes are per-user by
construction: the agent mounts into the user's home and holds their SSH keys, the hub keeps its
store in `~/.hubfuse-hub`. A system unit would need `User=`, a path outside `$HOME`, and root.

The settings, each with what actually justifies it:

- **`[Install] WantedBy=default.target`** — the one whose absence would defeat the entire point
  silently. A user manager has no `multi-user.target`; written with system-unit muscle memory, or
  omitted, `systemctl --user enable` links into a target that never activates and nothing starts at
  boot — while `enable --now` still succeeds in the interactive session, so the install looks fine
  and only a real reboot finds it. Pinned by a test AND by `systemd-analyze --user verify`.
- **`Restart=on-failure`, not `always`** — #98. A clean exit means a deliberate stop; relaunching it
  makes `hubfuse stop` a no-op. Since #74 the daemon retries an unreachable hub in-process, so a
  zero exit genuinely is "stop".
- **`RestartSec=30`** — the #98 `ThrottleInterval` argument, unchanged.
- **`KillMode=mixed`** — systemd's default `control-group` signals every process in the cgroup.
  `sshfs` is spawned without `-f` (`internal/agent/mounter.go`) so it self-daemonizes but stays in
  the unit's cgroup, and a plain `systemctl --user restart` would SIGTERM the live mounts out from
  under the agent instead of letting `Shutdown`'s `UnmountAllForce` do it — stale
  "Transport endpoint is not connected" mounts on every restart, which is #67's symptom
  reintroduced by the service manager. `mixed` sends the stop signal to the main process only.
- **`Environment=PATH=…`** — kept, but for a narrower reason than the first draft claimed. It is NOT
  the macOS bug: systemd's user PATH already covers `/usr/local/bin` and `/usr/bin`, where sshfs
  actually lives here. What it covers is (a) a **lingering** manager started at boot, which gets
  systemd's compiled-in default rather than a login session's imported environment, and (b) installs
  outside those two directories — `~/.local/bin`, `/snap/bin`, `/opt/*/bin`. The comment must say
  exactly that. Agent unit only; the hub shells out to nothing.
- **No `After=network-online.target`.** Measured inert in a user manager, and the agent survives a
  hubless start anyway (#74). Shipping an inert line with a comment implying it orders anything is
  how a future reader learns something false.
- **Logging goes to the journal**, systemd's default — no `StandardOutput=append:`. It is the
  idiomatic answer, it needs no path escaping, and the printed next-steps and README must say
  `journalctl --user -u hubfuse-agent -f` rather than leaving the operator looking for
  `~/.hubfuse/agent.log`, which is where the launchd plist puts it.

**Escaping.** `launchagent.go` treats XML-escaping as load-bearing and has a test for it; the
systemd analogue is real and different: `ExecStart=` is split on whitespace unless quoted, and `%`
introduces a specifier so a literal one must be doubled. A path containing either breaks the unit at
parse time. Mirror `xmlEscape` with a `systemdEscapeExec` helper and mirror its test.

## Testing Strategy

- **unit** (`cmd/hubfuse`, `cmd/hubfuse-hub`): every setting above pinned with the reason it exists,
  the `[Install]` line pinned separately, the escaping helper tested on a path with a space and a
  `%`, and the next-steps text pinned the way `TestInstallAgentNextSteps_SaysWhatIsTrue` pins the
  macOS one.
- **mechanical** (Task 3): `systemd-analyze --user verify` on both generated units. It needs no
  running manager and catches a missing `[Install]` or a quoting error as a parse error rather than
  as a passing substring assertion. If the CI image lacks it, keep it a documented local step rather
  than pretending.
- **no scenario test**: the suite has no systemd, and a test that writes a file and reads it back
  would restate the unit test at ten times the cost.
- **manual, on the test bed** — the only check that answers #117, and it belongs in Post-Completion
  rather than pretended in CI.

Deliberately NOT asserted: "the ExecStart path is absolute and symlink-resolved". `EvalSymlinks` is
called inside `runInstallAgent` on `os.Executable()`, exactly as in `launchagent.go`, where it is
also untested — the plist tests call the template helper with a literal. Making it testable means
injecting the resolver, which is a design change this plan does not need; claiming a test for it
would be the vacuous kind.

## Implementation Steps

### Task 1: `hubfuse install-service`

**Files:**
- Create: `cmd/hubfuse/systemdunit.go`
- Create: `cmd/hubfuse/systemdunit_test.go`
- Modify: `cmd/hubfuse/main.go` (register the command)
- Modify: `cmd/hubfuse/launchagent.go` (the refusal names the real command)
- Modify: `cmd/hubfuse/launchagent_test.go` (it pins that refusal at `:85`)

- [x] write the unit template with `[Install] WantedBy=default.target`, `Restart=on-failure`,
      `RestartSec=30`, `KillMode=mixed`, `Environment=PATH=…`, no `After=network-online.target`
- [x] each setting carries a comment stating what justifies it, with the PATH one saying the
      narrow measured truth rather than the macOS story
- [x] `systemdEscapeExec` mirroring `xmlEscape`: double `%`, quote a path containing whitespace
- [x] `runInstallService(out, goos, force)` mirroring `runInstallAgent`'s seam; refuse on non-Linux
      naming `install-agent` for darwin
- [x] `installServiceNextSteps`: `systemctl --user daemon-reload`, `enable --now`,
      `loginctl enable-linger` (noting it usually needs sudo over SSH — polkit has no active
      session there), and `journalctl --user -u` for logs
- [x] update `install-agent`'s Linux refusal to name `install-service`, keeping the literal word
      "systemd" so `launchagent_test.go:85` stays honest rather than being edited to match
- [x] tests: each setting pinned with its reason; `[Install]` pinned separately; escaping tested on
      a path with a space and with `%`; next-steps text pinned including the linger line
- [x] `go test ./cmd/...`

### Task 2: `hubfuse-hub install-service`

**Files:**
- Create: `cmd/hubfuse-hub/systemdunit.go`
- Create: `cmd/hubfuse-hub/systemdunit_test.go`
- Modify: `cmd/hubfuse-hub/main.go`

- [x] same shape, with **no** `Environment=PATH=` and **no** `KillMode=mixed`: the hub shells out to
      nothing and spawns no children, and the test states that absence is deliberate so neither is
      added later by symmetry
- [x] `Restart=on-failure` + `RestartSec=30` for the #98 reason
- [x] next-steps mirroring Task 1
- [x] tests mirroring Task 1, including the two deliberate absences
- [x] `go test ./cmd/...`

### Task 3: Verify acceptance criteria

- [x] `make test`, `make vet`, `make vulncheck` green
- [x] `systemd-analyze --user verify` clean on both generated units
- [x] both commands refuse cleanly on darwin

### Task 4: [Final] Documentation

- [x] README: a Linux section beside the macOS one — the two commands, `enable-linger` with its sudo
      caveat, and `journalctl --user -u` for logs
- [x] CLAUDE.md: the settings and what justifies each, including the two deliberate absences and the
      corrected PATH rationale
- [x] move this plan to `docs/plans/completed/`

## Post-Completion

**Manual verification on the test bed — the only check that answers #117:**
- install both units, `systemctl --user daemon-reload`, `enable --now`, `loginctl enable-linger`;
- **reboot the Linux box** — `enable --now` succeeding proves nothing about `[Install]`;
- confirm hub and agent are up unprompted and the Mac's existing mount still reads and writes;
- `systemctl --user restart hubfuse-agent` and confirm the Mac's mount survives it, which is what
  `KillMode=mixed` is for.
