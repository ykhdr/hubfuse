package hub

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/hub/store"
)

func TestLoadOrGenerateCerts_AutoSANs(t *testing.T) {
	dataDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	_, _, _, err := loadOrGenerateCerts(dataDir, nil, logger)
	require.NoError(t, err)

	cert := parseServerCert(t, dataDir)

	// Baseline: must contain localhost in DNSNames and 127.0.0.1 in IPAddresses.
	assert.True(t, containsString(cert.DNSNames, "localhost"),
		`server cert DNSNames missing "localhost": %v`, cert.DNSNames)
	assert.True(t, containsIP(cert.IPAddresses, net.ParseIP("127.0.0.1")),
		"server cert IPAddresses missing 127.0.0.1: %v", cert.IPAddresses)

	// Hostname should be present (as DNS name if not an IP).
	hostname, err := os.Hostname()
	if err == nil && hostname != "" && net.ParseIP(hostname) == nil {
		assert.True(t, containsString(cert.DNSNames, hostname),
			"server cert DNSNames missing hostname %q: %v", hostname, cert.DNSNames)
	}
}

func TestLoadOrGenerateCerts_ExtraSANs(t *testing.T) {
	dataDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	extra := []string{"10.99.99.1", "custom.example.com"}
	_, _, _, err := loadOrGenerateCerts(dataDir, extra, logger)
	require.NoError(t, err)

	cert := parseServerCert(t, dataDir)

	assert.True(t, containsIP(cert.IPAddresses, net.ParseIP("10.99.99.1")),
		"server cert IPAddresses missing 10.99.99.1: %v", cert.IPAddresses)
	assert.True(t, containsString(cert.DNSNames, "custom.example.com"),
		`server cert DNSNames missing "custom.example.com": %v`, cert.DNSNames)
}

func TestLoadOrGenerateCerts_ExistingCertsNotRegenerated(t *testing.T) {
	dataDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// First generation.
	_, _, _, err := loadOrGenerateCerts(dataDir, nil, logger)
	require.NoError(t, err, "first loadOrGenerateCerts")

	cert1 := parseServerCert(t, dataDir)

	// Second call with different extra SANs — should load existing, not regenerate.
	_, _, _, err = loadOrGenerateCerts(dataDir, []string{"10.0.0.1"}, logger)
	require.NoError(t, err, "second loadOrGenerateCerts")

	cert2 := parseServerCert(t, dataDir)

	assert.Zero(t, cert1.SerialNumber.Cmp(cert2.SerialNumber),
		"server cert was regenerated: serial %v != %v", cert1.SerialNumber, cert2.SerialNumber)
}

// TestStart_ReconcilesOnlineDevicesFromAPreviousLife covers the invariant the
// bounded shutdown leans on.
//
// The hub trusts its stored statuses across restarts — Register answers a
// joining device with ListOnlineDevices — so an online row that outlived the
// previous process is served to peers as a live endpoint to mount. Today nothing
// clears those rows: the shutdown sweep is the only writer that ever did, and it
// does not run after SIGKILL, OOM or a power cut, nor necessarily in full once
// shutdown is bounded by a budget. The heartbeat monitor only notices a
// timeout/3 later.
//
// A hub that has just started has no online devices by definition — none of them
// has heartbeated yet — so this is settled before the first RPC is served. (#75)
func TestStart_ReconcilesOnlineDevicesFromAPreviousLife(t *testing.T) {
	dataDir := t.TempDir()

	// Leave behind exactly what a SIGKILLed hub leaves: an online row.
	seed, err := store.NewSQLiteStore(filepath.Join(dataDir, common.DBFile))
	require.NoError(t, err, "seed store")
	require.NoError(t, seed.CreateDevice(context.Background(), &store.Device{
		DeviceID:      "dev-ghost",
		Nickname:      "ghost",
		LastIP:        "10.0.0.9",
		SSHPort:       2222,
		Status:        store.StatusOnline,
		LastHeartbeat: time.Now().UTC(),
	}), "seed device")
	require.NoError(t, seed.Close(), "close seed store")

	ready := make(chan struct{})
	h, err := NewHub(Config{
		ListenAddr: "127.0.0.1:0",
		DataDir:    dataDir,
		LogLevel:   slog.LevelError,
		OnReady:    func() { close(ready) },
	})
	require.NoError(t, err, "NewHub")

	serveErr := make(chan error, 1)
	go func() { serveErr <- h.Start(context.Background()) }()

	select {
	case <-ready:
	case err := <-serveErr:
		t.Fatalf("hub exited before it was ready: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("hub never became ready")
	}

	// Reconciliation happens before the listener is bound, so by the time the
	// hub is ready no peer can ever have been told about the ghost.
	online, err := h.store.ListOnlineDevices(context.Background())
	require.NoError(t, err, "ListOnlineDevices")
	assert.Empty(t, online, "a freshly started hub must have no online devices")

	d, err := h.store.GetDevice(context.Background(), "dev-ghost")
	require.NoError(t, err, "GetDevice")
	assert.Equal(t, store.StatusOffline, d.Status)
	assert.Equal(t, "10.0.0.9", d.LastIP, "the endpoint must survive so a heartbeat can recover the device")
	assert.Equal(t, 2222, d.SSHPort)

	require.NoError(t, h.Stop(), "Stop")
	require.NoError(t, <-serveErr, "Start must return cleanly after Stop")
}

func TestDedup(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"empty", nil, []string{}},
		{"no_duplicates", []string{"b", "a", "c"}, []string{"a", "b", "c"}},
		{"with_duplicates", []string{"a", "b", "a", "c", "b"}, []string{"a", "b", "c"}},
		{"single", []string{"x"}, []string{"x"}},
		{"all_same", []string{"a", "a", "a"}, []string{"a"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := dedup(tc.in)
			assert.Equal(t, tc.want, got, "dedup(%v)", tc.in)
			assert.True(t, sort.StringsAreSorted(got), "dedup result not sorted: %v", got)
		})
	}
}

// parseServerCert reads and parses the server certificate from dataDir/tls/server.crt.
func parseServerCert(t *testing.T, dataDir string) *x509.Certificate {
	t.Helper()
	certPEM, err := os.ReadFile(filepath.Join(dataDir, "tls", "server.crt"))
	require.NoError(t, err, "read server.crt")
	block, _ := pem.Decode(certPEM)
	require.NotNil(t, block, "no PEM block in server.crt")
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err, "parse server.crt")
	return cert
}

func containsString(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func containsIP(ips []net.IP, target net.IP) bool {
	for _, ip := range ips {
		if ip.Equal(target) {
			return true
		}
	}
	return false
}
