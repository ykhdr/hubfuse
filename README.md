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
- `sshfs` installed on agent machines

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
# -> HUB-AB2-9XY

# On each device — join the hub with the token, then start the agent
hubfuse join <hub-address>:9090 --token HUB-AB2-9XY
hubfuse start
```

Join tokens expire after 10 minutes and are consumed atomically by the first
Join call that presents them — a Join that fails after claiming the token
(for example, a nickname collision) does not restore it, so a retry requires
issuing a fresh token. This is deliberate: it keeps the token single-use under
concurrent requests and prevents exposing it to brute-force attempts. Configure
the TTL with `join-token-ttl "<duration>"` in `~/.hubfuse-hub/config.kdl`.

## Configuration

Agent configuration lives in `~/.hubfuse/config.kdl` (KDL format). Example:

```kdl
nickname "my-laptop"
hub "192.168.1.10:9090"
ssh-port 2222

shares {
    share "/home/user/projects" alias="projects" permissions="rw" {
        allowed-devices "all"
    }
}

mounts {
    mount device="work-pc" share="docs" to="/mnt/hubfuse/docs"
}
```

Changes to `config.kdl` are hot-reloaded — no restart needed.

### Share access control

`permissions` and `allowed-devices` are enforced by the agent's SFTP
server for every incoming request:

- `permissions="ro"` rejects every SFTP write (create, write, rename,
  remove, mkdir, chmod, symlink, link).
- `allowed-devices` lists the peers that may see and access the share.
  Tokens match the peer's nickname or device_id. Use the literal token
  `"all"` to grant access to every paired device.

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

Offline devices older than one week (`168h`) are pruned automatically. Customize the window with `--device-retention <duration>` or set `device-retention "<duration>"` in `~/.hubfuse-hub/config.kdl`. Use `0` to disable pruning.

### `hubfuse` (agent)

| Command | Description |
|---|---|
| `join <hub-address> --token HUB-XXX-YYY` | Register this device with a hub using a token issued via `hubfuse-hub issue-join`; receives TLS certs |
| `start [-d]` | Start the agent daemon |
| `stop` | Stop the running agent |
| `status` | Show agent status |
| `devices` | List all devices known to the hub |
| `rename <nickname>` | Change this device's nickname |
| `pair <device>` | Request pairing with a remote device (prints invite code) |
| `share add <path> --alias <name> [--permissions ro\|rw] [--allow ...]` | Share a local directory |
| `share remove <alias>` | Remove a share |
| `share list` | List local shares |
| `mount add <device>:<share> --to <path>` | Mount a remote share |
| `mount remove <device>:<share>` | Unmount |
| `mount list` | List mounts |

## Development

```bash
make build              # compile all packages
make test               # run unit + integration tests
make test-unit          # unit tests only
make test-integration   # integration tests (120s timeout)
make vet                # static analysis
make proto-gen          # regenerate gRPC code from proto/hubfuse.proto
```
