package integration

import (
	"crypto/tls"
	"crypto/x509"
	"testing"

	"github.com/ykhdr/hubfuse/internal/hub/hubtest"
	pb "github.com/ykhdr/hubfuse/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// dialNoClientCert connects to the hub without presenting a client certificate.
// The client still verifies the server certificate using the given CA PEM.
func dialNoClientCert(t *testing.T, h *hubtest.Harness) pb.HubFuseClient {
	t.Helper()

	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(h.CAPEM) {
		t.Fatal("dialNoClientCert: failed to parse CA cert PEM")
	}

	tlsCfg := &tls.Config{
		RootCAs:    caPool,
		MinVersion: tls.VersionTLS13,
	}

	conn, err := grpc.Dial(h.Addr, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg))) //nolint:staticcheck
	if err != nil {
		t.Fatalf("dialNoClientCert: grpc.Dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	return pb.NewHubFuseClient(conn)
}

// dialWithClientCert connects to the hub presenting the given mTLS client cert.
func dialWithClientCert(t *testing.T, h *hubtest.Harness, certPEM, keyPEM []byte) pb.HubFuseClient {
	t.Helper()

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("dialWithClientCert: X509KeyPair: %v", err)
	}

	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(h.CAPEM) {
		t.Fatal("dialWithClientCert: failed to parse CA cert PEM")
	}

	tlsCfg := &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caPool,
		MinVersion:   tls.VersionTLS13,
	}

	conn, err := grpc.Dial(h.Addr, grpc.WithTransportCredentials(credentials.NewTLS(tlsCfg))) //nolint:staticcheck
	if err != nil {
		t.Fatalf("dialWithClientCert: grpc.Dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	return pb.NewHubFuseClient(conn)
}
