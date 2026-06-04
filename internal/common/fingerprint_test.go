package common

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"testing"
	"time"
)

// selfSignedCertDER generates a minimal self-signed certificate and returns
// its DER encoding. It is used only by fingerprint tests.
func selfSignedCertDER(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	return der
}

// TestFingerprintFromCertDER_StableAcrossEncodings confirms that the same DER
// input always produces the same fingerprint and that mutating one byte changes
// it.
func TestFingerprintFromCertDER_StableAcrossEncodings(t *testing.T) {
	der := selfSignedCertDER(t)

	fp1 := FingerprintFromCertDER(der)
	fp2 := FingerprintFromCertDER(der)

	if fp1 != fp2 {
		t.Errorf("FingerprintFromCertDER not stable: %q != %q", fp1, fp2)
	}

	if len(fp1) != 26 {
		t.Errorf("fingerprint length = %d, want 26", len(fp1))
	}

	// Verify all chars are lowercase base32 (a-z, 2-7).
	for _, c := range fp1 {
		if !((c >= 'a' && c <= 'z') || (c >= '2' && c <= '7')) {
			t.Errorf("fingerprint contains non-base32 char %q in %q", c, fp1)
			break
		}
	}

	// Mutate one byte and confirm the fingerprint changes.
	mutated := make([]byte, len(der))
	copy(mutated, der)
	mutated[0] ^= 0xFF
	fpMutated := FingerprintFromCertDER(mutated)
	if fp1 == fpMutated {
		t.Error("mutated DER produced the same fingerprint — unexpected collision")
	}
}

func TestParseJoinToken_HappyPath(t *testing.T) {
	const input = "HUB-AB2-9XY.abc123defghijklmnopqrstuv"
	prefix, fp, err := ParseJoinToken(input)
	if err != nil {
		t.Fatalf("ParseJoinToken: unexpected error: %v", err)
	}
	if prefix != "HUB-AB2-9XY" {
		t.Errorf("prefix = %q, want %q", prefix, "HUB-AB2-9XY")
	}
	if fp != "abc123defghijklmnopqrstuv" {
		t.Errorf("fingerprint = %q, want %q", fp, "abc123defghijklmnopqrstuv")
	}
}

func TestParseJoinToken_MissingDot(t *testing.T) {
	_, _, err := ParseJoinToken("HUB-XXX-YYY")
	if !errors.Is(err, ErrJoinTokenMissingFingerprint) {
		t.Errorf("err = %v, want ErrJoinTokenMissingFingerprint", err)
	}
}

// TestParseJoinToken_MultipleDots verifies that only the first dot separates the
// prefix from the fingerprint — everything after the first dot is the fp.
func TestParseJoinToken_MultipleDots(t *testing.T) {
	const input = "HUB-AB2-9XY.abc.def"
	prefix, fp, err := ParseJoinToken(input)
	if err != nil {
		t.Fatalf("ParseJoinToken: unexpected error: %v", err)
	}
	if prefix != "HUB-AB2-9XY" {
		t.Errorf("prefix = %q, want %q", prefix, "HUB-AB2-9XY")
	}
	if fp != "abc.def" {
		t.Errorf("fingerprint = %q, want %q", fp, "abc.def")
	}
}
