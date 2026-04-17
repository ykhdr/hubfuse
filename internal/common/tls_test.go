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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"

	pb "github.com/ykhdr/hubfuse/proto"
)

// ─── GenerateCA ───────────────────────────────────────────────────────────────

func TestGenerateCA(t *testing.T) {
	cert, key, err := GenerateCA()
	require.NoError(t, err)
	require.NotNil(t, key, "GenerateCA() returned nil key")

	assert.True(t, cert.IsCA, "CA certificate does not have IsCA=true")
	assert.True(t, cert.BasicConstraintsValid, "CA certificate does not have BasicConstraintsValid=true")

	// Self-signed: Issuer == Subject
	assert.Equal(t, cert.Subject.String(), cert.Issuer.String(), "CA is not self-signed")

	require.NotEmpty(t, cert.Subject.Organization, "unexpected organization: %v", cert.Subject.Organization)
	assert.Equal(t, "HubFuse", cert.Subject.Organization[0])

	// Verify the cert with itself as root.
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	_, err = cert.Verify(x509.VerifyOptions{Roots: pool})
	assert.NoError(t, err, "self-signed CA does not verify")
}

// ─── SignClientCert ───────────────────────────────────────────────────────────

func TestSignClientCert(t *testing.T) {
	caCert, caKey, err := GenerateCA()
	require.NoError(t, err)

	deviceID := "device-abc-123"
	certPEM, keyPEM, err := SignClientCert(caCert, caKey, deviceID)
	require.NoError(t, err)

	require.NotEmpty(t, certPEM, "certPEM is empty")
	require.NotEmpty(t, keyPEM, "keyPEM is empty")

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)

	leaf, err := x509.ParseCertificate(tlsCert.Certificate[0])
	require.NoError(t, err)

	assert.Equal(t, deviceID, leaf.Subject.CommonName)

	// Verify against CA.
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	_, err = leaf.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	assert.NoError(t, err, "client cert does not verify against CA")
}

// ─── GenerateServerCert ───────────────────────────────────────────────────────

func TestGenerateServerCert(t *testing.T) {
	caCert, caKey, err := GenerateCA()
	require.NoError(t, err)

	hosts := []string{"localhost", "127.0.0.1", "example.internal"}
	certPEM, keyPEM, err := GenerateServerCert(caCert, caKey, hosts)
	require.NoError(t, err)

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)

	leaf, err := x509.ParseCertificate(tlsCert.Certificate[0])
	require.NoError(t, err)

	// Check SANs.
	dnsSet := make(map[string]bool)
	for _, d := range leaf.DNSNames {
		dnsSet[d] = true
	}
	ipSet := make(map[string]bool)
	for _, ip := range leaf.IPAddresses {
		ipSet[ip.String()] = true
	}

	assert.True(t, dnsSet["localhost"], "DNS SAN 'localhost' not found")
	assert.True(t, dnsSet["example.internal"], "DNS SAN 'example.internal' not found")
	assert.True(t, ipSet["127.0.0.1"], "IP SAN '127.0.0.1' not found; got %v", leaf.IPAddresses)

	// Verify against CA.
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	_, err = leaf.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	assert.NoError(t, err, "server cert does not verify against CA")
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
	require.NoError(t, err)

	serverCertPEM, serverKeyPEM, err := GenerateServerCert(caCert, caKey, []string{"127.0.0.1"})
	require.NoError(t, err)

	deviceID := "test-device-42"
	clientCertPEM, clientKeyPEM, err := SignClientCert(caCert, caKey, deviceID)
	require.NoError(t, err)

	// Build server TLS config.
	serverTLSCert, err := tls.X509KeyPair(serverCertPEM, serverKeyPEM)
	require.NoError(t, err, "server X509KeyPair()")
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
	require.NoError(t, err, "client X509KeyPair()")
	clientTLSCfg := &tls.Config{
		Certificates: []tls.Certificate{clientTLSCert},
		RootCAs:      caPool,
		MinVersion:   tls.VersionTLS13,
	}

	// Start gRPC server on a random port.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

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
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	client := pb.NewHubFuseClient(conn)
	_, err = client.Join(context.Background(), &pb.JoinRequest{DeviceId: deviceID})
	require.NoError(t, err, "Join RPC failed")

	got := <-srv.capturedDeviceID
	assert.Equal(t, deviceID, got)
}

// ─── ExtractDeviceID — no peer ────────────────────────────────────────────────

func TestExtractDeviceID_NoPeer(t *testing.T) {
	_, err := ExtractDeviceID(context.Background())
	assert.Error(t, err, "expected error when no peer in context")
}

// TestExtractDeviceID_NoTLS tests that a non-TLS peer returns an error.
func TestExtractDeviceID_NoTLS(t *testing.T) {
	ctx := peer.NewContext(context.Background(), &peer.Peer{
		Addr:     &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 1234},
		AuthInfo: nil,
	})
	_, err := ExtractDeviceID(ctx)
	assert.Error(t, err, "expected error when AuthInfo is nil")
}

// ─── SavePEM / LoadPEM ────────────────────────────────────────────────────────

func TestSavePEM_LoadPEM_RoundTrip(t *testing.T) {
	dir := t.TempDir()

	original := []byte("hello pem data")

	// Use a cert type (0644 perms).
	path := filepath.Join(dir, "test.pem")
	require.NoError(t, SavePEM(path, pemTypeCert, original))

	// Check file permissions.
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, permCert, info.Mode().Perm(), "cert file perm mismatch")

	loaded, err := LoadPEM(path)
	require.NoError(t, err)

	assert.Equal(t, string(original), string(loaded), "round-trip mismatch")
}

func TestSavePEM_KeyPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key.pem")

	require.NoError(t, SavePEM(path, pemTypeKey, []byte("key data")))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, permKey, info.Mode().Perm(), "key file perm mismatch")
}

func TestLoadPEM_NoPEMBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.pem")
	require.NoError(t, os.WriteFile(path, []byte("not pem"), 0644))
	_, err := LoadPEM(path)
	assert.Error(t, err, "expected error for file with no PEM block")
}

// ─── SaveCertAndKey ───────────────────────────────────────────────────────────

func TestSaveCertAndKey(t *testing.T) {
	caCert, caKey, err := GenerateCA()
	require.NoError(t, err)

	certPEM, keyPEM, err := SignClientCert(caCert, caKey, "roundtrip-device")
	require.NoError(t, err)

	dir := t.TempDir()
	certPath := filepath.Join(dir, "client.crt")
	keyPath := filepath.Join(dir, "client.key")

	require.NoError(t, SaveCertAndKey(certPath, keyPath, certPEM, keyPEM))

	// Verify we can load the pair again.
	_, err = tls.LoadX509KeyPair(certPath, keyPath)
	assert.NoError(t, err, "LoadX509KeyPair() after SaveCertAndKey")

	// Check permissions.
	ci, _ := os.Stat(certPath)
	ki, _ := os.Stat(keyPath)
	assert.Equal(t, permCert, ci.Mode().Perm(), "cert perm mismatch")
	assert.Equal(t, permKey, ki.Mode().Perm(), "key perm mismatch")
}

// ─── LoadTLSServerConfig / LoadTLSClientConfig ────────────────────────────────

func TestLoadTLSServerConfig(t *testing.T) {
	caCert, caKey, err := GenerateCA()
	require.NoError(t, err)

	serverCertPEM, serverKeyPEM, err := GenerateServerCert(caCert, caKey, []string{"localhost"})
	require.NoError(t, err)

	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.crt")
	certPath := filepath.Join(dir, "server.crt")
	keyPath := filepath.Join(dir, "server.key")

	_ = os.WriteFile(caPath, encodeCertPEM(caCert), 0644)
	_ = SaveCertAndKey(certPath, keyPath, serverCertPEM, serverKeyPEM)

	cfg, err := LoadTLSServerConfig(caPath, certPath, keyPath)
	require.NoError(t, err)
	require.NotNil(t, cfg, "LoadTLSServerConfig() returned nil config")
	assert.Equal(t, tls.VerifyClientCertIfGiven, cfg.ClientAuth)
}

func TestLoadTLSClientConfig(t *testing.T) {
	caCert, caKey, err := GenerateCA()
	require.NoError(t, err)

	clientCertPEM, clientKeyPEM, err := SignClientCert(caCert, caKey, "test-device")
	require.NoError(t, err)

	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.crt")
	certPath := filepath.Join(dir, "client.crt")
	keyPath := filepath.Join(dir, "client.key")

	_ = os.WriteFile(caPath, encodeCertPEM(caCert), 0644)
	_ = SaveCertAndKey(certPath, keyPath, clientCertPEM, clientKeyPEM)

	cfg, err := LoadTLSClientConfig(caPath, certPath, keyPath)
	require.NoError(t, err)
	require.NotNil(t, cfg, "LoadTLSClientConfig() returned nil config")
	assert.NotEmpty(t, cfg.Certificates, "LoadTLSClientConfig() returned config with no client certs")
}

// encodeCertPEM is a test helper that PEM-encodes a parsed x509.Certificate.
func encodeCertPEM(cert *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: pemTypeCert, Bytes: cert.Raw})
}
