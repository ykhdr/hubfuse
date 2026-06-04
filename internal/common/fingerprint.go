package common

import (
	"crypto/sha256"
	"encoding/base32"
	"strings"
)

// FingerprintFromCertDER returns the agent-pinning fingerprint of a server
// certificate: base32(no-padding, lowercase) of the first 16 bytes of
// SHA-256(cert DER). 26 ASCII chars.
func FingerprintFromCertDER(der []byte) string {
	sum := sha256.Sum256(der)
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(sum[:16])
	return strings.ToLower(enc)
}

// ParseJoinToken splits a token of the form HUB-XXX-YYY.<fp> into its parts.
// Only the first dot separates the prefix from the fingerprint; any additional
// dots are considered part of the fingerprint. Returns
// ErrJoinTokenMissingFingerprint when no dot is present.
func ParseJoinToken(token string) (prefix, fingerprint string, err error) {
	idx := strings.IndexByte(token, '.')
	if idx < 0 {
		return "", "", ErrJoinTokenMissingFingerprint
	}
	return token[:idx], token[idx+1:], nil
}
