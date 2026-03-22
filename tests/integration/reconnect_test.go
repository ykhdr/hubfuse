package integration

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/hub"
	"github.com/ykhdr/hubfuse/internal/hub/store"
	pb "github.com/ykhdr/hubfuse/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// startPersistentHub starts an in-process hub backed by on-disk TLS files and
// a SQLite database stored under dataDir. It returns the hubHandle and a stop
// function. Calling stop shuts the server down but leaves the data directory
// intact so the hub can be restarted with the same CA and DB.
func startPersistentHub(t *testing.T, dataDir string) (hubHandle, func()) {
	t.Helper()

	dbPath := filepath.Join(dataDir, "hubfuse.db")
	s, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("startPersistentHub: NewSQLiteStore: %v", err)
	}

	tlsDir := filepath.Join(dataDir, "tls")
	if err := os.MkdirAll(tlsDir, 0700); err != nil {
		t.Fatalf("startPersistentHub: mkdir tls: %v", err)
	}

	caCertPath := filepath.Join(tlsDir, "ca.crt")
	caKeyPath := filepath.Join(tlsDir, "ca.key")
	serverCertPath := filepath.Join(tlsDir, "server.crt")
	serverKeyPath := filepath.Join(tlsDir, "server.key")

	var caCertPEMBytes []byte

	// If the CA already exists on disk, load it; otherwise generate it.
	if _, err := os.Stat(caCertPath); os.IsNotExist(err) {
		caCert, caKey, genErr := common.GenerateCA()
		if genErr != nil {
			t.Fatalf("startPersistentHub: GenerateCA: %v", genErr)
		}

		caCertPEMBytes = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCert.Raw})
		caKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(caKey)})

		if err := os.WriteFile(caCertPath, caCertPEMBytes, 0644); err != nil {
			t.Fatalf("startPersistentHub: write CA cert: %v", err)
		}
		if err := os.WriteFile(caKeyPath, caKeyPEM, 0600); err != nil {
			t.Fatalf("startPersistentHub: write CA key: %v", err)
		}

		serverCertPEM, serverKeyPEM, err := common.GenerateServerCert(caCert, caKey, []string{"localhost", "127.0.0.1"})
		if err != nil {
			t.Fatalf("startPersistentHub: GenerateServerCert: %v", err)
		}
		if err := common.SaveCertAndKey(serverCertPath, serverKeyPath, serverCertPEM, serverKeyPEM); err != nil {
			t.Fatalf("startPersistentHub: SaveCertAndKey: %v", err)
		}
	} else {
		caCertPEMBytes, err = os.ReadFile(caCertPath)
		if err != nil {
			t.Fatalf("startPersistentHub: read CA cert: %v", err)
		}
	}

	// Parse the CA cert and key from disk to pass to NewRegistry.
	caCertDER, err := common.LoadPEM(caCertPath)
	if err != nil {
		t.Fatalf("startPersistentHub: LoadPEM CA cert: %v", err)
	}
	caCert, err := x509.ParseCertificate(caCertDER)
	if err != nil {
		t.Fatalf("startPersistentHub: parse CA cert: %v", err)
	}

	caKeyDER, err := common.LoadPEM(caKeyPath)
	if err != nil {
		t.Fatalf("startPersistentHub: LoadPEM CA key: %v", err)
	}
	caKey, err := x509.ParsePKCS1PrivateKey(caKeyDER)
	if err != nil {
		t.Fatalf("startPersistentHub: parse CA key: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	registry := hub.NewRegistry(s, caCert, caKey, logger)

	tlsCfg, err := common.LoadTLSServerConfig(caCertPath, serverCertPath, serverKeyPath)
	if err != nil {
		t.Fatalf("startPersistentHub: LoadTLSServerConfig: %v", err)
	}

	grpcServer := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsCfg)),
		grpc.UnaryInterceptor(hub.AuthUnaryInterceptor),
		grpc.StreamInterceptor(hub.AuthStreamInterceptor),
	)

	srvObj := hub.NewServer(registry, logger)
	pb.RegisterHubFuseServer(grpcServer, srvObj)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("startPersistentHub: listen: %v", err)
	}

	go func() {
		_ = grpcServer.Serve(lis)
	}()

	stopFn := func() {
		grpcServer.GracefulStop()
		s.Close()
	}

	h := hubHandle{
		addr:      lis.Addr().String(),
		caCertPEM: caCertPEMBytes,
	}

	return h, stopFn
}

// TestIntegration_Reconnect_AgentSurvivesHubRestart verifies that after a hub
// restart (same data directory → same CA and DB):
//  1. An agent that joined and registered before the restart can reconnect
//     using its existing certificate.
//  2. Pairings created before the restart still exist in the database.
func TestIntegration_Reconnect_AgentSurvivesHubRestart(t *testing.T) {
	dataDir := t.TempDir()

	// ── First hub instance ───────────────────────────────────────────────────
	h1, stop1 := startPersistentHub(t, dataDir)

	deviceID := "reconnect-dev-" + uuid.New().String()

	unauthClient1 := dialNoClientCert(t, h1)
	joinResp, err := unauthClient1.Join(context.Background(), &pb.JoinRequest{
		DeviceId: deviceID,
		Nickname: "reconnect-alice-" + uuid.New().String(),
	})
	if err != nil || !joinResp.Success {
		t.Fatalf("Join before restart: err=%v success=%v", err, joinResp.GetSuccess())
	}

	// Hold onto the cert returned by Join.
	clientCertPEM := joinResp.ClientCert
	clientKeyPEM := joinResp.ClientKey

	authedClient1 := dialWithClientCert(t, h1, clientCertPEM, clientKeyPEM)

	regResp1, err := authedClient1.Register(context.Background(), &pb.RegisterRequest{
		SshPort:         22,
		ProtocolVersion: int32(common.ProtocolVersion),
	})
	if err != nil || !regResp1.Success {
		t.Fatalf("Register before restart: err=%v success=%v", err, regResp1.GetSuccess())
	}

	// Create a second device and establish a pairing — we want to verify the
	// pairing survives the restart.
	devB := "reconnect-b-" + uuid.New().String()
	joinRespB, err := unauthClient1.Join(context.Background(), &pb.JoinRequest{
		DeviceId: devB,
		Nickname: "reconnect-bob-" + uuid.New().String(),
	})
	if err != nil || !joinRespB.Success {
		t.Fatalf("Join B before restart: err=%v success=%v", err, joinRespB.GetSuccess())
	}
	clientB1 := dialWithClientCert(t, h1, joinRespB.ClientCert, joinRespB.ClientKey)
	regRespB, err := clientB1.Register(context.Background(), &pb.RegisterRequest{
		SshPort:         22,
		ProtocolVersion: int32(common.ProtocolVersion),
	})
	if err != nil || !regRespB.Success {
		t.Fatalf("Register B before restart: err=%v", err)
	}

	// A requests pairing with B.
	pairResp, err := authedClient1.RequestPairing(context.Background(), &pb.RequestPairingRequest{
		ToDevice:  devB,
		PublicKey: "pk-alice",
	})
	if err != nil {
		t.Fatalf("RequestPairing before restart: %v", err)
	}
	// B confirms pairing.
	confirmResp, err := clientB1.ConfirmPairing(context.Background(), &pb.ConfirmPairingRequest{
		DeviceId:  devB,
		InviteCode: pairResp.InviteCode,
		PublicKey: "pk-bob",
	})
	if err != nil || !confirmResp.Success {
		t.Fatalf("ConfirmPairing before restart: err=%v success=%v", err, confirmResp.GetSuccess())
	}

	// ── Stop hub ─────────────────────────────────────────────────────────────
	stop1()

	// ── Second hub instance (same data dir) ──────────────────────────────────
	h2, stop2 := startPersistentHub(t, dataDir)
	t.Cleanup(stop2)

	// The agent reconnects using the same cert.
	authedClient2 := dialWithClientCert(t, h2, clientCertPEM, clientKeyPEM)

	regResp2, err := authedClient2.Register(context.Background(), &pb.RegisterRequest{
		SshPort:         22,
		ProtocolVersion: int32(common.ProtocolVersion),
	})
	if err != nil {
		t.Fatalf("Register after restart: %v", err)
	}
	if !regResp2.Success {
		t.Fatalf("Register after restart failed: %s", regResp2.Error)
	}

	// The device should appear in the online list.
	found := false
	for _, d := range regResp2.DevicesOnline {
		if d.DeviceId == deviceID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("device %q not in DevicesOnline after restart", deviceID)
	}

	// Verify the pairing still exists by attempting another pairing request
	// (which should fail with ErrPairingAlreadyExists — meaning the record is there).
	// We need to re-register devB to test this.
	clientB2 := dialWithClientCert(t, h2, joinRespB.ClientCert, joinRespB.ClientKey)
	regRespB2, err := clientB2.Register(context.Background(), &pb.RegisterRequest{
		SshPort:         22,
		ProtocolVersion: int32(common.ProtocolVersion),
	})
	if err != nil || !regRespB2.Success {
		t.Fatalf("Register B after restart: err=%v", err)
	}

	// Attempting RequestPairing again must fail because they are already paired.
	_, pairErr := authedClient2.RequestPairing(context.Background(), &pb.RequestPairingRequest{
		ToDevice:  devB,
		PublicKey: "pk-alice",
	})
	if pairErr == nil {
		t.Error("expected RequestPairing to fail with ErrPairingAlreadyExists after restart, got nil")
	}
}
