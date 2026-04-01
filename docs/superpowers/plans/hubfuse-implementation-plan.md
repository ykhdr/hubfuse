# HubFuse Implementation Plan

## Context

HubFuse — инструмент для объединения устройств в локальной сети с прозрачным файловым доступом через SSHFS. Проект greenfield: существует только design spec (`docs/superpowers/specs/2026-03-22-hubfuse-design.md`) и пустой README. Нужно реализовать два бинарника (`hubfuse-hub` и `hubfuse-agent`), связанных через gRPC с mTLS.

**Spec:** `docs/superpowers/specs/2026-03-22-hubfuse-design.md`

---

## Phase 1: Scaffolding + Protobuf

**Goal:** Компилируемый проект с proto-стабами.

**Files:**
- `go.mod` — module `github.com/ykhdr/hubfuse`, Go 1.26
- `Makefile` — target `proto-gen` для кодогенерации
- `proto/hubfuse.proto` — полный gRPC-контракт:
  - Service `HubFuse` с 9 RPC: `Join`, `Register`, `Rename`, `Heartbeat`, `UpdateShares`, `Deregister`, `Subscribe` (server stream), `RequestPairing`, `ConfirmPairing`
  - Messages: `Share`, `DeviceInfo`, `Event` (oneof: DeviceOnline, DeviceOffline, SharesUpdated, PairingRequested, PairingCompleted)
- Stub `main.go` файлы в `cmd/hubfuse-hub/` и `cmd/hubfuse-agent/`
- Package stubs: `internal/hub/`, `internal/agent/`, `internal/common/`

**Deps:** `google.golang.org/grpc`, `google.golang.org/protobuf`, `modernc.org/sqlite`, `github.com/ykhdr/kdl-config`, `github.com/spf13/cobra`, `github.com/fsnotify/fsnotify`, `golang.org/x/crypto`, `github.com/pkg/sftp`, `github.com/google/uuid`

**Verify:** `make proto-gen && go build ./... && go vet ./...`

---

## Phase 2: Common Utilities (TLS + Logging)

**Goal:** Фундамент безопасности и наблюдаемости.

**Files:**
- `internal/common/tls.go`:
  - `GenerateCA()` — self-signed CA для хаба
  - `SignClientCert(caCert, caKey, deviceID)` — клиентский серт с device_id в CN
  - `LoadTLSServerConfig(caCert, serverCert, serverKey)` — mTLS server config
  - `LoadTLSClientConfig(caCert, clientCert, clientKey)` — mTLS client config
  - `ExtractDeviceID(ctx)` — извлечение device_id из peer certificate
  - `SavePEM()` / `LoadPEM()`
- `internal/common/logging.go`:
  - `SetupLogger(level, output)` — `log/slog` с JSON handler
- `internal/common/errors.go`:
  - Ошибки → gRPC status codes (`NicknameTaken`, `UnsupportedProtocol`, `InvalidInviteCode`, `MaxAttemptsExceeded`, `InviteExpired`)
- `internal/common/version.go`:
  - `const ProtocolVersion = 1`

**Verify:** `go test ./internal/common/...` — unit tests для TLS (генерация CA, подпись, ExtractDeviceID)

---

## Phase 3: Hub Storage (SQLite)

**Goal:** Полностью протестированный data access layer.

**Can run in parallel with Phase 6.**

**Files:**
- `internal/hub/store/models.go` — `Device`, `Share`, `Pairing`, `PendingInvite` structs
- `internal/hub/store/store.go` — `Store` interface (CRUD для devices, shares, pairings, pending_invites)
- `internal/hub/store/sqlite.go` — SQLite реализация с `modernc.org/sqlite`:
  - `NewSQLiteStore(dbPath)` — open + migrate
  - Все 4 таблицы из спеки, `allowed_devices` как JSON array
  - Транзакции для `SetShares` (delete old + insert new)
- `internal/hub/store/sqlite_test.go` — тесты на `:memory:` SQLite

**Verify:** `go test ./internal/hub/store/...`

---

## Phase 4: Hub Registry + Event Broadcasting

**Goal:** Бизнес-логика хаба — регистрация, heartbeat, события, pairing.

**Files:**
- `internal/hub/registry.go`:
  - `Registry` struct с `Store`, subscribers map (`device_id → chan *pb.Event`), mutex
  - `Join()`, `Register()`, `Rename()`, `Heartbeat()`, `UpdateShares()`, `Deregister()`
  - `Subscribe()` → канал + unsubscribe func
  - `Broadcast(event, excludeDevice)` — non-blocking send, buffer 64
- `internal/hub/heartbeat.go`:
  - `HeartbeatMonitor` — goroutine с ticker, проверяет stale devices (>30s), broadcast DeviceOffline
- `internal/hub/pairing.go`:
  - `RequestPairing()`, `ConfirmPairing()`, `GenerateInviteCode()` (HUB-XXX-YYY, crypto/rand)
  - Max 5 attempts, 5 min TTL

**Verify:** `go test ./internal/hub/...` — unit tests (mock Store interface)

---

## Phase 5: Hub gRPC Server + CLI

**Goal:** Работающий бинарник хаба.

**Files:**
- `internal/hub/server.go`:
  - gRPC server, implements `pb.HubFuseServer`
  - Auth interceptor: skip mTLS for `Join`, require for all others
  - TLS config: `VerifyClientCertIfGiven` + interceptor enforcement
- `internal/hub/hub.go`:
  - `Hub` lifecycle: `NewHub()`, `Start(ctx)`, `Stop()`
  - First start: generate CA + server cert → `~/.hubfuse-hub/tls/`
  - SQLite: `~/.hubfuse-hub/hubfuse.db`
  - PID file: `~/.hubfuse-hub/hubfuse-hub.pid`
- `cmd/hubfuse-hub/main.go`:
  - Cobra: `start`, `stop`, `status`
  - Signal handling: SIGINT/SIGTERM → graceful shutdown

**Verify:** `go build ./cmd/hubfuse-hub/` + integration test (in-process hub, Join → Register → Subscribe → verify events)

---

## Phase 6: Agent Config + Identity + Keys

**Goal:** Агент может парсить конфиг, управлять идентификацией и SSH-ключами.

**Can run in parallel with Phase 3.**

**Files:**
- `internal/agent/config/config.go`:
  - Config structs для KDL (Device, Hub, Agent, Shares, Mounts)
  - `Load(path)`, `NormalizePermissions()`, `ExpandTilde()`
- `internal/agent/config/watcher.go`:
  - Hot-reload через fsnotify, `onChange(old, new)` callback
- `internal/agent/config/diff.go`:
  - `ComputeDiff(old, new)` → SharesChanged, MountsAdded, MountsRemoved
- `internal/agent/identity.go`:
  - `DeviceIdentity` (device_id + nickname), Load/Save, `GenerateDeviceID()` (UUID v4)
- `internal/agent/keys.go`:
  - `GenerateSSHKeyPair(dir)` (ed25519), `LoadPublicKey()`, `SavePeerPublicKey()`, `ListPairedDevices()`

**Verify:** `go test ./internal/agent/...`

---

## Phase 7: Agent Hub Client + CLI

**Goal:** Агент может подключаться к хабу, выполнять все RPC. CLI usable.

**Depends on:** Phase 5 (hub running), Phase 6 (config/identity)

**Files:**
- `internal/agent/client.go`:
  - `HubClient` — gRPC wrapper: `Join()`, `Register()`, `Heartbeat()`, `Subscribe()`, pairing RPCs
  - `DialInsecure()` для Join, mTLS для остального
- `internal/agent/connector.go`:
  - Reconnection с exponential backoff (1s→60s)
- `cmd/hubfuse-agent/main.go`:
  - Cobra: `join`, `start`, `stop`, `status`, `pair`, `devices`, `rename`
  - `share add/remove/list`, `mount add/remove/list`
  - `share`/`mount` commands modify config.kdl → daemon picks up via hot-reload

**Verify:** `go build ./cmd/hubfuse-agent/` + integration test (join + register + subscribe flow)

---

## Phase 8: Agent SSHFS Mounting + SSH Server

**Goal:** Агент может монтировать чужие шары и обслуживать свои.

**Can run in parallel with Phase 7.**

**Files:**
- `internal/agent/mounter.go`:
  - `Mounter` — tracks active mounts (`map[string]*Mount`)
  - `Mount()` — exec `sshfs -p <port> -o IdentityFile=<key> user@ip:<alias> <local>`
  - `Unmount()` — `umount` (macOS) / `fusermount -u` (Linux)
  - `UnmountAll()`, `UnmountDevice()`
- `internal/agent/sshserver.go`:
  - Embedded SSH server via `golang.org/x/crypto/ssh` + `github.com/pkg/sftp`
  - Alias mapping: `alias → local path` (NOT full filesystem)
  - Public key auth: check against `known_devices/` + `allowed-devices` config
  - `UpdateShares()`, `UpdateAllowedKeys()`

**Verify:** Integration test — start SSH server, connect SFTP client, verify file access

---

## Phase 9: Agent Daemon (Full Lifecycle)

**Goal:** Полный демон — event loop, heartbeat, hot-reload, reconnection, graceful shutdown.

**Depends on:** Phases 7 + 8

**Files:**
- `internal/agent/daemon.go`:
  - `Daemon` orchestrator: config, client, mounter, sshServer, watcher
  - `Run(ctx)`:
    1. Load TLS → connect to hub (backoff)
    2. `Register` → process `devices_online` → auto-mount
    3. Start `Subscribe` stream goroutine
    4. Start heartbeat goroutine (10s)
    5. Start SSH server goroutine
    6. Start config watcher
    7. Wait for ctx cancel → `Shutdown()`
  - `Shutdown()`: unmount all → deregister → stop SSH → close gRPC
- `internal/agent/events.go`:
  - `handleDeviceOnline()` — auto-mount if paired + configured
  - `handleDeviceOffline()` — unmount
  - `handleSharesUpdated()` — remount if needed
  - `handlePairingRequested()` — prompt user for invite code
  - `handlePairingCompleted()` — save peer key, auto-mount
- `internal/agent/heartbeat.go`:
  - `runHeartbeat(ctx)` — ticker 10s

**Verify:** Integration test — full lifecycle (hub + daemon, events, reconnection, graceful shutdown)

---

## Phase 10: Integration Tests + Polish

**Goal:** End-to-end тесты, Makefile, финальная полировка.

**Files:**
- `tests/integration/join_test.go` — Join flow
- `tests/integration/pairing_test.go` — Full pairing between two agents
- `tests/integration/lifecycle_test.go` — Online/offline, shares update
- `tests/integration/reconnect_test.go` — Hub restart, agent reconnection
- `Makefile` targets: `build`, `test`, `proto-gen`, `lint`, `install`

**Verify:** `go test ./...` — all pass, `go build ./cmd/...` — both binaries

---

## Parallelization Map

```
Phase 1 (scaffolding)
  │
  v
Phase 2 (common: TLS, logging)
  │
  ├──────────────────────┐
  v                      v
Phase 3 (hub store)    Phase 6 (agent config, keys)
  │                      │
  v                      ├──────────────────┐
Phase 4 (hub registry)  │                   v
  │                      │          Phase 8 (mounter, SSH server)
  v                      │                   │
Phase 5 (hub server+CLI)│                   │
  │                      v                   │
  │              Phase 7 (agent client+CLI)  │
  │                      │                   │
  │                      └─────┬─────────────┘
  │                            v
  │                    Phase 9 (agent daemon)
  │                            │
  └────────────────────────────┤
                               v
                    Phase 10 (integration tests)
```

**Parallel pairs:** Phase 3 || Phase 6, Phase 7 || Phase 8

---

## Key Architectural Decisions

1. **Join auth bypass:** `tls.VerifyClientCertIfGiven` + gRPC interceptor enforcing mTLS for non-Join methods. One server, not two.
2. **Event fan-out:** Buffered channels (64), non-blocking broadcast. Slow subscriber drops events (logged).
3. **SSH server alias mapping:** SFTP server exposes aliases, not real paths. `projects` → `/home/user/projects`.
4. **PID files** for `stop`/`status`: `~/.hubfuse-hub/hubfuse-hub.pid`, `~/.hubfuse/hubfuse-agent.pid`.
5. **Config hot-reload:** fsnotify → diff → `UpdateShares` if shares changed, mount/unmount as needed.
