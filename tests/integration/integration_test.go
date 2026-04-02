package integration

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"net"
	"os"
	"testing"

	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/hub"
	"github.com/ykhdr/hubfuse/internal/hub/store"
	pb "github.com/ykhdr/hubfuse/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// hubHandle groups everything needed to interact with an in-process hub.
type hubHandle struct {
	addr      string
	caCertPEM []byte
}

// startTestHub starts an in-process gRPC hub server on a random port. It
// returns a hubHandle containing the listen address and the CA certificate in
// PEM form. The gRPC server is stopped automatically via t.Cleanup.
func startTestHub(t *testing.T) hubHandle {
	t.Helper()

	s, err := store.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("startTestHub: NewSQLiteStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	caCert, caKey, err := common.GenerateCA()
	if err != nil {
		t.Fatalf("startTestHub: GenerateCA: %v", err)
	}

	tlsDir := t.TempDir()

	serverCertPEM, serverKeyPEM, err := common.GenerateServerCert(caCert, caKey, common.LocalHosts())
	if err != nil {
		t.Fatalf("startTestHub: GenerateServerCert: %v", err)
	}

	caCertPEMBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCert.Raw})

	caCertPath := tlsDir + "/ca.crt"
	serverCertPath := tlsDir + "/server.crt"
	serverKeyPath := tlsDir + "/server.key"

	if err := os.WriteFile(caCertPath, caCertPEMBytes, 0644); err != nil {
		t.Fatalf("startTestHub: write CA cert: %v", err)
	}
	if err := common.SaveCertAndKey(serverCertPath, serverKeyPath, serverCertPEM, serverKeyPEM); err != nil {
		t.Fatalf("startTestHub: SaveCertAndKey: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	registry := hub.NewRegistry(s, caCert, caKey, logger)

	tlsCfg, err := common.LoadTLSServerConfig(caCertPath, serverCertPath, serverKeyPath)
	if err != nil {
		t.Fatalf("startTestHub: LoadTLSServerConfig: %v", err)
	}

	grpcServer := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsCfg)),
		grpc.UnaryInterceptor(hub.AuthUnaryInterceptor),
		grpc.StreamInterceptor(hub.AuthStreamInterceptor),
	)

	srv := hub.NewServer(registry, logger)
	pb.RegisterHubFuseServer(grpcServer, srv)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("startTestHub: listen: %v", err)
	}

	go func() {
		_ = grpcServer.Serve(lis)
	}()

	t.Cleanup(func() { grpcServer.GracefulStop() })

	return hubHandle{
		addr:      lis.Addr().String(),
		caCertPEM: caCertPEMBytes,
	}
}

// dialNoClientCert connects to the hub without presenting a client certificate.
// The client still verifies the server certificate using the given CA PEM.
func dialNoClientCert(t *testing.T, h hubHandle) pb.HubFuseClient {
	t.Helper()

	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(h.caCertPEM) {
		t.Fatal("dialNoClientCert: failed to parse CA cert PEM")
	}

	tlsCfg := &tls.Config{
		RootCAs:    caPool,
		MinVersion: tls.VersionTLS13,
	}

	conn, err := grpc.Dial(h.addr, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg))) //nolint:staticcheck
	if err != nil {
		t.Fatalf("dialNoClientCert: grpc.Dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	return pb.NewHubFuseClient(conn)
}

// dialWithClientCert connects to the hub presenting the given mTLS client cert.
func dialWithClientCert(t *testing.T, h hubHandle, certPEM, keyPEM []byte) pb.HubFuseClient {
	t.Helper()

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("dialWithClientCert: X509KeyPair: %v", err)
	}

	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(h.caCertPEM) {
		t.Fatal("dialWithClientCert: failed to parse CA cert PEM")
	}

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caPool,
		MinVersion:   tls.VersionTLS13,
	}

	conn, err := grpc.Dial(h.addr, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg))) //nolint:staticcheck
	if err != nil {
		t.Fatalf("dialWithClientCert: grpc.Dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	return pb.NewHubFuseClient(conn)
}
