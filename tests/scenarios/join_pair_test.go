package scenarios_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/ykhdr/hubfuse/tests/scenarios/helpers"
)

// TestJoinPersistsCert verifies that `hubfuse join` writes the full TLS
// material set under $HOME/.hubfuse/tls/ after a successful join.
func TestJoinPersistsCert(t *testing.T) {
	hub := helpers.StartHub(t)
	alice := helpers.StartAgent(t, hub, "alice")
	alice.Join(t)

	tlsDir := filepath.Join(alice.HomeDir, ".hubfuse", "tls")
	for _, name := range []string{"ca.crt", "client.crt", "client.key"} {
		path := filepath.Join(tlsDir, name)
		info, err := os.Stat(path)
		require.NoErrorf(t, err, "expected %s on disk", path)
		require.Falsef(t, info.IsDir(), "%s should be a file, not a directory", path)
		require.NotZerof(t, info.Size(), "%s should not be empty", path)
	}

	// Also verify identity file exists.
	identityPath := filepath.Join(alice.HomeDir, ".hubfuse", "device.json")
	_, err := os.Stat(identityPath)
	require.NoErrorf(t, err, "expected %s on disk", identityPath)
}

// TestScenario_Join_RefusesWithoutToken verifies that `hubfuse join` exits
// non-zero and reports a missing-flag error when --token is omitted. This
// catches any regression that makes the flag optional again.
func TestScenario_Join_RefusesWithoutToken(t *testing.T) {
	hub := helpers.StartHub(t)
	// StartAgent just creates an isolated HOME; no Join or daemon needed.
	alice := helpers.StartAgent(t, hub, "alice")

	// tryRun returns (combinedOutput, exitZero). We expect exitZero == false.
	out, ok := alice.TryJoinWithoutToken(t, hub.Address)

	require.False(t, ok, "hubfuse join without --token must exit non-zero")
	require.True(t, strings.Contains(out, "required"),
		"output should mention \"required\" (cobra flag error); got: %s", out)
}
