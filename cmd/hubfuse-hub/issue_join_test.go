package main

import (
	"bytes"
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var reJoinToken = regexp.MustCompile(`^HUB-[A-Z0-9]{3}-[A-Z0-9]{3}\n$`)

func TestIssueJoinCmd_TokenFormat(t *testing.T) {
	dir := t.TempDir()

	cmd := issueJoinCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--data-dir", dir, "--ttl", "5m"})

	require.NoError(t, cmd.Execute())

	got := out.String()
	assert.Regexp(t, reJoinToken, got, "stdout should match HUB-XXX-YYY format")
}

func TestIssueJoinCmd_TokenStoredInDB(t *testing.T) {
	dir := t.TempDir()

	cmd := issueJoinCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--data-dir", dir, "--ttl", "5m"})

	require.NoError(t, cmd.Execute())

	token := strings.TrimSpace(out.String())
	require.NotEmpty(t, token, "token should not be empty")

	s, err := openStore(context.Background(), dir)
	require.NoError(t, err)
	defer s.Close()

	jt, err := s.GetJoinToken(context.Background(), token)
	require.NoError(t, err, "token should be queryable from store")
	assert.Equal(t, token, jt.Token)
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
