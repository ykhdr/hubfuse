# HubFuse

Network file sharing for local networks. Mount remote directories transparently via SSHFS, coordinated by a central hub server.

## How it works

HubFuse uses a hub-and-spoke architecture:

- **Hub** (`hubfuse-hub`) — a central gRPC server that tracks devices, manages pairing, and broadcasts events.
- **Agent** (`hubfuse-agent`) — a daemon on each device that connects to the hub, exports local directories via an embedded SSH server, and mounts remote shares via SSHFS.

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

# On each device — join the hub and start the agent
hubfuse-agent join --hub <hub-address>:9090
hubfuse-agent start
```

## Configuration

Agent configuration lives in `~/.hubfuse/config.kdl` (KDL format). Example:

```kdl
nickname "my-laptop"
hub "192.168.1.10:9090"
ssh-port 2222

shares {
    projects "/home/user/projects" permissions="rw" allowed="all"
}

mounts {
    docs "work-pc" "docs" "/mnt/hubfuse/docs"
}
```

Changes to `config.kdl` are hot-reloaded — no restart needed.

## Development

```bash
make build              # compile all packages
make test               # run unit + integration tests
make test-unit          # unit tests only
make test-integration   # integration tests (120s timeout)
make vet                # static analysis
make proto-gen          # regenerate gRPC code from proto/hubfuse.proto
```

## Project structure

```
cmd/
  hubfuse-hub/          # hub CLI entry point
  hubfuse-agent/        # agent CLI entry point
proto/
  hubfuse.proto         # gRPC service definition
internal/
  hub/                  # hub server: registry, heartbeat, pairing, gRPC handlers
    store/              # Store interface + SQLite implementation
  agent/                # agent daemon: connector, mounter, SSH server, config
    config/             # KDL config parser, diff detection, hot-reload
  common/               # TLS helpers, logging, shared types
tests/
  integration/          # end-to-end gRPC tests with in-process hub
```
