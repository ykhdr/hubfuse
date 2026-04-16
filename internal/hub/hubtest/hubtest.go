// Package hubtest provides a reusable in-process hub harness for tests.
// Callers receive a Harness with everything they need to dial the hub
// with mTLS and inspect its state.
package hubtest

import (
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/hub"
	"github.com/ykhdr/hubfuse/internal/hub/store"
	pb "github.com/ykhdr/hubfuse/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Harness is what StartTestHub returns. Fields are exported so tests can
// do setup that reaches past the gRPC surface (seed the store, assert
// registry state).
type Harness struct {
	Addr     string
	CAPEM    []byte
	CACert   *x509.Certificate
	CAKey    *rsa.PrivateKey
	Store    store.Store
	Registry *hub.Registry
	// TLSDir is the on-disk path where the harness wrote its TLS files.
	// Useful for tests that want to compose their own client cert files.
	TLSDir string
	// DataDir is the on-disk path that contains the SQLite DB and the
	// TLSDir. Useful for tests that want to restart the hub against the
	// same data (see Options.DataDir).
	DataDir string

	stop func()
}

// Options tunes StartTestHub's behaviour. The zero value is fine for
// most tests.
type Options struct {
	// DataDir, when non-empty, is used as the on-disk home for the
	// SQLite DB and the TLS directory. This lets tests restart the hub
	// against existing state. If the TLS files already exist in
	// DataDir/<TLSDir>, they are loaded instead of being regenerated.
	// When empty, a fresh t.TempDir() is used.
	DataDir string

	// InMemoryStore, when true, uses a ":memory:" SQLite store instead
	// of a file-backed one. Ignored if DataDir is set.
	InMemoryStore bool

	// Logger overrides the default test logger. When nil, a Debug-level
	// text handler writing to os.Stderr is used.
	Logger *slog.Logger
}

// ClientCreds issues a new device cert signed by the harness CA and
// returns TLS credentials ready for gRPC dial. deviceID is the CN
// placed in the cert and the value the auth interceptor will observe.
func (h *Harness) ClientCreds(t *testing.T, deviceID string) credentials.TransportCredentials {
	t.Helper()
	certPEM, keyPEM, err := common.SignClientCert(h.CACert, h.CAKey, deviceID)
	if err != nil {
		t.Fatalf("sign client cert: %v", err)
	}
	clientCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("parse client cert: %v", err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(h.CAPEM)
	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{clientCert},
		RootCAs:      pool,
		MinVersion:   tls.VersionTLS13,
	})
}

// Stop shuts the hub down. It is also registered as a t.Cleanup, so
// callers do not normally need to invoke it. Calling Stop a second
// time is a no-op.
func (h *Harness) Stop() {
	if h.stop != nil {
		h.stop()
		h.stop = nil
	}
}

// StartTestHub builds an in-process hub on a random localhost port.
// The returned Harness cleans itself up via t.Cleanup.
//
// Implementation detail: we materialise the TLS files on disk in a
// t.TempDir so LoadTLSServerConfig's normal loader path runs unchanged.
func StartTestHub(t *testing.T) *Harness {
	t.Helper()
	return StartTestHubWithOptions(t, Options{InMemoryStore: true})
}

// StartTestHubWithOptions is the knob-exposing version of StartTestHub.
func StartTestHubWithOptions(t *testing.T, opts Options) *Harness {
	t.Helper()

	dataDir := opts.DataDir
	if dataDir == "" {
		dataDir = t.TempDir()
	}

	var dbPath string
	if opts.DataDir == "" && opts.InMemoryStore {
		dbPath = ":memory:"
	} else {
		dbPath = filepath.Join(dataDir, common.DBFile)
	}

	s, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("hubtest: NewSQLiteStore: %v", err)
	}

	tlsDir := filepath.Join(dataDir, common.TLSDir)
	if err := os.MkdirAll(tlsDir, 0o700); err != nil {
		_ = s.Close()
		t.Fatalf("hubtest: mkdir tls: %v", err)
	}

	caCertPath := filepath.Join(tlsDir, common.CACertFile)
	caKeyPath := filepath.Join(tlsDir, common.CAKeyFile)
	serverCertPath := filepath.Join(tlsDir, common.ServerCertFile)
	serverKeyPath := filepath.Join(tlsDir, common.ServerKeyFile)

	caCert, caKey, caPEM, err := loadOrCreateCA(caCertPath, caKeyPath, serverCertPath, serverKeyPath)
	if err != nil {
		_ = s.Close()
		t.Fatalf("hubtest: CA setup: %v", err)
	}

	tlsCfg, err := common.LoadTLSServerConfig(caCertPath, serverCertPath, serverKeyPath)
	if err != nil {
		_ = s.Close()
		t.Fatalf("hubtest: LoadTLSServerConfig: %v", err)
	}

	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}

	registry := hub.NewRegistry(s, caCert, caKey, logger)

	grpcServer := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(tlsCfg)),
		grpc.UnaryInterceptor(hub.AuthUnaryInterceptor),
		grpc.StreamInterceptor(hub.AuthStreamInterceptor),
	)
	srv := hub.NewServer(registry, logger)
	pb.RegisterHubFuseServer(grpcServer, srv)

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		_ = s.Close()
		t.Fatalf("hubtest: listen: %v", err)
	}

	go func() { _ = grpcServer.Serve(lis) }()

	var stopped bool
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		grpcServer.GracefulStop()
		_ = s.Close()
	}
	t.Cleanup(stop)

	return &Harness{
		Addr:     lis.Addr().String(),
		CAPEM:    caPEM,
		CACert:   caCert,
		CAKey:    caKey,
		Store:    s,
		Registry: registry,
		TLSDir:   tlsDir,
		DataDir:  dataDir,
		stop:     stop,
	}
}

// loadOrCreateCA materialises the CA + server cert on disk, reusing
// existing files if they are already present. It returns the parsed CA
// cert + key and the PEM-encoded CA cert for client TLS configs.
func loadOrCreateCA(caCertPath, caKeyPath, serverCertPath, serverKeyPath string) (*x509.Certificate, *rsa.PrivateKey, []byte, error) {
	if _, err := os.Stat(caCertPath); os.IsNotExist(err) {
		caCert, caKey, err := common.GenerateCA()
		if err != nil {
			return nil, nil, nil, err
		}

		caPEM := common.EncodeCACertPEM(caCert)
		caKeyPEM := common.EncodeCAKeyPEM(caKey)

		if err := os.WriteFile(caCertPath, caPEM, 0o644); err != nil {
			return nil, nil, nil, err
		}
		if err := os.WriteFile(caKeyPath, caKeyPEM, 0o600); err != nil {
			return nil, nil, nil, err
		}

		serverCertPEM, serverKeyPEM, err := common.GenerateServerCert(caCert, caKey, common.LocalHosts())
		if err != nil {
			return nil, nil, nil, err
		}
		if err := common.SaveCertAndKey(serverCertPath, serverKeyPath, serverCertPEM, serverKeyPEM); err != nil {
			return nil, nil, nil, err
		}

		return caCert, caKey, caPEM, nil
	}

	caCertDER, err := common.LoadPEM(caCertPath)
	if err != nil {
		return nil, nil, nil, err
	}
	caCert, err := x509.ParseCertificate(caCertDER)
	if err != nil {
		return nil, nil, nil, err
	}

	caKeyDER, err := common.LoadPEM(caKeyPath)
	if err != nil {
		return nil, nil, nil, err
	}
	caKey, err := x509.ParsePKCS1PrivateKey(caKeyDER)
	if err != nil {
		return nil, nil, nil, err
	}

	caPEM, err := os.ReadFile(caCertPath)
	if err != nil {
		return nil, nil, nil, err
	}

	return caCert, caKey, caPEM, nil
}
