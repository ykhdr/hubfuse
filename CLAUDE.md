# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Workflow

Always commit and push after completing an action (feature, fix, refactor, etc.) without waiting for explicit request.

## Project

HubFuse is a network file sharing system for local networks. Devices mount remote directories transparently via SSHFS, coordinated by a central gRPC hub server. Written in Go 1.25.

## Build & Test Commands

```bash
make build              # go build ./...
make test               # run all tests (unit + integration)
make test-unit          # go test ./internal/...
make test-integration   # go test ./tests/integration/... -timeout 120s
make vet                # go vet ./...
make proto-gen          # regenerate gRPC code from proto/hubfuse.proto
make install            # install hubfuse-hub and hubfuse-agent to $GOPATH/bin
```

Run a single test: `go test ./internal/hub/store/... -run TestName`

## Architecture

Hub-and-spoke model with two binaries:

- **hubfuse-hub** — Central gRPC server. Manages device registry, pairing, heartbeat monitoring, and event broadcasting. Stores state in SQLite (pure Go, no CGO via `modernc.org/sqlite`).
- **hubfuse-agent** — Daemon running on each device. Connects to hub, exports local directories via an embedded SSH server, and mounts remote shares via SSHFS.

### Communication

All communication uses gRPC with mTLS. The proto definition is in `proto/hubfuse.proto`. Key flows:

1. **Join** (unauthenticated) — Device gets TLS client cert from hub
2. **Register** — Device announces shares/SSH port, receives online device list
3. **Subscribe** — Server-streaming RPC pushes events (DeviceOnline/Offline/SharesUpdated/PairingRequested/PairingCompleted)
4. **Pairing** — Two-step invite code exchange (RequestPairing → ConfirmPairing) to swap SSH public keys

Device identity is extracted from the mTLS certificate CN field via a gRPC interceptor (`internal/hub/interceptor.go`).

### Hub internals (`internal/hub/`)

- `hub.go` — Orchestrator that wires components and manages lifecycle
- `server.go` — gRPC service implementation
- `registry.go` — In-memory device state, event fanout to subscribers
- `heartbeat.go` — Monitors device liveness (10s interval, 30s timeout)
- `pairing.go` — Invite code generation, validation (5-min TTL, 5-attempt limit)
- `store/` — `Store` interface (`store.go`) with SQLite implementation (`sqlite.go`). Schema: devices, shares, pairings, pending_invites

### Agent internals (`internal/agent/`)

- `daemon.go` — Orchestrator
- `client.go` — gRPC client wrapper
- `connector.go` — Hub connection with backoff retry
- `mounter.go` — SSHFS mount/unmount lifecycle
- `sshserver.go` — Embedded SSH server (default port 2222) for incoming SSHFS
- `config/` — KDL format config parser (`config.go`), diff detection (`diff.go`), hot-reload via fsnotify (`watcher.go`)

### Shared utilities (`internal/common/`)

TLS cert helpers, structured logging setup, protocol version constant, common error types.

## Key Patterns

- **Config format**: KDL (parsed via `github.com/sblinch/kdl-go`), not YAML/JSON
- **CLI framework**: Cobra (`github.com/spf13/cobra`)
- **Data layer**: All hub persistence goes through the `store.Store` interface — add new queries there, implement in `sqlite.go`
- **Events**: Registry fans out events to subscriber channels; agents process them in `events.go`
- **Integration tests** (`tests/integration/`): Spin up an in-process hub with in-memory SQLite, create TLS certs programmatically, and test full gRPC flows
