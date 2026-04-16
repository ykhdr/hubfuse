package hub

import (
	"context"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"

	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/hub/store"
	pb "github.com/ykhdr/hubfuse/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Config holds configuration for a Hub instance.
type Config struct {
	ListenAddr string     // e.g. ":9090"
	DataDir    string     // e.g. "~/.hubfuse-hub"
	LogFile    string     // path to JSON log file ("" = no file logging)
	LogLevel   slog.Level // file log level (default: Debug)
	Verbose    bool       // show debug logs in console
	ExtraSANs  []string   // additional SANs for the server TLS certificate

	// OnReady, if non-nil, is invoked exactly once from Start right
	// after net.Listen returns — the TCP listener is bound and the
	// kernel is already queueing SYNs, and grpcServer.Serve runs
	// immediately after the callback. The cmd layer uses this hook to
	// write the PID file.
	OnReady func()
}

// Hub wires together the store, registry, heartbeat monitor, and gRPC server.
type Hub struct {
	config     Config
	store      store.Store
	registry   *Registry
	heartbeat  *HeartbeatMonitor
	grpcServer *grpc.Server
	tlsCfg     *tls.Config
	logger     *slog.Logger
}

// NewHub creates a Hub from the given config. It sets up the logger, opens
// (or creates) the SQLite database, and loads (or generates) the CA and
// server TLS certificates.
func NewHub(config Config) (*Hub, error) {
	logger, err := common.SetupLogger(common.LoggerOptions{
		LogFile:   config.LogFile,
		FileLevel: config.LogLevel,
		Verbose:   config.Verbose,
	})
	if err != nil {
		return nil, fmt.Errorf("setup logger: %w", err)
	}

	dataDir := common.ExpandHome(config.DataDir)

	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("create data dir %q: %w", dataDir, err)
	}

	dbPath := filepath.Join(dataDir, common.DBFile)
	s, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}

	caCert, caKey, tlsCfg, err := loadOrGenerateCerts(dataDir, config.ExtraSANs, logger)
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
		tlsCfg:    tlsCfg,
		logger:    logger,
	}, nil
}

// Start begins serving gRPC requests and starts the heartbeat monitor. It
// invokes OnReady (if set) once the listener is up, and blocks until the
// gRPC server stops.
func (h *Hub) Start(ctx context.Context) error {
	creds := credentials.NewTLS(h.tlsCfg)

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

	if h.config.OnReady != nil {
		h.config.OnReady()
	}

	go h.heartbeat.Start(ctx)

	h.logger.Info("hub gRPC server starting", slog.String("addr", h.config.ListenAddr))

	if err := h.grpcServer.Serve(lis); err != nil {
		return fmt.Errorf("gRPC serve: %w", err)
	}

	return nil
}

// Stop performs a graceful shutdown: broadcasts DeviceOffline for all online
// devices, stops the gRPC server, and closes the store.
func (h *Hub) Stop() error {
	ctx := context.Background()

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
			h.registry.BroadcastAll(event)
			if err := h.store.UpdateDeviceStatus(ctx, d.DeviceID, store.StatusOffline, d.LastIP, d.SSHPort); err != nil {
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

	return nil
}

// loadOrGenerateCerts loads existing CA and server TLS certificates from
// dataDir/tls/, or generates and saves them if they do not exist. When
// generating, it auto-detects local IPs/hostnames and merges extraSANs.
func loadOrGenerateCerts(dataDir string, extraSANs []string, logger *slog.Logger) (*x509.Certificate, *rsa.PrivateKey, *tls.Config, error) {
	tlsDir := filepath.Join(dataDir, common.TLSDir)

	caCertPath := filepath.Join(tlsDir, common.CACertFile)
	caKeyPath := filepath.Join(tlsDir, common.CAKeyFile)
	serverCertPath := filepath.Join(tlsDir, common.ServerCertFile)
	serverKeyPath := filepath.Join(tlsDir, common.ServerKeyFile)

	if fileExists(caCertPath) && fileExists(caKeyPath) && fileExists(serverCertPath) && fileExists(serverKeyPath) {
		logger.Info("loading existing TLS certificates", slog.String("tls_dir", tlsDir))
		caCert, caKey, err := loadCACertAndKey(caCertPath, caKeyPath)
		if err != nil {
			return nil, nil, nil, err
		}
		tlsCfg, err := common.LoadTLSServerConfig(caCertPath, serverCertPath, serverKeyPath)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("load TLS config: %w", err)
		}
		return caCert, caKey, tlsCfg, nil
	}

	logger.Info("generating new TLS certificates", slog.String("tls_dir", tlsDir))

	if err := os.MkdirAll(tlsDir, 0700); err != nil {
		return nil, nil, nil, fmt.Errorf("create tls dir %q: %w", tlsDir, err)
	}

	caCert, caKey, err := common.GenerateCA()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generate CA: %w", err)
	}

	caCertPEM := common.EncodeCACertPEM(caCert)
	caKeyPEM := common.EncodeCAKeyPEM(caKey)

	if err := os.WriteFile(caCertPath, caCertPEM, 0644); err != nil {
		return nil, nil, nil, fmt.Errorf("write CA cert: %w", err)
	}
	if err := os.WriteFile(caKeyPath, caKeyPEM, 0600); err != nil {
		return nil, nil, nil, fmt.Errorf("write CA key: %w", err)
	}

	hosts := common.LocalHosts()
	hosts = append(hosts, extraSANs...)
	hosts = dedup(hosts)

	logger.Info("generating server TLS certificate", slog.Any("sans", hosts))

	serverCertPEM, serverKeyPEM, err := common.GenerateServerCert(caCert, caKey, hosts)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generate server cert: %w", err)
	}

	if err := common.SaveCertAndKey(serverCertPath, serverKeyPath, serverCertPEM, serverKeyPEM); err != nil {
		return nil, nil, nil, fmt.Errorf("save server cert/key: %w", err)
	}

	tlsCfg, err := common.LoadTLSServerConfig(caCertPath, serverCertPath, serverKeyPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load TLS config: %w", err)
	}

	return caCert, caKey, tlsCfg, nil
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

