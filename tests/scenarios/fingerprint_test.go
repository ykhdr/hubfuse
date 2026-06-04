package scenarios_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/ykhdr/hubfuse/tests/scenarios/helpers"
)

// TestScenario_TamperedTokenRefused verifies that an agent attempting to join
// with a hub-issued token whose fingerprint suffix has been tampered with fails
// before any RPC succeeds. The join command must exit non-zero and report an
// error that mentions the fingerprint mismatch.
func TestScenario_TamperedTokenRefused(t *testing.T) {
	hub := helpers.StartHub(t)
	alice := helpers.StartAgent(t, hub, "alice")

	// Issue a real token via the binary (which embeds the correct fingerprint).
	goodToken := hub.IssueJoinToken(t)

	// Tamper with the fingerprint suffix — flip the last character.
	tampered := tamperedScenarioToken(goodToken)
	require.NotEqual(t, goodToken, tampered, "tampered token must differ from original")

	// Attempt to join with the tampered token — expect failure.
	ctx, ok := alice.TryJoinWithTamperedToken(t, hub.Address, tampered, "alice")
	require.False(t, ok, "hubfuse join with tampered fingerprint must exit non-zero")

	// The output must mention fingerprint or MITM.
	lower := strings.ToLower(ctx)
	if !strings.Contains(lower, "fingerprint") && !strings.Contains(lower, "mitm") &&
		!strings.Contains(lower, "mismatch") && !strings.Contains(lower, "tls") &&
		!strings.Contains(lower, "certificate") && !strings.Contains(lower, "handshake") {
		t.Errorf("output should mention fingerprint/mismatch/TLS failure; got:\n%s", ctx)
	}
}

// tamperedScenarioToken flips the last character of the fingerprint suffix.
func tamperedScenarioToken(token string) string {
	idx := strings.LastIndexByte(token, '.')
	if idx < 0 || idx == len(token)-1 {
		return token
	}
	prefix := token[:idx+1]
	fp := []byte(token[idx+1:])
	last := fp[len(fp)-1]
	switch {
	case last >= 'a' && last < 'z':
		fp[len(fp)-1] = last + 1
	case last == 'z':
		fp[len(fp)-1] = '2'
	case last >= '2' && last < '7':
		fp[len(fp)-1] = last + 1
	default:
		fp[len(fp)-1] = 'a'
	}
	return prefix + string(fp)
}
