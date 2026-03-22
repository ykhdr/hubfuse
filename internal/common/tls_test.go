package common

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"

	pb "github.com/ykhdr/hubfuse/proto"
)

// ─── GenerateCA ───────────────────────────────────────────────────────────────

func TestGenerateCA(t *testing.T) {
	cert, key, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA() error: %v", err)
	}

	if key == nil {
		t.Fatal("GenerateCA() returned nil key")
	}

	if !cert.IsCA {
		t.Error("CA certificate does not have IsCA=true")
	}

	if !cert.BasicConstraintsValid {
		t.Error("CA certificate does not have BasicConstraintsValid=true")
	}

	// Self-signed: Issuer == Subject
	if cert.Issuer.String() != cert.Subject.String() {
		t.Errorf("CA is not self-signed: issuer=%q subject=%q", cert.Issuer, cert.Subject)
	}

	if len(cert.Subject.Organization) == 0 || cert.Subject.Organization[0] != "HubFuse" {
		t.Errorf("unexpected organization: %v", cert.Subject.Organization)
	}

	// Verify the cert with itself as root.
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	_, err = cert.Verify(x509.VerifyOptions{Roots: pool})
	if err != nil {
		t.Errorf("self-signed CA does not verify: %v", err)
	}
}

// ─── SignClientCert ───────────────────────────────────────────────────────────

func TestSignClientCert(t *testing.T) {
	caCert, caKey, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA(): %v", err)
	}

	deviceID := "device-abc-123"
	certPEM, keyPEM, err := SignClientCert(caCert, caKey, deviceID)
	if err != nil {
		t.Fatalf("SignClientCert(): %v", err)
	}

	if len(certPEM) == 0 {
		t.Fatal("certPEM is empty")
	}
	if len(keyPEM) == 0 {
		t.Fatal("keyPEM is empty")
	}

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair(): %v", err)
	}

	leaf, err := x509.ParseCertificate(tlsCert.Certificate[0])
	if err != nil {
		t.Fatalf("ParseCertificate(): %v", err)
	}

	if leaf.Subject.CommonName != deviceID {
		t.Errorf("CN = %q, want %q", leaf.Subject.CommonName, deviceID)
	}

	// Verify against CA.
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	_, err = leaf.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	if err != nil {
		t.Errorf("client cert does not verify against CA: %v", err)
	}
}

// ─── GenerateServerCert ───────────────────────────────────────────────────────

func TestGenerateServerCert(t *testing.T) {
	caCert, caKey, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA(): %v", err)
	}

	hosts := []string{"localhost", "127.0.0.1", "example.internal"}
	certPEM, keyPEM, err := GenerateServerCert(caCert, caKey, hosts)
	if err != nil {
		t.Fatalf("GenerateServerCert(): %v", err)
	}

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("X509KeyPair(): %v", err)
	}

	leaf, err := x509.ParseCertificate(tlsCert.Certificate[0])
	if err != nil {
		t.Fatalf("ParseCertificate(): %v", err)
	}

	// Check SANs.
	dnsSet := make(map[string]bool)
	for _, d := range leaf.DNSNames {
		dnsSet[d] = true
	}
	ipSet := make(map[string]bool)
	for _, ip := range leaf.IPAddresses {
		ipSet[ip.String()] = true
	}

	if !dnsSet["localhost"] {
		t.Error("DNS SAN 'localhost' not found")
	}
	if !dnsSet["example.internal"] {
		t.Error("DNS SAN 'example.internal' not found")
	}
	if !ipSet["127.0.0.1"] {
		t.Errorf("IP SAN '127.0.0.1' not found; got %v", leaf.IPAddresses)
	}

	// Verify against CA.
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	_, err = leaf.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err != nil {
		t.Errorf("server cert does not verify against CA: %v", err)
	}
}

// ─── ExtractDeviceID (real TLS handshake) ────────────────────────────────────

// minimalHubFuseServer implements the generated gRPC service interface with
// no-op handlers so that we can start a real gRPC server for the mTLS test.
type minimalHubFuseServer struct {
	pb.UnimplementedHubFuseServer
	// capturedDeviceID is set by the Join handler which calls ExtractDeviceID.
	capturedDeviceID chan string
}

func (s *minimalHubFuseServer) Join(ctx context.Context, _ *pb.JoinRequest) (*pb.JoinResponse, error) {
	id, err := ExtractDeviceID(ctx)
	if err != nil {
		// No client cert was presented; that is valid per the spec
		// (Join is unauthenticated). Send empty string.
		id = ""
	}
	s.capturedDeviceID <- id
	return &pb.JoinResponse{Success: true}, nil
}

func TestExtractDeviceID(t *testing.T) {
	// Generate CA + server cert + client cert.
	caCert, caKey, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA(): %v", err)
	}

	serverCertPEM, serverKeyPEM, err := GenerateServerCert(caCert, caKey, []string{"127.0.0.1"})
	if err != nil {
		t.Fatalf("GenerateServerCert(): %v", err)
	}

	deviceID := "test-device-42"
	clientCertPEM, clientKeyPEM, err := SignClientCert(caCert, caKey, deviceID)
	if err != nil {
		t.Fatalf("SignClientCert(): %v", err)
	}

	// Build server TLS config.
	serverTLSCert, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	if err != nil {
		t.Fatalf("server X509KeyPair(): %v", err)
	}
	caPool := x509.NewCertPool()
	caPool.AddCert(caCert)
	serverTLSCfg := &tls.Config{
		Certificates: []tls.Certificate{serverTLSCert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    caPool,
		MinVersion:   tls.VersionTLS13,
	}

	// Build client TLS config.
	clientTLSCert, err := tls.X509KeyPair(clientCertPEM, clientKeyPEM)
	if err != nil {
		t.Fatalf("client X509KeyPair(): %v", err)
	}
	clientTLSCfg := &tls.Config{
		Certificates: []tls.Certificate{clientTLSCert},
		RootCAs:      caPool,
		MinVersion:   tls.VersionTLS13,
	}

	// Start gRPC server on a random port.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen(): %v", err)
	}

	srv := &minimalHubFuseServer{capturedDeviceID: make(chan string, 1)}
	grpcSrv := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLSCfg)))
	pb.RegisterHubFuseServer(grpcSrv, srv)

	go func() {
		_ = grpcSrv.Serve(lis)
	}()
	t.Cleanup(grpcSrv.Stop)

	// Connect with the client certificate.
	addr := lis.Addr().String()
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(credentials.NewTLS(clientTLSCfg)))
	if err != nil {
		t.Fatalf("grpc.NewClient(): %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })

	client := pb.NewHubFuseClient(conn)
	_, err = client.Join(context.Background(), &pb.JoinRequest{DeviceId: deviceID})
	if err != nil {
		t.Fatalf("Join RPC failed: %v", err)
	}

	got := <-srv.capturedDeviceID
	if got != deviceID {
		t.Errorf("ExtractDeviceID = %q, want %q", got, deviceID)
	}
}

// ─── ExtractDeviceID — no peer ────────────────────────────────────────────────

func TestExtractDeviceID_NoPeer(t *testing.T) {
	_, err := ExtractDeviceID(context.Background())
	if err == nil {
		t.Fatal("expected error when no peer in context, got nil")
	}
}

// TestExtractDeviceID_NoTLS tests that a non-TLS peer returns an error.
func TestExtractDeviceID_NoTLS(t *testing.T) {
	ctx := peer.NewContext(context.Background(), &peer.Peer{
		Addr:     &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1234},
		AuthInfo: nil,
	})
	_, err := ExtractDeviceID(ctx)
	if err == nil {
		t.Fatal("expected error when AuthInfo is nil, got nil")
	}
}

// ─── SavePEM / LoadPEM ────────────────────────────────────────────────────────

func TestSavePEM_LoadPEM_RoundTrip(t *testing.T) {
	dir := t.TempDir()

	original := []byte("hello pem data")

	// Use a cert type (0644 perms).
	path := filepath.Join(dir, "test.pem")
	if err := SavePEM(path, pemTypeCert, original); err != nil {
		t.Fatalf("SavePEM(): %v", err)
	}

	// Check file permissions.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(): %v", err)
	}
	if info.Mode().Perm() != permCert {
		t.Errorf("cert file perm = %o, want %o", info.Mode().Perm(), permCert)
	}

	loaded, err := LoadPEM(path)
	if err != nil {
		t.Fatalf("LoadPEM(): %v", err)
	}

	if string(loaded) != string(original) {
		t.Errorf("round-trip mismatch: got %q, want %q", loaded, original)
	}
}

func TestSavePEM_KeyPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key.pem")

	if err := SavePEM(path, pemTypeKey, []byte("key data")); err != nil {
		t.Fatalf("SavePEM(): %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(): %v", err)
	}
	if info.Mode().Perm() != permKey {
		t.Errorf("key file perm = %o, want %o", info.Mode().Perm(), permKey)
	}
}

func TestLoadPEM_NoPEMBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.pem")
	if err := os.WriteFile(path, []byte("not pem"), 0644); err != nil {
		t.Fatalf("WriteFile(): %v", err)
	}
	_, err := LoadPEM(path)
	if err == nil {
		t.Fatal("expected error for file with no PEM block, got nil")
	}
}

// ─── SaveCertAndKey ───────────────────────────────────────────────────────────

func TestSaveCertAndKey(t *testing.T) {
	caCert, caKey, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA(): %v", err)
	}

	certPEM, keyPEM, err := SignClientCert(caCert, caKey, "roundtrip-device")
	if err != nil {
		t.Fatalf("SignClientCert(): %v", err)
	}

	dir := t.TempDir()
	certPath := filepath.Join(dir, "client.crt")
	keyPath := filepath.Join(dir, "client.key")

	if err := SaveCertAndKey(certPath, keyPath, certPEM, keyPEM); err != nil {
		t.Fatalf("SaveCertAndKey(): %v", err)
	}

	// Verify we can load the pair again.
	if _, err := tls.LoadX509KeyPair(certPath, keyPath); err != nil {
		t.Errorf("LoadX509KeyPair() after SaveCertAndKey: %v", err)
	}

	// Check permissions.
	ci, _ := os.Stat(certPath)
	ki, _ := os.Stat(keyPath)
	if ci.Mode().Perm() != permCert {
		t.Errorf("cert perm = %o, want %o", ci.Mode().Perm(), permCert)
	}
	if ki.Mode().Perm() != permKey {
		t.Errorf("key perm = %o, want %o", ki.Mode().Perm(), permKey)
	}
}

// ─── LoadTLSServerConfig / LoadTLSClientConfig ────────────────────────────────

func TestLoadTLSServerConfig(t *testing.T) {
	caCert, caKey, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA(): %v", err)
	}

	serverCertPEM, serverKeyPEM, err := GenerateServerCert(caCert, caKey, []string{"localhost"})
	if err != nil {
		t.Fatalf("GenerateServerCert(): %v", err)
	}

	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.crt")
	certPath := filepath.Join(dir, "server.crt")
	keyPath := filepath.Join(dir, "server.key")

	_ = os.WriteFile(caPath, encodeCertPEM(caCert), 0644)
	_ = SaveCertAndKey(certPath, keyPath, serverCertPEM, serverKeyPEM)

	cfg, err := LoadTLSServerConfig(caPath, certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadTLSServerConfig(): %v", err)
	}
	if cfg == nil {
		t.Fatal("LoadTLSServerConfig() returned nil config")
	}
	if cfg.ClientAuth != tls.VerifyClientCertIfGiven {
		t.Errorf("ClientAuth = %v, want VerifyClientCertIfGiven", cfg.ClientAuth)
	}
}

func TestLoadTLSClientConfig(t *testing.T) {
	caCert, caKey, err := GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA(): %v", err)
	}

	clientCertPEM, clientKeyPEM, err := SignClientCert(caCert, caKey, "test-device")
	if err != nil {
		t.Fatalf("SignClientCert(): %v", err)
	}

	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.crt")
	certPath := filepath.Join(dir, "client.crt")
	keyPath := filepath.Join(dir, "client.key")

	_ = os.WriteFile(caPath, encodeCertPEM(caCert), 0644)
	_ = SaveCertAndKey(certPath, keyPath, clientCertPEM, clientKeyPEM)

	cfg, err := LoadTLSClientConfig(caPath, certPath, keyPath)
	if err != nil {
		t.Fatalf("LoadTLSClientConfig(): %v", err)
	}
	if cfg == nil {
		t.Fatal("LoadTLSClientConfig() returned nil config")
	}
	if len(cfg.Certificates) == 0 {
		t.Error("LoadTLSClientConfig() returned config with no client certs")
	}
}

// encodeCertPEM is a test helper that PEM-encodes a parsed x509.Certificate.
func encodeCertPEM(cert *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: pemTypeCert, Bytes: cert.Raw})
}
