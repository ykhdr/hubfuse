package scenarios_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/tests/scenarios/helpers"
)

// TestScenario_LeaveAndRejoin verifies the full leave → local wipe → rejoin cycle:
//  1. Alice joins the hub.
//  2. Alice runs `hubfuse leave`.
//  3. The TLS directory, identity file, and known_devices directory are gone.
//  4. config.kdl is preserved.
//  5. Alice can rejoin with a fresh token using the same nickname.
func TestScenario_LeaveAndRejoin(t *testing.T) {
	hub := helpers.StartHub(t)
	alice := helpers.StartAgent(t, hub, "alice")
	alice.Join(t)

	hubfuseDir := filepath.Join(alice.HomeDir, ".hubfuse")

	// Verify pre-leave state.
	tlsDir := filepath.Join(hubfuseDir, common.TLSDir)
	identityPath := filepath.Join(hubfuseDir, common.IdentityFile)
	configPath := filepath.Join(hubfuseDir, common.ConfigFile)

	_, err := os.Stat(tlsDir)
	require.NoError(t, err, "tls dir must exist before leave")
	_, err = os.Stat(identityPath)
	require.NoError(t, err, "identity file must exist before leave")
	_, err = os.Stat(configPath)
	require.NoError(t, err, "config.kdl must exist before leave")

	// Alice leaves.
	alice.Leave(t)

	// TLS directory must be gone.
	_, err = os.Stat(tlsDir)
	assert.True(t, os.IsNotExist(err), "tls dir should be removed after leave")

	// Identity file must be gone.
	_, err = os.Stat(identityPath)
	assert.True(t, os.IsNotExist(err), "identity file should be removed after leave")

	// known_devices directory must be gone (may not have existed yet — that is ok).
	knownDevicesDir := filepath.Join(hubfuseDir, common.KnownDevicesDir)
	_, err = os.Stat(knownDevicesDir)
	assert.True(t, os.IsNotExist(err), "known_devices dir should be removed after leave")

	// config.kdl must still be present.
	data, err := os.ReadFile(configPath)
	require.NoError(t, err, "config.kdl should be preserved after leave")
	require.NotEmpty(t, data, "config.kdl should not be empty")

	// Alice rejoins with a fresh token and the same nickname.
	alice.Join(t)

	// Verify the new identity file exists.
	_, err = os.Stat(identityPath)
	require.NoError(t, err, "identity file should exist after rejoin")

	// Verify the new TLS material exists.
	_, err = os.Stat(tlsDir)
	require.NoError(t, err, "tls dir should exist after rejoin")
}
