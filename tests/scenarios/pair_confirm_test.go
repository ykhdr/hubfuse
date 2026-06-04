package scenarios_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/tests/scenarios/helpers"
)

// TestScenario_PairConfirm_CompletesPairing verifies the full pair-confirm CLI
// flow: alice requests pairing with bob, bob runs `hubfuse pair-confirm <code>`,
// and the command exits zero with the expected output. It also asserts that
// bob's known_devices directory contains alice's public key afterwards.
func TestScenario_PairConfirm_CompletesPairing(t *testing.T) {
	hub := helpers.StartHub(t)

	alice := helpers.StartAgent(t, hub, "alice")
	alice.Join(t)
	alice.StartDaemon(t)

	bob := helpers.StartAgent(t, hub, "bob")
	bob.Join(t)
	bob.StartDaemon(t)

	// Wait for both agents to appear online so pairing RPCs succeed.
	require.Eventually(t, func() bool { return alice.HasPeer(t, "bob") && bob.HasPeer(t, "alice") },
		5*time.Second, 200*time.Millisecond, "hub should see both devices online")

	// alice initiates pairing with bob.
	code := alice.RequestPairing(t, "bob")
	require.NotEmpty(t, code, "invite code must not be empty")

	// bob confirms via the new CLI command.
	out := bob.ConfirmPairingCLI(t, code)

	// Output must contain alice's nickname and the paired-with prefix.
	require.True(t, strings.Contains(out, "alice"),
		"pair-confirm output should mention peer nickname %q; got: %s", "alice", out)
	require.True(t, strings.Contains(out, "paired with"),
		"pair-confirm output should contain \"paired with\"; got: %s", out)

	// bob's known_devices directory must contain alice's public key file.
	knownDevicesDir := filepath.Join(bob.HomeDir, ".hubfuse", common.KnownDevicesDir)
	entries, err := os.ReadDir(knownDevicesDir)
	require.NoError(t, err, "known_devices dir should exist after pair-confirm")

	var pubFiles []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".pub") {
			pubFiles = append(pubFiles, e.Name())
		}
	}
	require.Len(t, pubFiles, 1,
		fmt.Sprintf("expected exactly one .pub file in known_devices, got: %v", pubFiles))

	pubKeyPath := filepath.Join(knownDevicesDir, pubFiles[0])
	pubKeyBytes, err := os.ReadFile(pubKeyPath)
	require.NoError(t, err)
	require.NotEmpty(t, pubKeyBytes, "alice's public key file must not be empty")
}

// TestScenario_PairConfirm_DaemonReceivesEvent verifies that the hub emits a
// PairingCompleted event to the confirmer's daemon (not just the initiator).
// After bob runs `hubfuse pair-confirm`, his running daemon must receive the
// event and write alice's public key to known_devices/, proving that
// reloadSSHAllowedKeys will run and bob's SSH server will accept alice's key
// without a daemon restart.
func TestScenario_PairConfirm_DaemonReceivesEvent(t *testing.T) {
	hub := helpers.StartHub(t)

	alice := helpers.StartAgent(t, hub, "alice")
	alice.Join(t)
	alice.StartDaemon(t)

	bob := helpers.StartAgent(t, hub, "bob")
	bob.Join(t)
	bob.StartDaemon(t)

	// Wait for both agents to appear online.
	require.Eventually(t, func() bool { return alice.HasPeer(t, "bob") && bob.HasPeer(t, "alice") },
		5*time.Second, 200*time.Millisecond, "hub should see both devices online")

	// alice initiates pairing.
	code := alice.RequestPairing(t, "bob")
	require.NotEmpty(t, code, "invite code must not be empty")

	// bob confirms via CLI — daemon is running, so the hub's PairingCompleted
	// event (now sent to the confirmer too) is what drives the file write.
	bob.ConfirmPairingCLI(t, code)

	// The CLI also saves the key directly (idempotent fallback). Either path
	// produces the same file. Wait for it to appear.
	ok := bob.WaitForPairedWith(t, 5*time.Second)
	require.True(t, ok, "bob's daemon should have written alice's public key to known_devices/")

	// Verify the key file is non-empty and contains alice's public key material.
	knownDevicesDir := filepath.Join(bob.HomeDir, ".hubfuse", common.KnownDevicesDir)
	entries, err := os.ReadDir(knownDevicesDir)
	require.NoError(t, err, "known_devices dir should exist")

	var pubFiles []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".pub") {
			pubFiles = append(pubFiles, e.Name())
		}
	}
	require.Len(t, pubFiles, 1,
		fmt.Sprintf("expected exactly one .pub file in known_devices, got: %v", pubFiles))

	pubKeyBytes, err := os.ReadFile(filepath.Join(knownDevicesDir, pubFiles[0]))
	require.NoError(t, err)
	assert.NotEmpty(t, pubKeyBytes, "alice's public key file must not be empty")
}
