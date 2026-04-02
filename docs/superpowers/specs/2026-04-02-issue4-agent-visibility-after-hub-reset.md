# Issue #4: Agent visibility after hub reset

## Problem

After a hub reset (deleting `~/.hubfuse-hub` and restarting), agents with old TLS certificates cannot connect because the hub generates a new CA. The agent retries forever with no clear error. Additionally, there is no way to list all devices (including offline) or get meaningful errors when pairing with unavailable devices.

## Changes

### 1. TLS certificate error detection in agent

**Files:** `internal/agent/connector.go`

When `DialWithMTLS` fails in the `Connect()` retry loop, classify the error:

- If `errors.As` matches `x509.UnknownAuthorityError` or `x509.CertificateInvalidError` — return immediately with error: `"hub certificate not trusted — the hub CA may have changed, please re-join with: hubfuse join <hub-addr>"`
- All other errors (network unreachable, connection refused, timeout) — retry with backoff as before

Add a private helper `isTLSCertError(err error) bool` that checks for these x509 error types. The check must unwrap through gRPC transport errors.

The `Daemon.Run()` method receives this error, logs it at Error level, and exits.

### 2. ListDevices RPC — show all devices with status

**Proto** (`proto/hubfuse.proto`):

```protobuf
message ListDevicesRequest {
  string device_id = 1;
}

message ListDevicesResponse {
  repeated DeviceInfo devices = 1;
}

rpc ListDevices(ListDevicesRequest) returns (ListDevicesResponse);
```

Add `string status = 6;` field to existing `DeviceInfo` message.

**Store** (`internal/hub/store/`):

New method in `Store` interface and `sqlite.go`:

```go
ListAllDevices(ctx context.Context) ([]*Device, error)
```

Query: `SELECT device_id, nickname, status, last_ip, ssh_port, last_heartbeat FROM devices`

**Server** (`internal/hub/server.go`):

New `ListDevices()` handler:
1. Extract device_id from mTLS cert (authentication via existing interceptor)
2. Call `store.ListAllDevices()`
3. For each device, fetch shares via `store.GetShares()`
4. Return all devices with status field populated

**Agent CLI** (`cmd/hubfuse-agent/`):

The `devices` command calls `ListDevices` RPC instead of reading from `RegisterResponse`. Displays all devices with their status (online/offline).

### 3. RequestPairing — distinguish device states

**Files:** `internal/hub/pairing.go` or `internal/hub/server.go` (wherever pairing request is handled)

Change the pairing request flow:
1. Look up target device by nickname via `store.GetDeviceByNickname()`
2. If device does not exist → error: `"device not found: no device with nickname <nickname>"`
3. If device exists but status is offline → error: `"device offline: <nickname> is not currently connected"`
4. If device is online → proceed with existing pairing flow

No proto changes needed — errors are returned via existing `error` field in `PairingResponse`.

## Testing

### Unit tests

- **`internal/agent/connector_test.go`** — `TestIsTLSCertError`: table-driven test that `x509.UnknownAuthorityError` and `x509.CertificateInvalidError` return true, `net.OpError` and generic errors return false
- **`internal/hub/store/sqlite_test.go`** — `TestListAllDevices`: create devices with different statuses, verify all returned
- **`internal/hub/server_test.go`** — `TestListDevices`: register devices, call ListDevices RPC, verify all returned with correct statuses
- **`internal/hub/pairing_test.go`** (or `server_test.go`) — `TestRequestPairing_DeviceNotFound`, `TestRequestPairing_DeviceOffline`: verify distinct error messages

### Integration tests (`tests/integration/`)

- **`TestListDevices_AllStatuses`**: join + register two devices, one goes offline, ListDevices returns both with correct statuses
- **`TestPairing_OfflineDevice`**: join two devices, register only one, attempt pair with the unregistered one — verify "device offline" error

## Files to modify

| File | Change |
|------|--------|
| `proto/hubfuse.proto` | Add `ListDevices` RPC, `ListDevicesRequest/Response`, `status` field to `DeviceInfo` |
| `internal/agent/connector.go` | Add `isTLSCertError()`, early return on cert errors in `Connect()` |
| `internal/hub/store/store.go` | Add `ListAllDevices()` to interface |
| `internal/hub/store/sqlite.go` | Implement `ListAllDevices()` |
| `internal/hub/server.go` | Add `ListDevices()` handler, update pairing error messages |
| `internal/hub/pairing.go` | Update pairing request to distinguish device states |
| `cmd/hubfuse-agent/` | Update `devices` command to use `ListDevices` RPC |
| `internal/agent/connector_test.go` | New: `TestIsTLSCertError` |
| `internal/hub/store/sqlite_test.go` | Add `TestListAllDevices` |
| `internal/hub/server_test.go` | Add `TestListDevices` |
| `internal/hub/pairing_test.go` | Add pairing error distinction tests |
| `tests/integration/` | Add ListDevices and pairing offline tests |
