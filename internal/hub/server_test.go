package hub_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/hub/hubtest"
	pb "github.com/ykhdr/hubfuse/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// dialNoClientCert connects to the hub without presenting a client certificate.
// The client still verifies the server certificate using the given CA PEM.
func dialNoClientCert(t *testing.T, addr string, caCertPEM []byte) pb.HubFuseClient {
	t.Helper()

	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caCertPEM) {
		t.Fatal("dialNoClientCert: failed to parse CA cert PEM")
	}

	tlsCfg := &tls.Config{
		RootCAs:    caPool,
		MinVersion: tls.VersionTLS13,
	}

	conn, err := grpc.Dial(addr, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg))) //nolint:staticcheck
	if err != nil {
		t.Fatalf("grpc.Dial (no cert): %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	return pb.NewHubFuseClient(conn)
}

// dialWithClientCert connects to the hub presenting the given mTLS client cert.
func dialWithClientCert(t *testing.T, addr string, certPEM, keyPEM, caCertPEM []byte) pb.HubFuseClient {
	t.Helper()

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("dialWithClientCert: X509KeyPair: %v", err)
	}

	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caCertPEM) {
		t.Fatal("dialWithClientCert: failed to parse CA cert PEM")
	}

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caPool,
		MinVersion:   tls.VersionTLS13,
	}

	conn, err := grpc.Dial(addr, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg))) //nolint:staticcheck
	if err != nil {
		t.Fatalf("grpc.Dial (with cert): %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	return pb.NewHubFuseClient(conn)
}

// TestIntegration_JoinRegisterSubscribeHeartbeatDeregister runs a full
// device lifecycle against an in-process hub:
//  1. Join (no client cert) — receive signed cert
//  2. Reconnect with mTLS
//  3. Register — verify success and devices list
//  4. Open Subscribe stream — verify DeviceOnline event received
//  5. Heartbeat — verify success
//  6. Deregister — verify DeviceOffline event on the stream
func TestIntegration_JoinRegisterSubscribeHeartbeatDeregister(t *testing.T) {
	h := hubtest.StartTestHub(t)

	// ── 1. Join without a client certificate ────────────────────────────────
	unauthClient := dialNoClientCert(t, h.Addr, h.CAPEM)

	deviceID := "dev-" + uuid.New().String()

	joinResp, err := unauthClient.Join(context.Background(), &pb.JoinRequest{
		DeviceId: deviceID,
		Nickname: "tester",
	})
	if err != nil {
		t.Fatalf("Join RPC: %v", err)
	}
	if !joinResp.Success {
		t.Fatalf("Join failed: %s", joinResp.Error)
	}
	if len(joinResp.ClientCert) == 0 {
		t.Fatal("Join: ClientCert is empty")
	}
	if len(joinResp.ClientKey) == 0 {
		t.Fatal("Join: ClientKey is empty")
	}
	if len(joinResp.CaCert) == 0 {
		t.Fatal("Join: CaCert is empty")
	}

	// ── 2. Reconnect with mTLS using the received certificate ───────────────
	authedClient := dialWithClientCert(t, h.Addr, joinResp.ClientCert, joinResp.ClientKey, joinResp.CaCert)

	// ── 3. Register ──────────────────────────────────────────────────────────
	regResp, err := authedClient.Register(context.Background(), &pb.RegisterRequest{
		SshPort:         22,
		ProtocolVersion: int32(common.ProtocolVersion),
	})
	if err != nil {
		t.Fatalf("Register RPC: %v", err)
	}
	if !regResp.Success {
		t.Fatalf("Register failed: %s", regResp.Error)
	}

	found := false
	for _, d := range regResp.DevicesOnline {
		if d.DeviceId == deviceID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("Register: registered device %q not in DevicesOnline list", deviceID)
	}

	// ── 4. Open Subscribe stream + verify DeviceOnline ───────────────────────
	// Subscribe as device1 and register a second device — device1's stream
	// should receive the DeviceOnline event for device2.
	subscribeCtx, cancelSubscribe := context.WithCancel(context.Background())
	t.Cleanup(cancelSubscribe)

	stream, err := authedClient.Subscribe(subscribeCtx, &pb.SubscribeRequest{})
	if err != nil {
		t.Fatalf("Subscribe RPC: %v", err)
	}
	// Consume SubscribeReady sentinel before expecting real events.
	if ev, err := stream.Recv(); err != nil {
		t.Fatalf("Subscribe ready: %v", err)
	} else if ev.GetSubscribeReady() == nil {
		t.Fatalf("expected SubscribeReady, got %T", ev.GetPayload())
	}

	// Join + register device2.
	device2ID := "dev-" + uuid.New().String()
	joinResp2, err := unauthClient.Join(context.Background(), &pb.JoinRequest{
		DeviceId: device2ID,
		Nickname: "tester2",
	})
	if err != nil || !joinResp2.Success {
		t.Fatalf("Join2: err=%v resp=%+v", err, joinResp2)
	}

	authedClient2 := dialWithClientCert(t, h.Addr, joinResp2.ClientCert, joinResp2.ClientKey, joinResp2.CaCert)

	regResp2, err := authedClient2.Register(context.Background(), &pb.RegisterRequest{
		SshPort:         22,
		ProtocolVersion: int32(common.ProtocolVersion),
	})
	if err != nil || !regResp2.Success {
		t.Fatalf("Register2: err=%v resp=%+v", err, regResp2)
	}

	// Receive DeviceOnline event for device2 on device1's stream.
	eventCh := make(chan *pb.Event, 1)
	go func() {
		ev, err := stream.Recv()
		if err != nil {
			return
		}
		eventCh <- ev
	}()

	select {
	case ev := <-eventCh:
		if ev.GetDeviceOnline() == nil {
			t.Errorf("expected DeviceOnline event, got %T", ev.GetPayload())
		} else if ev.GetDeviceOnline().DeviceId != device2ID {
			t.Errorf("DeviceOnline.DeviceId = %q, want %q", ev.GetDeviceOnline().DeviceId, device2ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for DeviceOnline event on Subscribe stream")
	}

	// ── 5. Heartbeat ─────────────────────────────────────────────────────────
	hbResp, err := authedClient.Heartbeat(context.Background(), &pb.HeartbeatRequest{})
	if err != nil {
		t.Fatalf("Heartbeat RPC: %v", err)
	}
	if !hbResp.Success {
		t.Error("Heartbeat: success = false")
	}

	// ── 6. Deregister device2 → DeviceOffline on device1's stream ────────────
	offlineCh := make(chan *pb.Event, 1)
	go func() {
		ev, err := stream.Recv()
		if err != nil {
			return
		}
		offlineCh <- ev
	}()

	deregResp, err := authedClient2.Deregister(context.Background(), &pb.DeregisterRequest{})
	if err != nil {
		t.Fatalf("Deregister RPC: %v", err)
	}
	if !deregResp.Success {
		t.Error("Deregister: success = false")
	}

	select {
	case ev := <-offlineCh:
		if ev.GetDeviceOffline() == nil {
			t.Errorf("expected DeviceOffline event, got %T", ev.GetPayload())
		} else if ev.GetDeviceOffline().DeviceId != device2ID {
			t.Errorf("DeviceOffline.DeviceId = %q, want %q", ev.GetDeviceOffline().DeviceId, device2ID)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for DeviceOffline event on Subscribe stream")
	}
}

// TestIntegration_AuthenticatedRPCBlockedWithoutCert verifies that
// authenticated RPCs are rejected when no client certificate is presented.
func TestIntegration_AuthenticatedRPCBlockedWithoutCert(t *testing.T) {
	h := hubtest.StartTestHub(t)

	unauthClient := dialNoClientCert(t, h.Addr, h.CAPEM)

	_, err := unauthClient.Register(context.Background(), &pb.RegisterRequest{
		SshPort:         22,
		ProtocolVersion: int32(common.ProtocolVersion),
	})
	if err == nil {
		t.Fatal("expected error calling Register without client cert, got nil")
	}
}

// TestIntegration_JoinNicknameConflict verifies that a duplicate nickname
// results in success=false without a transport error.
func TestIntegration_JoinNicknameConflict(t *testing.T) {
	h := hubtest.StartTestHub(t)
	client := dialNoClientCert(t, h.Addr, h.CAPEM)

	ctx := context.Background()

	resp1, err := client.Join(ctx, &pb.JoinRequest{DeviceId: "d1", Nickname: "clash"})
	if err != nil || !resp1.Success {
		t.Fatalf("first Join: err=%v success=%v", err, resp1.GetSuccess())
	}

	resp2, err := client.Join(ctx, &pb.JoinRequest{DeviceId: "d2", Nickname: "clash"})
	if err != nil {
		t.Fatalf("second Join RPC transport error: %v", err)
	}
	if resp2.Success {
		t.Error("expected second Join to fail for duplicate nickname, got success=true")
	}
	if resp2.Error == "" {
		t.Error("expected non-empty error message for duplicate nickname")
	}
}

func TestRequestPairing_DeviceNotFound(t *testing.T) {
	h := hubtest.StartTestHub(t)
	unauthClient := dialNoClientCert(t, h.Addr, h.CAPEM)

	joinResp, err := unauthClient.Join(context.Background(), &pb.JoinRequest{
		DeviceId: "dev-pair-from",
		Nickname: "pair-from",
	})
	if err != nil || !joinResp.Success {
		t.Fatalf("Join: err=%v success=%v", err, joinResp.GetSuccess())
	}

	client := dialWithClientCert(t, h.Addr, joinResp.ClientCert, joinResp.ClientKey, h.CAPEM)
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
	h := hubtest.StartTestHub(t)
	unauthClient := dialNoClientCert(t, h.Addr, h.CAPEM)

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
	client1 := dialWithClientCert(t, h.Addr, joinResp1.ClientCert, joinResp1.ClientKey, h.CAPEM)
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
	if !strings.Contains(st.Message(), "device offline") {
		t.Errorf("expected 'device offline' in message, got %q", st.Message())
	}
}

func TestListDevices(t *testing.T) {
	h := hubtest.StartTestHub(t)

	// Join two devices.
	unauthClient := dialNoClientCert(t, h.Addr, h.CAPEM)

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
	client1 := dialWithClientCert(t, h.Addr, joinResp1.ClientCert, joinResp1.ClientKey, h.CAPEM)
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
	if statusMap["list-bob"] != "registered" {
		t.Errorf("bob status = %q, want %q", statusMap["list-bob"], "registered")
	}
}
