package scenarios_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ykhdr/hubfuse/tests/scenarios/helpers"
)

// TestPairAndMountBasic runs the full pair+mount flow with two real agent
// daemons and a stub sshfs. It exercises:
//   - Join + daemon startup on both agents
//   - RequestPairing CLI + ConfirmPairing via gRPC
//   - share add + mount add via CLI
//   - Hot-reload of config into the mounter
//   - Real SSH + sftp handshake (via stub-sshfs) against the exporter's SSH
//     server, using keys exchanged during pairing
func TestPairAndMountBasic(t *testing.T) {
	hub := helpers.StartHub(t)

	exportDir := t.TempDir()
	// Populate the export with a marker file so sftp ReadDir returns something observable.
	require.NoError(t, writeTestFile(exportDir, "hello.txt", "hello"), "seed export")

	alice := helpers.StartAgent(t, hub, "alice", helpers.WithExport(exportDir, "docs"))
	alice.Join(t)
	alice.StartDaemon(t)

	bob := helpers.StartAgent(t, hub, "bob")
	bob.Join(t)
	bob.StartDaemon(t)

	// Wait for both to appear as peers in each other's device list.
	require.Eventually(t, func() bool { return alice.HasPeer(t, "bob") && bob.HasPeer(t, "alice") },
		5*time.Second, 200*time.Millisecond, "hub should see both devices online")

	code := alice.RequestPairing(t, "bob")
	bob.ConfirmPairing(t, code)

	// Wait for the PairingCompleted event to propagate to alice's daemon via the
	// subscribe stream and for alice's SSH server to load bob's public key into
	// its allowed-key cache. Without this wait the stub-sshfs connection races
	// against alice's event processing and the SSH handshake fails.
	require.True(t, alice.WaitForPairedWith(t, 5*time.Second),
		"alice's daemon should have saved bob's public key before mounting")

	mountPoint := filepath.Join(t.TempDir(), "alice-docs")
	bob.Mount(t, "alice:docs", mountPoint)

	marker := helpers.ReadMarker(t, bob.MountMarker(mountPoint))
	require.Equal(t, "hubfuse", marker.RemoteUser, "sshfs always uses hubfuse@ as remote user")
	require.Equal(t, alice.SSHPort, marker.RemotePort, "sshfs -p should target alice's ssh-port")
	require.Equal(t, "docs", marker.RemotePath, "remote path is the share alias")
	require.Contains(t, marker.RemoteFiles, "hello.txt", "stub should have listed the seeded file via sftp")
}

// writeTestFile is a tiny helper to seed files into export directories.
func writeTestFile(dir, name, contents string) error {
	return os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o644)
}
