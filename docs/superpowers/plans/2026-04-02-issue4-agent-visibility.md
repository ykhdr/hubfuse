# Issue #4: Agent Visibility After Hub Reset — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fix agent invisibility after hub CA reset by detecting TLS cert errors, add ListDevices RPC showing all devices with status, and distinguish device states in pairing errors.

**Architecture:** Three independent changes: (1) agent-side TLS error classification in connector, (2) new ListDevices gRPC endpoint + store method + CLI, (3) pairing error differentiation in hub registry. All share a proto update.

**Tech Stack:** Go 1.25, gRPC/protobuf, SQLite, Cobra CLI

---

### Task 1: Proto — add ListDevices RPC and status field to DeviceInfo

**Files:**
- Modify: `proto/hubfuse.proto`

- [ ] **Step 1: Add status field to DeviceInfo and ListDevices RPC**

In `proto/hubfuse.proto`, add `status` field to `DeviceInfo` (after field 5) and new messages + RPC:

```protobuf
message DeviceInfo {
  string device_id = 1;
  string nickname = 2;
  string ip = 3;
  int32 ssh_port = 4;
  repeated Share shares = 5;
  string status = 6;
}
```

Add after `ConfirmPairing` RPC in the service block:

```protobuf
  rpc ListDevices(ListDevicesRequest) returns (ListDevicesResponse);
```

Add at the end of the file:

```protobuf
// ─── ListDevices ────────────────────────────────────────────────────────────

message ListDevicesRequest {}

message ListDevicesResponse {
  repeated DeviceInfo devices = 1;
}
```

- [ ] **Step 2: Regenerate gRPC code**

Run: `make proto-gen`

- [ ] **Step 3: Verify build**

Run: `make build`
Expected: build succeeds (server won't implement ListDevices yet — `UnimplementedHubFuseServer` provides a default)

- [ ] **Step 4: Commit**

```bash
git add proto/
git commit -m "proto: add ListDevices RPC and status field to DeviceInfo"
```

---

### Task 2: Store — add ListAllDevices method

**Files:**
- Modify: `internal/hub/store/store.go`
- Modify: `internal/hub/store/sqlite.go`
- Modify: `internal/hub/store/sqlite_test.go`

- [ ] **Step 1: Write the failing test**

In `internal/hub/store/sqlite_test.go`, add:

```go
func TestListAllDevices(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	d1 := makeDevice("dev-1", "alice")
	d2 := makeDevice("dev-2", "bob")
	if err := s.CreateDevice(ctx, d1); err != nil {
		t.Fatalf("CreateDevice d1: %v", err)
	}
	if err := s.CreateDevice(ctx, d2); err != nil {
		t.Fatalf("CreateDevice d2: %v", err)
	}
	// Mark d1 online.
	if err := s.UpdateDeviceStatus(ctx, "dev-1", "online", "10.0.0.1", 2222); err != nil {
		t.Fatalf("UpdateDeviceStatus: %v", err)
	}

	devices, err := s.ListAllDevices(ctx)
	if err != nil {
		t.Fatalf("ListAllDevices: %v", err)
	}
	if len(devices) != 2 {
		t.Fatalf("expected 2 devices, got %d", len(devices))
	}

	statusMap := map[string]string{}
	for _, d := range devices {
		statusMap[d.DeviceID] = d.Status
	}
	if statusMap["dev-1"] != "online" {
		t.Errorf("dev-1 status = %q, want %q", statusMap["dev-1"], "online")
	}
	if statusMap["dev-2"] != "offline" {
		t.Errorf("dev-2 status = %q, want %q", statusMap["dev-2"], "offline")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/hub/store/ -run TestListAllDevices -v`
Expected: FAIL — `ListAllDevices` not defined

- [ ] **Step 3: Add ListAllDevices to Store interface**

In `internal/hub/store/store.go`, add after `ListOnlineDevices` (line 25):

```go
	// ListAllDevices returns all devices regardless of status.
	ListAllDevices(ctx context.Context) ([]*Device, error)
```

- [ ] **Step 4: Implement in sqlite.go**

In `internal/hub/store/sqlite.go`, add after `ListOnlineDevices` method (after line 137):

```go
// ListAllDevices returns all devices regardless of status.
func (s *sqliteStore) ListAllDevices(ctx context.Context) ([]*Device, error) {
	const q = `
		SELECT device_id, nickname, last_ip, ssh_port, status, last_heartbeat
		FROM devices`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list all devices: %w", err)
	}
	defer rows.Close()
	return scanDevices(rows)
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `go test ./internal/hub/store/ -run TestListAllDevices -v`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/hub/store/
git commit -m "store: add ListAllDevices method"
```

---

### Task 3: Hub server — implement ListDevices RPC handler

**Files:**
- Modify: `internal/hub/server.go`
- Modify: `internal/hub/server_test.go`

- [ ] **Step 1: Write the failing test**

In `internal/hub/server_test.go`, add:

```go
func TestListDevices(t *testing.T) {
	addr, caCertPEM := startTestHub(t)

	// Join two devices.
	unauthClient := dialNoClientCert(t, addr, caCertPEM)

	joinResp1, err := unauthClient.Join(context.Background(), &pb.JoinRequest{
		DeviceId: "dev-list-1",
		Nickname: "list-alice",
	})
	if err != nil || !joinResp1.Success {
		t.Fatalf("Join dev1: err=%v success=%v", err, joinResp1.GetSuccess())
	}

	joinResp2, err := unauthClient.Join(context.Background(), &pb.JoinRequest{
		DeviceId: "dev-list-2",
		Nickname: "list-bob",
	})
	if err != nil || !joinResp2.Success {
		t.Fatalf("Join dev2: err=%v success=%v", err, joinResp2.GetSuccess())
	}

	// Register only device 1 (device 2 stays offline).
	client1 := dialWithClientCert(t, addr, caCertPEM, joinResp1.ClientCert, joinResp1.ClientKey)
	_, err = client1.Register(context.Background(), &pb.RegisterRequest{
		SshPort:         2222,
		ProtocolVersion: int32(common.ProtocolVersion),
	})
	if err != nil {
		t.Fatalf("Register dev1: %v", err)
	}

	// Call ListDevices as device 1.
	resp, err := client1.ListDevices(context.Background(), &pb.ListDevicesRequest{})
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}

	if len(resp.Devices) != 2 {
		t.Fatalf("expected 2 devices, got %d", len(resp.Devices))
	}

	statusMap := map[string]string{}
	for _, d := range resp.Devices {
		statusMap[d.Nickname] = d.Status
	}
	if statusMap["list-alice"] != "online" {
		t.Errorf("alice status = %q, want %q", statusMap["list-alice"], "online")
	}
	if statusMap["list-bob"] != "offline" {
		t.Errorf("bob status = %q, want %q", statusMap["list-bob"], "offline")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/hub/ -run TestListDevices -v`
Expected: FAIL — `ListDevices` returns Unimplemented

- [ ] **Step 3: Implement ListDevices handler**

In `internal/hub/server.go`, add after the `Subscribe` method:

```go
// ListDevices returns all devices known to the hub, regardless of status.
func (s *Server) ListDevices(ctx context.Context, req *pb.ListDevicesRequest) (*pb.ListDevicesResponse, error) {
	if _, err := common.ExtractDeviceID(ctx); err != nil {
		return nil, err
	}

	all, err := s.registry.store.ListAllDevices(ctx)
	if err != nil {
		return nil, fmt.Errorf("list all devices: %w", err)
	}

	devices := make([]*pb.DeviceInfo, 0, len(all))
	for _, d := range all {
		shares, err := s.registry.store.GetShares(ctx, d.DeviceID)
		if err != nil {
			s.logger.Warn("ListDevices: get shares",
				slog.String("device_id", d.DeviceID),
				slog.Any("error", err))
			continue
		}

		pbShares := make([]*pb.Share, 0, len(shares))
		for _, sh := range shares {
			pbShares = append(pbShares, &pb.Share{
				Alias:          sh.Alias,
				Permissions:    sh.Permissions,
				AllowedDevices: sh.AllowedDevices,
			})
		}

		devices = append(devices, &pb.DeviceInfo{
			DeviceId: d.DeviceID,
			Nickname: d.Nickname,
			Ip:       d.LastIP,
			SshPort:  int32(d.SSHPort),
			Shares:   pbShares,
			Status:   d.Status,
		})
	}

	return &pb.ListDevicesResponse{Devices: devices}, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/hub/ -run TestListDevices -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/hub/server.go internal/hub/server_test.go
git commit -m "hub: implement ListDevices RPC handler"
```

---

### Task 4: Agent client — add ListDevices wrapper

**Files:**
- Modify: `internal/agent/client.go`

- [ ] **Step 1: Add ListDevices method to HubClient**

In `internal/agent/client.go`, add after the `RequestPairing` method:

```go
// ListDevices retrieves all devices known to the hub.
func (c *HubClient) ListDevices(ctx context.Context) (*pb.ListDevicesResponse, error) {
	resp, err := c.client.ListDevices(ctx, &pb.ListDevicesRequest{})
	if err != nil {
		return nil, fmt.Errorf("ListDevices RPC: %w", err)
	}
	return resp, nil
}
```

- [ ] **Step 2: Verify build**

Run: `make build`
Expected: success

- [ ] **Step 3: Commit**

```bash
git add internal/agent/client.go
git commit -m "agent: add ListDevices client method"
```

---

### Task 5: Agent CLI — update `devices` command to use ListDevices

**Files:**
- Modify: `cmd/hubfuse-agent/main.go`

- [ ] **Step 1: Rewrite devicesCmd**

Replace the `devicesCmd` function (lines 301-333 in `cmd/hubfuse-agent/main.go`) with:

```go
// devicesCmd implements: hubfuse-agent devices
func devicesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "devices",
		Short: "List all devices",
		RunE: func(cmd *cobra.Command, args []string) error {
			dataDir := expandHome(defaultDataDir)
			logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

			hubClient, _, err := dialHub(dataDir, logger)
			if err != nil {
				return fmt.Errorf("connect to hub: %w", err)
			}
			defer hubClient.Close()

			resp, err := hubClient.ListDevices(context.Background())
			if err != nil {
				return fmt.Errorf("list devices: %w", err)
			}

			if len(resp.Devices) == 0 {
				fmt.Println("no devices registered")
				return nil
			}

			fmt.Printf("%-40s  %-20s  %-8s  %s\n", "DEVICE ID", "NICKNAME", "STATUS", "IP")
			fmt.Printf("%-40s  %-20s  %-8s  %s\n",
				strings.Repeat("-", 40), strings.Repeat("-", 20),
				strings.Repeat("-", 8), strings.Repeat("-", 15))
			for _, d := range resp.Devices {
				fmt.Printf("%-40s  %-20s  %-8s  %s\n", d.DeviceId, d.Nickname, d.Status, d.Ip)
			}
			return nil
		},
	}
}
```

- [ ] **Step 2: Verify build**

Run: `make build`
Expected: success

- [ ] **Step 3: Commit**

```bash
git add cmd/hubfuse-agent/main.go
git commit -m "cli: update devices command to show all devices with status"
```

---

### Task 6: Pairing — distinguish device states in error messages

**Files:**
- Modify: `internal/common/errors.go`
- Modify: `internal/hub/pairing.go`
- Modify: `internal/hub/server_test.go`

- [ ] **Step 1: Write failing tests**

In `internal/hub/server_test.go`, add:

```go
func TestRequestPairing_DeviceNotFound(t *testing.T) {
	addr, caCertPEM := startTestHub(t)
	unauthClient := dialNoClientCert(t, addr, caCertPEM)

	joinResp, err := unauthClient.Join(context.Background(), &pb.JoinRequest{
		DeviceId: "dev-pair-from",
		Nickname: "pair-from",
	})
	if err != nil || !joinResp.Success {
		t.Fatalf("Join: err=%v success=%v", err, joinResp.GetSuccess())
	}

	client := dialWithClientCert(t, addr, caCertPEM, joinResp.ClientCert, joinResp.ClientKey)
	_, err = client.Register(context.Background(), &pb.RegisterRequest{
		SshPort:         2222,
		ProtocolVersion: int32(common.ProtocolVersion),
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Pair with non-existent device.
	_, err = client.RequestPairing(context.Background(), &pb.RequestPairingRequest{
		ToDevice:  "nonexistent",
		PublicKey: "ssh-ed25519 AAAA...",
	})
	if err == nil {
		t.Fatal("expected error for non-existent device")
	}
	st := status.Convert(err)
	if st.Code() != codes.NotFound {
		t.Errorf("expected NotFound, got %v", st.Code())
	}
	if !strings.Contains(st.Message(), "no device with nickname") {
		t.Errorf("expected 'no device with nickname' in message, got %q", st.Message())
	}
}

func TestRequestPairing_DeviceOffline(t *testing.T) {
	addr, caCertPEM := startTestHub(t)
	unauthClient := dialNoClientCert(t, addr, caCertPEM)

	// Join two devices but only register one.
	joinResp1, err := unauthClient.Join(context.Background(), &pb.JoinRequest{
		DeviceId: "dev-pair-1",
		Nickname: "pair-alice",
	})
	if err != nil || !joinResp1.Success {
		t.Fatalf("Join dev1: err=%v", err)
	}

	joinResp2, err := unauthClient.Join(context.Background(), &pb.JoinRequest{
		DeviceId: "dev-pair-2",
		Nickname: "pair-bob",
	})
	if err != nil || !joinResp2.Success {
		t.Fatalf("Join dev2: err=%v", err)
	}

	// Register only device 1.
	client1 := dialWithClientCert(t, addr, caCertPEM, joinResp1.ClientCert, joinResp1.ClientKey)
	_, err = client1.Register(context.Background(), &pb.RegisterRequest{
		SshPort:         2222,
		ProtocolVersion: int32(common.ProtocolVersion),
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Pair with offline device 2.
	_, err = client1.RequestPairing(context.Background(), &pb.RequestPairingRequest{
		ToDevice:  "pair-bob",
		PublicKey: "ssh-ed25519 AAAA...",
	})
	if err == nil {
		t.Fatal("expected error for offline device")
	}
	st := status.Convert(err)
	if st.Code() != codes.Unavailable {
		t.Errorf("expected Unavailable, got %v", st.Code())
	}
	if !strings.Contains(st.Message(), "not currently connected") {
		t.Errorf("expected 'not currently connected' in message, got %q", st.Message())
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/hub/ -run "TestRequestPairing_Device" -v`
Expected: FAIL — current code returns generic `ErrDeviceNotFound` for both cases

- [ ] **Step 3: Add new error sentinels**

In `internal/common/errors.go`, add:

```go
	ErrDeviceOffline = status.Error(codes.Unavailable, "device offline")
```

- [ ] **Step 4: Update pairing logic**

In `internal/hub/pairing.go`, replace lines 27-40 of `RequestPairing` with:

```go
func (r *Registry) RequestPairing(ctx context.Context, fromDevice, toDevice, publicKey string) (string, error) {
	from, err := r.store.GetDevice(ctx, fromDevice)
	if err != nil {
		return "", common.ErrDeviceNotFound
	}

	to, err := r.store.GetDeviceByNickname(ctx, toDevice)
	if err != nil {
		return "", status.Errorf(codes.NotFound, "no device with nickname %q", toDevice)
	}

	if from.Status != "online" {
		return "", common.ErrDeviceNotFound
	}
	if to.Status != "online" {
		return "", status.Errorf(codes.Unavailable, "%s is not currently connected", toDevice)
	}

	paired, err := r.store.IsPaired(ctx, fromDevice, to.DeviceID)
```

Note: `toDevice` is now looked up by **nickname** (via `GetDeviceByNickname`) instead of by device ID (via `GetDevice`). The rest of the function must use `to.DeviceID` instead of `toDevice` for store calls. Update the remaining references:

- Line `r.store.IsPaired(ctx, fromDevice, toDevice)` → `r.store.IsPaired(ctx, fromDevice, to.DeviceID)`
- Line `ToDevice: toDevice` in `PendingInvite` → `ToDevice: to.DeviceID`
- Line `r.sendToDevice(toDevice, event)` → `r.sendToDevice(to.DeviceID, event)`

Add to imports in `pairing.go`:

```go
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/hub/ -run "TestRequestPairing_Device" -v`
Expected: PASS

- [ ] **Step 6: Run full hub test suite**

Run: `go test ./internal/hub/ -v`
Expected: all pass

- [ ] **Step 7: Commit**

```bash
git add internal/common/errors.go internal/hub/pairing.go internal/hub/server_test.go
git commit -m "pairing: distinguish device-not-found vs device-offline errors"
```

---

### Task 7: Connector — detect TLS certificate errors and stop retrying

**Files:**
- Modify: `internal/agent/connector.go`
- Create: `internal/agent/connector_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/agent/connector_test.go`:

```go
package agent

import (
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"testing"
)

func TestIsTLSCertError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "UnknownAuthorityError",
			err:  x509.UnknownAuthorityError{},
			want: true,
		},
		{
			name: "CertificateInvalidError",
			err:  x509.CertificateInvalidError{Reason: x509.Expired},
			want: true,
		},
		{
			name: "wrapped_UnknownAuthority",
			err:  fmt.Errorf("dial: %w", x509.UnknownAuthorityError{}),
			want: true,
		},
		{
			name: "net_OpError",
			err:  &net.OpError{Op: "dial", Err: errors.New("connection refused")},
			want: false,
		},
		{
			name: "generic_error",
			err:  errors.New("something went wrong"),
			want: false,
		},
		{
			name: "nil",
			err:  nil,
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := isTLSCertError(tc.err)
			if got != tc.want {
				t.Errorf("isTLSCertError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/agent/ -run TestIsTLSCertError -v`
Expected: FAIL — `isTLSCertError` undefined

- [ ] **Step 3: Implement isTLSCertError and update Connect**

In `internal/agent/connector.go`, add to imports:

```go
	"crypto/x509"
	"errors"
```

Add the helper function:

```go
// isTLSCertError reports whether err is a TLS certificate validation error
// that will not resolve on retry (e.g. the hub CA has changed).
func isTLSCertError(err error) bool {
	if err == nil {
		return false
	}
	var unknownAuth x509.UnknownAuthorityError
	if errors.As(err, &unknownAuth) {
		return true
	}
	var certInvalid x509.CertificateInvalidError
	if errors.As(err, &certInvalid) {
		return true
	}
	return false
}
```

Update the `Connect` method — replace the warn log + sleep block (lines 47-57) with:

```go
		if isTLSCertError(err) {
			return nil, fmt.Errorf("hub certificate not trusted — the hub CA may have changed, please re-join with: hubfuse join %s", c.hubAddr)
		}

		c.logger.Warn("failed to connect to hub, retrying",
			"addr", c.hubAddr,
			"err", err,
			"backoff", delay,
		)

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("connect to hub: %w", ctx.Err())
		case <-time.After(delay):
		}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/agent/ -run TestIsTLSCertError -v`
Expected: PASS

- [ ] **Step 5: Run full agent test suite**

Run: `go test ./internal/agent/ -v`
Expected: all pass

- [ ] **Step 6: Commit**

```bash
git add internal/agent/connector.go internal/agent/connector_test.go
git commit -m "connector: detect TLS cert errors and stop retrying"
```

---

### Task 8: Integration tests

**Files:**
- Modify: `tests/integration/integration_test.go` (if new helpers needed)
- Create or modify: `tests/integration/devices_test.go`
- Modify: `tests/integration/pairing_test.go`

- [ ] **Step 1: Write ListDevices integration test**

Create `tests/integration/devices_test.go`:

```go
package integration

import (
	"context"
	"testing"

	"github.com/ykhdr/hubfuse/internal/common"
	pb "github.com/ykhdr/hubfuse/proto"
)

func TestListDevices_AllStatuses(t *testing.T) {
	h := startTestHub(t)

	unauthClient := dialNoClientCert(t, h.addr, h.caCertPEM)

	// Join two devices.
	join1, err := unauthClient.Join(context.Background(), &pb.JoinRequest{
		DeviceId: "dev-ld-1",
		Nickname: "ld-alice",
	})
	if err != nil || !join1.Success {
		t.Fatalf("Join dev1: err=%v", err)
	}

	join2, err := unauthClient.Join(context.Background(), &pb.JoinRequest{
		DeviceId: "dev-ld-2",
		Nickname: "ld-bob",
	})
	if err != nil || !join2.Success {
		t.Fatalf("Join dev2: err=%v", err)
	}

	// Register only device 1.
	client1 := dialWithClientCert(t, h.addr, h.caCertPEM, join1.ClientCert, join1.ClientKey)
	_, err = client1.Register(context.Background(), &pb.RegisterRequest{
		SshPort:         2222,
		ProtocolVersion: int32(common.ProtocolVersion),
	})
	if err != nil {
		t.Fatalf("Register dev1: %v", err)
	}

	// ListDevices should return both.
	resp, err := client1.ListDevices(context.Background(), &pb.ListDevicesRequest{})
	if err != nil {
		t.Fatalf("ListDevices: %v", err)
	}

	if len(resp.Devices) != 2 {
		t.Fatalf("expected 2 devices, got %d", len(resp.Devices))
	}

	statusMap := map[string]string{}
	for _, d := range resp.Devices {
		statusMap[d.Nickname] = d.Status
	}
	if statusMap["ld-alice"] != "online" {
		t.Errorf("alice status = %q, want online", statusMap["ld-alice"])
	}
	if statusMap["ld-bob"] != "offline" {
		t.Errorf("bob status = %q, want offline", statusMap["ld-bob"])
	}
}
```

- [ ] **Step 2: Write pairing offline integration test**

In `tests/integration/pairing_test.go`, add:

```go
func TestPairing_OfflineDevice(t *testing.T) {
	h := startTestHub(t)

	unauthClient := dialNoClientCert(t, h.addr, h.caCertPEM)

	// Join two devices.
	join1, err := unauthClient.Join(context.Background(), &pb.JoinRequest{
		DeviceId: "dev-po-1",
		Nickname: "po-alice",
	})
	if err != nil || !join1.Success {
		t.Fatalf("Join dev1: err=%v", err)
	}

	join2, err := unauthClient.Join(context.Background(), &pb.JoinRequest{
		DeviceId: "dev-po-2",
		Nickname: "po-bob",
	})
	if err != nil || !join2.Success {
		t.Fatalf("Join dev2: err=%v", err)
	}

	// Register only device 1.
	client1 := dialWithClientCert(t, h.addr, h.caCertPEM, join1.ClientCert, join1.ClientKey)
	_, err = client1.Register(context.Background(), &pb.RegisterRequest{
		SshPort:         2222,
		ProtocolVersion: int32(common.ProtocolVersion),
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Pair with offline device.
	_, err = client1.RequestPairing(context.Background(), &pb.RequestPairingRequest{
		ToDevice:  "po-bob",
		PublicKey: "ssh-ed25519 AAAA...",
	})
	if err == nil {
		t.Fatal("expected error pairing with offline device")
	}

	st := status.Convert(err)
	if st.Code() != codes.Unavailable {
		t.Errorf("expected Unavailable, got %v", st.Code())
	}
}
```

Add imports `"google.golang.org/grpc/codes"` and `"google.golang.org/grpc/status"` to the file if not present.

- [ ] **Step 3: Run integration tests**

Run: `make test-integration`
Expected: all pass

- [ ] **Step 4: Commit**

```bash
git add tests/integration/
git commit -m "integration: add ListDevices and pairing-offline tests"
```

---

### Task 9: Final verification

- [ ] **Step 1: Run full test suite**

Run: `make test`
Expected: all pass

- [ ] **Step 2: Run vet**

Run: `make vet`
Expected: clean

- [ ] **Step 3: Run build**

Run: `make build`
Expected: success
