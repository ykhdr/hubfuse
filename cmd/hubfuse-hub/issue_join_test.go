package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ykhdr/hubfuse/internal/common"
)

// reDottedToken matches the new dotted format: HUB-XXX-YYY.<26-char base32>.
var reDottedToken = regexp.MustCompile(`^HUB-[A-Z0-9]{3}-[A-Z0-9]{3}\.[a-z2-7]{26}\n$`)

// reFPSuffix matches the 26-char lowercase base32 fingerprint suffix.
var reFPSuffix = regexp.MustCompile(`^[a-z2-7]{26}$`)

// seedTLSCerts generates a CA and server cert in <dir>/tls/ so that
// issueJoinCmd can load the server cert fingerprint.
func seedTLSCerts(t *testing.T, dir string) {
	t.Helper()
	tlsDir := filepath.Join(dir, common.TLSDir)
	require.NoError(t, os.MkdirAll(tlsDir, 0o700))

	caCert, caKey, err := common.GenerateCA()
	require.NoError(t, err, "GenerateCA")

	caPEM := common.EncodeCACertPEM(caCert)
	caKeyPEM := common.EncodeCAKeyPEM(caKey)
	require.NoError(t, os.WriteFile(filepath.Join(tlsDir, common.CACertFile), caPEM, 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(tlsDir, common.CAKeyFile), caKeyPEM, 0o600))

	serverCertPEM, serverKeyPEM, err := common.GenerateServerCert(caCert, caKey, []string{"127.0.0.1"})
	require.NoError(t, err, "GenerateServerCert")
	require.NoError(t, common.SaveCertAndKey(
		filepath.Join(tlsDir, common.ServerCertFile),
		filepath.Join(tlsDir, common.ServerKeyFile),
		serverCertPEM, serverKeyPEM,
	))
}

func TestIssueJoinCmd_TokenFormat(t *testing.T) {
	dir := t.TempDir()
	seedTLSCerts(t, dir)

	cmd := issueJoinCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--data-dir", dir, "--ttl", "5m"})

	require.NoError(t, cmd.Execute())

	got := out.String()
	assert.Regexp(t, reDottedToken, got, "stdout should match HUB-XXX-YYY.<26 base32> format")
}

func TestIssueJoinCmd_TokenContainsExactlyOneDot(t *testing.T) {
	dir := t.TempDir()
	seedTLSCerts(t, dir)

	cmd := issueJoinCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--data-dir", dir, "--ttl", "5m"})

	require.NoError(t, cmd.Execute())

	token := strings.TrimSpace(out.String())
	parts := strings.SplitN(token, ".", 2)
	require.Len(t, parts, 2, "token must contain exactly one dot separating prefix from fingerprint")

	prefix := parts[0]
	fp := parts[1]

	assert.Regexp(t, regexp.MustCompile(`^HUB-[A-Z0-9]{3}-[A-Z0-9]{3}$`), prefix, "prefix must match HUB-XXX-YYY")
	assert.Regexp(t, reFPSuffix, fp, "fingerprint suffix must be 26 lowercase base32 chars")
}

func TestIssueJoinCmd_TokenStoredInDB_OnlyPrefix(t *testing.T) {
	dir := t.TempDir()
	seedTLSCerts(t, dir)

	cmd := issueJoinCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--data-dir", dir, "--ttl", "5m"})

	require.NoError(t, cmd.Execute())

	dottedToken := strings.TrimSpace(out.String())
	require.NotEmpty(t, dottedToken, "token should not be empty")

	// Split off the fingerprint — the DB stores only the prefix.
	parts := strings.SplitN(dottedToken, ".", 2)
	require.Len(t, parts, 2, "printed token must include fingerprint suffix")
	prefix := parts[0]

	s, err := openStore(context.Background(), dir)
	require.NoError(t, err)
	defer s.Close()

	// The DB must be queryable by the prefix alone.
	jt, err := s.GetJoinToken(context.Background(), prefix)
	require.NoError(t, err, "prefix should be queryable from store")
	assert.Equal(t, prefix, jt.Token, "store must contain only the prefix, not the dotted form")

	// Querying by the dotted form must fail (token not found).
	_, err = s.GetJoinToken(context.Background(), dottedToken)
	assert.Error(t, err, "store must NOT contain the full dotted token")
}

func TestIssueJoinCmd_RejectsNegativeTTL(t *testing.T) {
	dir := t.TempDir()

	cmd := issueJoinCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--data-dir", dir, "--ttl", "-5m"})

	err := cmd.Execute()
	require.Error(t, err, "negative --ttl must be rejected")
	assert.Contains(t, err.Error(), "--ttl must be positive")
}
