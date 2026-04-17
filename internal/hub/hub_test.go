package hub

import (
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
