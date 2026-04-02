package hub

import (
	"context"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/hub/store"
	pb "github.com/ykhdr/hubfuse/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// HubConfig holds configuration for a Hub instance.
type HubConfig struct {
	ListenAddr string   // e.g. ":9090"
	DataDir    string   // e.g. "~/.hubfuse-hub"
	LogLevel   string   // "debug", "info", "warn", "error"
	LogOutput  string   // "stderr" or file path
	ExtraSANs  []string // additional SANs for the server TLS certificate
}

// Hub wires together the store, registry, heartbeat monitor, and gRPC server.
type Hub struct {
	config     HubConfig
	store      store.Store
	registry   *Registry
	heartbeat  *HeartbeatMonitor
	grpcServer *grpc.Server
	logger     *slog.Logger
}

// NewHub creates a Hub from the given config. It sets up the logger, opens
// (or creates) the SQLite database, and loads (or generates) the CA and
// server TLS certificates.
func NewHub(config HubConfig) (*Hub, error) {
	logger, err := common.SetupLogger(config.LogLevel, config.LogOutput)
	if err != nil {
		return nil, fmt.Errorf("setup logger: %w", err)
	}

	dataDir := expandHome(config.DataDir)

	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("create data dir %q: %w", dataDir, err)
	}

	dbPath := filepath.Join(dataDir, "hubfuse.db")
	s, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}

	caCert, caKey, err := loadOrGenerateCerts(dataDir, config.ExtraSANs, logger)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("load/generate certs: %w", err)
	}

	registry := NewRegistry(s, caCert, caKey, logger)
	heartbeat := NewHeartbeatMonitor(registry, s, 0, logger)

	return &Hub{
		config:    config,
		store:     s,
		registry:  registry,
		heartbeat: heartbeat,
		logger:    logger,
	}, nil
}

// Start begins serving gRPC requests and starts the heartbeat monitor. It
// writes a PID file and blocks until the gRPC server stops.
func (h *Hub) Start(ctx context.Context) error {
	dataDir := expandHome(h.config.DataDir)
	tlsDir := filepath.Join(dataDir, "tls")

	tlsCfg, err := common.LoadTLSServerConfig(
		filepath.Join(tlsDir, "ca.crt"),
		filepath.Join(tlsDir, "server.crt"),
		filepath.Join(tlsDir, "server.key"),
	)
	if err != nil {
		return fmt.Errorf("load TLS config: %w", err)
	}

	creds := credentials.NewTLS(tlsCfg)

	h.grpcServer = grpc.NewServer(
		grpc.Creds(creds),
		grpc.UnaryInterceptor(AuthUnaryInterceptor),
		grpc.StreamInterceptor(AuthStreamInterceptor),
	)

	srv := NewServer(h.registry, h.logger)
	pb.RegisterHubFuseServer(h.grpcServer, srv)

	lis, err := net.Listen("tcp", h.config.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on %q: %w", h.config.ListenAddr, err)
	}

	pidFile := filepath.Join(dataDir, "hubfuse-hub.pid")
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(os.Getpid())), 0644); err != nil {
		h.logger.Warn("failed to write PID file", slog.String("path", pidFile), slog.Any("error", err))
	}

	// Start heartbeat monitor in the background.
	go h.heartbeat.Start(ctx)

	h.logger.Info("hub gRPC server starting", slog.String("addr", h.config.ListenAddr))

	if err := h.grpcServer.Serve(lis); err != nil {
		return fmt.Errorf("gRPC serve: %w", err)
	}

	return nil
}

// Stop performs a graceful shutdown: broadcasts DeviceOffline for all online
// devices, stops the gRPC server, closes the store, and removes the PID file.
func (h *Hub) Stop() error {
	ctx := context.Background()

	// Mark all online devices offline.
	online, err := h.store.ListOnlineDevices(ctx)
	if err != nil {
		h.logger.Warn("stop: list online devices", slog.Any("error", err))
	} else {
		for _, d := range online {
			event := &pb.Event{
				Payload: &pb.Event_DeviceOffline{
					DeviceOffline: &pb.DeviceOfflineEvent{
						DeviceId: d.DeviceID,
						Nickname: d.Nickname,
					},
				},
			}
			h.registry.Broadcast(event, "")
			if err := h.store.UpdateDeviceStatus(ctx, d.DeviceID, "offline", d.LastIP, d.SSHPort); err != nil {
				h.logger.Warn("stop: mark device offline",
					slog.String("device_id", d.DeviceID),
					slog.Any("error", err))
			}
		}
	}

	if h.grpcServer != nil {
		h.grpcServer.GracefulStop()
	}

	if err := h.store.Close(); err != nil {
		h.logger.Warn("stop: close store", slog.Any("error", err))
	}

	dataDir := expandHome(h.config.DataDir)
	pidFile := filepath.Join(dataDir, "hubfuse-hub.pid")
	if err := os.Remove(pidFile); err != nil && !os.IsNotExist(err) {
		h.logger.Warn("stop: remove PID file", slog.Any("error", err))
	}

	return nil
}

// loadOrGenerateCerts loads existing CA and server TLS certificates from
// dataDir/tls/, or generates and saves them if they do not exist. When
// generating, it auto-detects local IPs/hostnames and merges extraSANs.
func loadOrGenerateCerts(dataDir string, extraSANs []string, logger *slog.Logger) (*x509.Certificate, *rsa.PrivateKey, error) {
	tlsDir := filepath.Join(dataDir, "tls")

	caCertPath := filepath.Join(tlsDir, "ca.crt")
	caKeyPath := filepath.Join(tlsDir, "ca.key")
	serverCertPath := filepath.Join(tlsDir, "server.crt")
	serverKeyPath := filepath.Join(tlsDir, "server.key")

	if fileExists(caCertPath) && fileExists(caKeyPath) && fileExists(serverCertPath) && fileExists(serverKeyPath) {
		logger.Info("loading existing TLS certificates", slog.String("tls_dir", tlsDir))
		return loadCACertAndKey(caCertPath, caKeyPath)
	}

	logger.Info("generating new TLS certificates", slog.String("tls_dir", tlsDir))

	if err := os.MkdirAll(tlsDir, 0700); err != nil {
		return nil, nil, fmt.Errorf("create tls dir %q: %w", tlsDir, err)
	}

	caCert, caKey, err := common.GenerateCA()
	if err != nil {
		return nil, nil, fmt.Errorf("generate CA: %w", err)
	}

	// Save CA cert.
	caCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caCert.Raw})
	caKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(caKey)})

	if err := os.WriteFile(caCertPath, caCertPEM, 0644); err != nil {
		return nil, nil, fmt.Errorf("write CA cert: %w", err)
	}
	if err := os.WriteFile(caKeyPath, caKeyPEM, 0600); err != nil {
		return nil, nil, fmt.Errorf("write CA key: %w", err)
	}

	// Build SAN list: auto-detected local hosts + extra SANs from config.
	hosts := common.LocalHosts()
	hosts = append(hosts, extraSANs...)
	hosts = dedup(hosts)

	logger.Info("generating server TLS certificate", slog.Any("sans", hosts))

	// Generate and save server cert.
	serverCertPEM, serverKeyPEM, err := common.GenerateServerCert(caCert, caKey, hosts)
	if err != nil {
		return nil, nil, fmt.Errorf("generate server cert: %w", err)
	}

	if err := common.SaveCertAndKey(serverCertPath, serverKeyPath, serverCertPEM, serverKeyPEM); err != nil {
		return nil, nil, fmt.Errorf("save server cert/key: %w", err)
	}

	return caCert, caKey, nil
}

// loadCACertAndKey reads the CA certificate and private key from disk.
func loadCACertAndKey(caCertPath, caKeyPath string) (*x509.Certificate, *rsa.PrivateKey, error) {
	certDER, err := common.LoadPEM(caCertPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load CA cert: %w", err)
	}

	caCert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA cert: %w", err)
	}

	keyDER, err := common.LoadPEM(caKeyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load CA key: %w", err)
	}

	caKey, err := x509.ParsePKCS1PrivateKey(keyDER)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA key: %w", err)
	}

	return caCert, caKey, nil
}

// fileExists reports whether a file exists and is not a directory.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// expandHome replaces a leading "~" with the user's home directory.
func expandHome(path string) string {
	if len(path) == 0 || path[0] != '~' {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, path[1:])
}

// dedup returns a sorted, deduplicated copy of ss.
func dedup(ss []string) []string {
	seen := make(map[string]struct{}, len(ss))
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// TLSConfig returns a tls.Config that trusts the hub's CA and presents the
// client certificate identified by the given PEM bytes. This is a convenience
// helper used in tests.
func tlsConfigFromPEM(certPEM, keyPEM, caCertPEM []byte) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("parse client cert/key: %w", err)
	}

	caPool := x509.NewCertPool()
	if !caPool.AppendCertsFromPEM(caCertPEM) {
		return nil, fmt.Errorf("parse CA cert PEM")
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      caPool,
		MinVersion:   tls.VersionTLS13,
	}, nil
}
