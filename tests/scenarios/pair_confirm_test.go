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

// TestScenario_PairConfirm_InvalidCode verifies that the hub's Success=false
// response on an invalid invite code surfaces as a non-zero CLI exit instead
// of a misleading 'paired with ""' line. This pins the fix where
// HubClient.ConfirmPairing now translates resp.Success=false into a real Go
// error instead of returning ("", "", "", nil) and pretending pairing
// succeeded.
func TestScenario_PairConfirm_InvalidCode(t *testing.T) {
	hub := helpers.StartHub(t)

	alice := helpers.StartAgent(t, hub, "alice")
	alice.Join(t)

	out, ok := alice.TryRun(t, "pair-confirm", "HUB-NOPE-XYZ")
	require.False(t, ok, "pair-confirm with invalid code must exit non-zero; output: %s", out)
	require.True(t,
		strings.Contains(strings.ToLower(out), "invalid") ||
			strings.Contains(strings.ToLower(out), "invite"),
		"output should mention invalid/invite (the friendly clierrors translation); got: %s", out)
	require.False(t, strings.Contains(out, "paired with"),
		"output must not claim 'paired with' on failure; got: %s", out)
}

// TestScenario_PairConfirm_DaemonReloadsSSHAllowedKeys validates the live
// reload of the confirmer-side SSH allowed-key cache after pair-confirm.
// Previously this test only asserted that bob's known_devices/ contained
// alice's public key — but `hubfuse pair-confirm` also writes that file
// directly, so a passing assertion did NOT prove the hub's PairingCompleted
// event reached bob's daemon. Now we drive a real SSH+SFTP handshake from
// alice into bob's exported share: that handshake can only succeed if bob's
// daemon processed PairingCompleted and called reloadSSHAllowedKeys, because
// the in-memory deviceIDByFingerprint map is populated from that path — not
// from the on-disk file the CLI wrote.
func TestScenario_PairConfirm_DaemonReloadsSSHAllowedKeys(t *testing.T) {
	hub := helpers.StartHub(t)

	exportDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(exportDir, "secret.txt"), []byte("ok"), 0o644),
		"seed bob's export")

	// bob exports `notes` so alice has something to mount. The mount triggers a
	// real SSH+SFTP handshake against bob's daemon-hosted SSH server.
	alice := helpers.StartAgent(t, hub, "alice")
	alice.Join(t)
	alice.StartDaemon(t)

	bob := helpers.StartAgent(t, hub, "bob", helpers.WithExport(exportDir, "notes"))
	bob.Join(t)
	bob.StartDaemon(t)

	require.Eventually(t, func() bool { return alice.HasPeer(t, "bob") && bob.HasPeer(t, "alice") },
		5*time.Second, 200*time.Millisecond, "hub should see both devices online")

	// alice initiates pairing, bob confirms via the new CLI. Only the hub's
	// confirmer-side PairingCompleted event makes bob's in-memory SSH allowed
	// keys map include alice — the file-write done by the CLI is invisible to
	// the running daemon's cache.
	code := alice.RequestPairing(t, "bob")
	require.NotEmpty(t, code, "invite code must not be empty")
	bob.ConfirmPairingCLI(t, code)

	// Both sides need the pairing to settle: bob to authenticate alice's
	// incoming SSH (confirmer-side event), alice to know bob's key for the
	// mounter's SSH client (initiator-side event).
	require.True(t, alice.WaitForPairedWith(t, 5*time.Second),
		"alice's daemon should have processed PairingCompleted for the mounter side")
	require.True(t, bob.WaitForPairedWith(t, 5*time.Second),
		"bob's daemon should have processed PairingCompleted for the SSH server side")

	mountPoint := filepath.Join(t.TempDir(), "alice-notes")
	alice.Mount(t, "bob:notes", mountPoint)

	marker := helpers.ReadMarker(t, alice.MountMarker(mountPoint))
	assert.Equal(t, bob.SSHPort, marker.RemotePort, "alice must reach bob's SSH port")
	assert.Equal(t, "notes", marker.RemotePath, "remote path is the share alias")
	assert.Contains(t, marker.RemoteFiles, "secret.txt",
		"stub-sshfs lists files via real sftp — empty list means the SSH handshake never authenticated alice's key")
}
