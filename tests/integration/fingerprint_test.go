package integration

import (
	"context"
	"encoding/pem"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/ykhdr/hubfuse/internal/agent"
	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/hub/hubtest"
)

// serverCertFP reads the test hub's server.crt and returns its pinning fingerprint.
func serverCertFP(t *testing.T, h *hubtest.Harness) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(h.TLSDir, common.ServerCertFile))
	if err != nil {
		t.Fatalf("read server cert: %v", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		t.Fatal("no PEM block in server cert")
	}
	return common.FingerprintFromCertDER(block.Bytes)
}

// silentLogger returns a logger that discards all output, appropriate for
// transient test connections.
func silentLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// tamperedFP flips the last base32 character of fp to produce a different value.
func tamperedFP(fp string) string {
	if len(fp) == 0 {
		return fp
	}
	b := []byte(fp)
	last := b[len(b)-1]
	switch {
	case last >= 'a' && last < 'z':
		b[len(b)-1] = last + 1
	case last == 'z':
		b[len(b)-1] = '2'
	case last >= '2' && last < '7':
		b[len(b)-1] = last + 1
	default:
		b[len(b)-1] = 'a'
	}
	return string(b)
}

// TestJoin_FingerprintMatch_Succeeds verifies that DialPinned with the
// correct fingerprint can complete a Join RPC successfully.
func TestJoin_FingerprintMatch_Succeeds(t *testing.T) {
	h := hubtest.StartTestHub(t)
	fp := serverCertFP(t, h)

	hubClient, err := agent.DialPinned(h.Addr, fp, silentLogger())
	if err != nil {
		t.Fatalf("DialPinned: %v", err)
	}
	t.Cleanup(func() { _ = hubClient.Close() })

	tok, _, err := h.Registry.IssueJoinToken(context.Background())
	if err != nil {
		t.Fatalf("IssueJoinToken: %v", err)
	}

	resp, err := hubClient.Join(context.Background(),
		"fp-match-dev-"+uuid.New().String(),
		"fp-match-nick-"+uuid.New().String(),
		tok,
	)
	if err != nil {
		t.Fatalf("Join RPC: %v", err)
	}
	if !resp.Success {
		t.Fatalf("Join failed: %s", resp.Error)
	}
	if len(resp.ClientCert) == 0 {
		t.Error("Join: ClientCert is empty")
	}
}

// TestJoin_FingerprintMismatch_Refused verifies that DialPinned rejects a
// connection when the hub's TLS fingerprint differs from the expected value.
// The Join RPC must never succeed — the TLS handshake should abort.
func TestJoin_FingerprintMismatch_Refused(t *testing.T) {
	h := hubtest.StartTestHub(t)
	fp := serverCertFP(t, h)
	tampered := tamperedFP(fp)

	hubClient, err := agent.DialPinned(h.Addr, tampered, silentLogger())
	if err != nil {
		// Some gRPC implementations reject at dial time — acceptable, but the
		// error must still be a TLS/pinning failure, not an unrelated dial
		// problem (refused connection, address parse, etc.) that would mask a
		// regression of the actual security control.
		assertTLSFingerprintError(t, err, "DialPinned early error")
		return
	}
	t.Cleanup(func() { _ = hubClient.Close() })

	tok, _, err := h.Registry.IssueJoinToken(context.Background())
	if err != nil {
		t.Fatalf("IssueJoinToken: %v", err)
	}

	_, err = hubClient.Join(context.Background(),
		"fp-mismatch-dev-"+uuid.New().String(),
		"fp-mismatch-nick-"+uuid.New().String(),
		tok,
	)
	if err == nil {
		t.Fatal("Join RPC succeeded with tampered fingerprint — expected TLS rejection")
	}
	assertTLSFingerprintError(t, err, "Join RPC")
}

// assertTLSFingerprintError fails the test unless err names a TLS/pinning
// failure — guards against a regression where the test would pass on an
// unrelated dial error (refused connection, parse failure, etc.).
func assertTLSFingerprintError(t *testing.T, err error, context string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected error, got nil", context)
	}
	msg := strings.ToLower(err.Error())
	for _, want := range []string{"fingerprint", "tls", "handshake", "certificate", "transport"} {
		if strings.Contains(msg, want) {
			return
		}
	}
	t.Errorf("%s: error does not look like a TLS/fingerprint failure: %v", context, err)
}

// TestJoin_NoFingerprint_Refused verifies that ParseJoinToken rejects a token
// without a dot and returns ErrJoinTokenMissingFingerprint.
func TestJoin_NoFingerprint_Refused(t *testing.T) {
	_, _, err := common.ParseJoinToken("HUB-XXX-YYY")
	if err == nil {
		t.Fatal("expected error for token without fingerprint, got nil")
	}
	if !strings.Contains(err.Error(), "fingerprint") {
		t.Errorf("error message should mention fingerprint, got: %v", err)
	}
}
