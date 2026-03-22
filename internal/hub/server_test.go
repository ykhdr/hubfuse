package hub

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/hub/store"
	pb "github.com/ykhdr/hubfuse/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// startTestHub starts an in-process gRPC hub server on a random port. It
// returns the server's listen address and the CA certificate in PEM form. The
// gRPC server is stopped automatically via t.Cleanup.
func startTestHub(t *testing.T) (addr string, caCertPEM []byte) {
	t.Helper()

	s, err := store.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	caCert, caKey, err := common.GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}

	// Write CA and server certs to a temp dir so LoadTLSServerConfig can read them.
	tlsDir := t.TempDir()

	serverCertPEM, serverKeyPEM, err := common.GenerateServerCert(caCert, caKey, []string{"localhost", "127.0.0.1"})
	if err != nil {
		t.Fatalf("GenerateServerCert: %v", err)
	}

	caCertPEMBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCert.Raw})

	caCertPath := tlsDir + "/ca.crt"
	serverCertPath := tlsDir + "/server.crt"
	serverKeyPath := tlsDir + "/server.key"

	if err := os.WriteFile(caCertPath, caCertPEMBytes, 0644); err != nil {
		t.Fatalf("write CA cert: %v", err)
	}
	if err := common.SaveCertAndKey(serverCertPath, serverKeyPath, serverCertPEM, serverKeyPEM); err != nil {
		t.Fatalf("SaveCertAndKey: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	registry := NewRegistry(s, caCert, caKey, logger)

	tlsCfg, err := common.LoadTLSServerConfig(caCertPath, serverCertPath, serverKeyPath)
	if err != nil {
		t.Fatalf("LoadTLSServerConfig: %v", err)
	}

	grpcServer := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsCfg)),
		grpc.UnaryInterceptor(AuthUnaryInterceptor),
		grpc.StreamInterceptor(AuthStreamInterceptor),
	)

	srv := NewServer(registry, logger)
	pb.RegisterHubFuseServer(grpcServer, srv)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	go func() {
		_ = grpcServer.Serve(lis)
	}()

	t.Cleanup(func() { grpcServer.GracefulStop() })

	return lis.Addr().String(), caCertPEMBytes
}

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

	tlsCfg, err := tlsConfigFromPEM(certPEM, keyPEM, caCertPEM)
	if err != nil {
		t.Fatalf("tlsConfigFromPEM: %v", err)
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
	addr, caCertPEM := startTestHub(t)

	// ── 1. Join without a client certificate ────────────────────────────────
	unauthClient := dialNoClientCert(t, addr, caCertPEM)

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
	authedClient := dialWithClientCert(t, addr, joinResp.ClientCert, joinResp.ClientKey, joinResp.CaCert)

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

	stream, err := authedClient.Subscribe(subscribeCtx, &pb.SubscribeRequest{DeviceId: deviceID})
	if err != nil {
		t.Fatalf("Subscribe RPC: %v", err)
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

	authedClient2 := dialWithClientCert(t, addr, joinResp2.ClientCert, joinResp2.ClientKey, joinResp2.CaCert)

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
	addr, caCertPEM := startTestHub(t)

	unauthClient := dialNoClientCert(t, addr, caCertPEM)

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
	addr, caCertPEM := startTestHub(t)
	client := dialNoClientCert(t, addr, caCertPEM)

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

// Ensure fmt is used (it's used in parseCACertPool if we add it, but let's keep it in case).
var _ = fmt.Sprintf
