# HubFuse

Network file sharing for local networks. Mount remote directories transparently via SSHFS, coordinated by a central hub server.

## How it works

HubFuse uses a hub-and-spoke architecture:

- **Hub** (`hubfuse-hub`) — a central gRPC server that tracks devices, manages pairing, and broadcasts events.
- **Agent** (`hubfuse`) — a daemon on each device that connects to the hub, exports local directories via an embedded SSH server, and mounts remote shares via SSHFS.

All communication is secured with mTLS. Devices pair using short-lived invite codes to exchange SSH public keys.

## Requirements

- Go 1.25+
- `protoc` with `protoc-gen-go` and `protoc-gen-go-grpc` (for proto regeneration only)
- `sshfs` installed on agent machines (see [Installing the mount tool](#installing-the-mount-tool))

### Installing the mount tool

Agents mount remote shares with `sshfs`. The FUSE implementation behind it
depends on the platform.

**macOS** — FUSE-T is the recommended, kext-free path. Its casks live in a
third-party tap, so tap it first:

```bash
brew tap macos-fuse-t/homebrew-cask
brew install --cask fuse-t fuse-t-sshfs
```

FUSE-T runs a local NFS server instead of a kernel extension, so there is
nothing to approve and no reboot. The alternative, macFUSE, installs a kernel
extension that requires System Settings approval plus a reboot, and on Apple
Silicon also forces enabling reduced-security mode. To use FUSE-T set
`mount-tool "fuse-t"` in the agent config (see [Configuration](#configuration));
FUSE-T is macOS-only. Note: FUSE-T is free for personal use; commercial use
requires a license (see fuse-t.org).

**Linux** — install the distribution's `sshfs` package (which uses `fusermount`),
e.g. `apt install sshfs` or `dnf install fuse-sshfs`. The default
`mount-tool "sshfs"` applies; `"fuse-t"` is not available on Linux.

## Quick start

```bash
# Build
make build

# Install binaries to $GOPATH/bin
make install

# Start the hub (default :9090)
hubfuse-hub start

# On the hub host — issue a single-use token for the joining device
hubfuse-hub issue-join
# -> HUB-AB2-9XY.mfqwcylbmfqwcylbmfqwcylb
#    ^^^^^^^^^^^  ^^^^^^^^^^^^^^^^^^^^^^^^
#    DB prefix    hub TLS fingerprint (26-char base32)

# On each device — join the hub with the full token, then start the agent
hubfuse join <hub-address>:9090 --token HUB-AB2-9XY.mfqwcylbmfqwcylbmfqwcylb
hubfuse start
```

Join tokens expire after 10 minutes and are consumed atomically by the first
Join call that presents them — a Join that fails after claiming the token
(for example, a nickname collision) does not restore it, so a retry requires
issuing a fresh token. This is deliberate: it keeps the token single-use under
concurrent requests and prevents exposing it to brute-force attempts. Configure
the TTL with `join-token-ttl "<duration>"` in `~/.hubfuse-hub/config.kdl`.

### Security

The suffix after the `.` is a truncated SHA-256 fingerprint of the hub's TLS
leaf certificate (first 16 bytes, base32-encoded). During `hubfuse join`, the
agent pins this fingerprint against the server's certificate before sending
any data — an active MITM presenting a different certificate is rejected before
the Join RPC is ever issued. Rotating the hub's server certificate invalidates
all outstanding join tokens; issue fresh ones after rotation.

## Installation

### Install via `go install`

With Go installed, install either binary directly from the module path:

```bash
go install github.com/ykhdr/hubfuse/cmd/hubfuse@latest
go install github.com/ykhdr/hubfuse/cmd/hubfuse-hub@latest
```

This requires a Go toolchain and `$GOPATH/bin` (or `$GOBIN`) on your `PATH`.

### Updating

Updating is the same command — re-run `go install ...@latest` to pull the
newest released tag:

```bash
go install github.com/ykhdr/hubfuse/cmd/hubfuse@latest
go install github.com/ykhdr/hubfuse/cmd/hubfuse-hub@latest
```

**Upgrade the hub before the agents.** From v0.1.3 the agent keeps its hub
connection alive with a gRPC keepalive ping every 10 seconds, and a hub from
v0.1.2 or earlier rejects that cadence with `GOAWAY too_many_pings` and closes
the connection.

A *running* agent is not affected: its heartbeat keeps the connection busy
enough that no ping is ever sent, and an agent talking to an old hub survives
indefinitely. What is affected is any connection that goes quiet for ~30
seconds — most visibly `hubfuse join`, which holds a connection open while it
waits for you to type a nickname. Each rejection also permanently doubles that
connection's keepalive interval, which weakens the agent's ability to notice a
hub that has gone away without closing the socket.

If it happens, the agent says so once, at error level, naming the hub as too
old. Upgrading the hub is the fix; agents do not need to be downgraded or
restarted in any particular order afterwards.

One related setting: leave `HUBFUSE_HEARTBEAT_INTERVAL` alone unless you have a
reason. Anything at or above 30 seconds exceeds the hub's own liveness timeout,
so the hub marks the device offline and peers unmount its shares — against any
hub version, not just old ones.

### Running the agent on macOS

macOS gates access to the local network **per binary**, and grants it only
through a GUI prompt. That has two consequences a HubFuse user will hit:

- A daemon started over SSH can never be shown the prompt. It registers with the
  hub, works for roughly forty seconds, and is then cut off: every dial fails
  with `connect: no route to host` while `ping` to the same hub keeps working.
- The approval is bound to that exact binary. Rebuilding or reinstalling
  `hubfuse` changes its identity, and you have to approve the new one.

The supported way to run it is as a LaunchAgent in your logged-in GUI session,
where the prompt can actually appear:

```bash
hubfuse install-agent                                  # writes the plist
launchctl bootstrap gui/$(id -u) \
    ~/Library/LaunchAgents/com.github.ykhdr.hubfuse.plist
```

Run the `launchctl` line **from a terminal on the Mac itself**, not over SSH,
and approve the local-network prompt when it appears. If you miss it, the
setting is under System Settings → Privacy & Security → Local Network.

From v0.1.4 the agent detects this state and says so once, at error level,
instead of logging a dial failure every retry forever. It only does so when the
hub address is actually on the local network — `no route to host` to a routable
address is an ordinary routing problem and is reported as one.

One more macOS detail, unrelated to permissions: binaries that are not
code-signed are killed on launch by macOS 26 (`Killed: 9`, with no output). A
binary you built yourself locally is signed by the toolchain; one copied from
elsewhere may not be, and `codesign -f -s - ./hubfuse` fixes it.

### Prebuilt binaries

If you don't have Go, prebuilt binaries are published on the project's
[GitHub Releases](https://github.com/ykhdr/hubfuse/releases) page as `tar.gz`
archives (one per binary, per OS/arch) alongside a `checksums.txt`.

### Version

Both binaries report their version. The `version` subcommand prints a detailed
block (version, commit, build date, Go version, OS/arch):

```bash
hubfuse version
hubfuse-hub version
```

The `--version` flag prints the single-line version (e.g. `hubfuse --version`).

## Configuration

Agent configuration lives in `~/.hubfuse/config.kdl` (KDL format). Example:

```kdl
device {
    nickname "my-laptop"
}

hub {
    address "192.168.1.10:9090"
}

agent {
    ssh-port 2222
    mount-tool "sshfs"   // "sshfs" (default) | "fuse-t"
}

shares {
    share "/home/user/projects" alias="projects" permissions="rw" {
        allowed-devices "all"
    }
}

mounts {
    mount device="work-pc" share="docs" to="/mnt/hubfuse/docs"
}
```

Changes to `shares` and `mounts` in `config.kdl` are hot-reloaded — no restart
needed. Settings in the `agent` block (`ssh-port`, `mount-tool`) are read once at
startup and require a daemon restart to take effect.

`ssh-port` must be free when the daemon starts. If something else is already
listening there — most often a leftover `hubfuse` process — `hubfuse start`
fails immediately and says so:

```
start SSH server: listen on port 2222: listen tcp :2222: bind: address already in use
```

The daemon deliberately does not continue in that state. Without its own SSH
server it has nothing to serve, and registering anyway would hand every peer a
port owned by another process — peers would then mount from whatever answers
there rather than getting a clean failure. Free the port (`hubfuse stop`, or
`lsof -i :2222`) or pick another one, then start again. The same rule applies
while the daemon runs: if the SSH server stops serving, the daemon leaves the
hub and exits instead of staying online behind a port it no longer holds.

### Mount tool

`agent { mount-tool "..." }` selects the mount backend for this device
(device-global; it applies to every mount). Allowed values:

- `"sshfs"` (default) — the distribution `sshfs` (macFUSE on macOS, `fusermount`
  on Linux).
- `"fuse-t"` — macOS only; requires `fuse-t-sshfs`
  (`brew tap macos-fuse-t/homebrew-cask && brew install --cask fuse-t fuse-t-sshfs`).
  The kext-free path described in
  [Installing the mount tool](#installing-the-mount-tool). Selecting `"fuse-t"`
  on a non-macOS host is a configuration error.

Unlike `shares` and `mounts`, changing `mount-tool` requires a daemon restart —
the backend is selected once at startup and is not picked up by hot-reload.

Both values run the `sshfs` binary found on `PATH`; `mount-tool` does not pick a
binary by itself. If both macFUSE's and FUSE-T's `sshfs` are installed, whichever
comes first on `PATH` is the engine that actually serves the mount — so keep only
one installed (e.g. `brew uninstall sshfs-mac` to let FUSE-T win). As a safety
net, with `mount-tool "fuse-t"` the agent warns at startup when the FUSE-T runtime
isn't detected, and a mount that never materializes is reported as an error rather
than logged as a (false) success.

### Share access control

`permissions` and `allowed-devices` are enforced by the agent's SFTP
server for every incoming request:

- `permissions="ro"` rejects every SFTP write (create, write, rename,
  remove, mkdir, chmod, symlink, link).
- `allowed-devices` lists the peers that may see and access the share.
  Tokens match the peer's **nickname** or raw **device_id**. Use the
  literal token `"all"` to grant access to every paired device.
  Nickname tokens resolve correctly even before the peer comes online
  (e.g. right after a daemon restart) because paired nicknames are
  persisted locally and loaded before the SSH server begins serving.
  If a peer's nickname changes, the mapping self-heals on the next
  online event or daemon restart.

Defaults are secure: omitting `permissions` treats the share as
read-only, and omitting (or leaving empty) `allowed-devices` makes the
share inaccessible to every peer. This is a deliberate tightening — in
earlier releases these fields were documented but not enforced.

## Commands

### `hubfuse-hub`

| Command | Description |
|---|---|
| `start [--listen :9090] [--device-retention 168h] [-d]` | Start the hub server (use `-d` to run in the background) |
| `stop` | Stop the running hub |
| `status` | Show hub status (running/stopped, pid) |
| `issue-join [--ttl 10m]` | Issue a single-use join token; print it on stdout |
| `version` | Print version, commit, build date, Go version, and OS/arch |

Offline devices older than one week (`168h`) are pruned automatically. Customize the window with `--device-retention <duration>` or set `device-retention "<duration>"` in `~/.hubfuse-hub/config.kdl`. Use `0` to disable pruning.

### `hubfuse` (agent)

| Command | Description |
|---|---|
| `join <hub-address> --token HUB-XXX-YYY.<fp> [--force]` | Register this device with a hub using a token issued via `hubfuse-hub issue-join`; receives TLS certs. Refuses if already joined unless `--force` is passed. |
| `leave [--force-local]` | Permanently remove this device from the hub and wipe local TLS state. Pass `--force-local` to wipe even if the hub is unreachable. |
| `start [-d]` | Start the agent daemon |
| `stop` | Stop the running agent |
| `restart [-d]` | Stop the running agent (if any) and start a fresh one. Mirrors `start`: runs in the foreground by default, detaches with `-d`, and honors `--log-file`/`--log-level`/`--verbose` (logging flags apply to the foreground form; a detached restart uses default logging). |
| `status` | Show agent status |
| `devices` | List all devices known to the hub |
| `rename <nickname>` | Change this device's nickname |
| `pair <device>` | Request pairing with a remote device (prints invite code) |
| `share add <path> --alias <name> [--permissions ro\|rw] [--allow ...]` | Share a local directory |
| `share remove <alias>` | Remove a share |
| `share list` | List local shares |
| `share allow <alias> <device>...` | Grant device(s) access to an existing share |
| `share deny <alias> <device>...` | Revoke device(s) access to an existing share |
| `mount add <device>:<share> --to <path>` | Mount a remote share |
| `mount remove <device>:<share>` | Unmount |
| `mount list` | List mounts |
| `version` | Print version, commit, build date, Go version, and OS/arch |

### Recovery

If local state is lost (e.g. after wiping `~/.hubfuse/`) but the device record
still exists on the hub, ask the hub operator to prune it or wait for the
retention window to expire. Then rejoin with a fresh token as normal.
If the hub is unreachable, run `hubfuse leave --force-local` to wipe the stale
local state before rejoining a new hub.

## Development

```bash
make build              # compile all packages
make test               # run unit + integration tests
make test-unit          # unit tests only
make test-integration   # integration tests (120s timeout)
make vet                # static analysis
make proto-gen          # regenerate gRPC code from proto/hubfuse.proto
make release-snapshot   # build a local snapshot release with GoReleaser (no publish)
make release-check      # validate .goreleaser.yaml (goreleaser check)
```

## License

HubFuse is licensed under the [Apache License 2.0](LICENSE).
