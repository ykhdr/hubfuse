package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestReloadSSHAllowedKeys_LoadsFromDisk verifies that reloadSSHAllowedKeys
// picks up every *.pub file under known_devices/ and installs them into the
// SSH server's allowed-key set, so freshly paired peers are authenticated
// without a daemon restart.
func TestReloadSSHAllowedKeys_LoadsFromDisk(t *testing.T) {
	d, dir := buildTestDaemon(t)

	knownDevicesDir := filepath.Join(dir, "known_devices")

	// Generate two peer key pairs and persist their public halves as if a
	// prior pairing had written them.
	peerADir := t.TempDir()
	signerA, pubA := generateTestKeyPair(t, peerADir)
	require.NoError(t, SavePeerPublicKey(knownDevicesDir, "peer-a", string(readPub(t, peerADir))),
		"SavePeerPublicKey peer-a")

	peerBDir := t.TempDir()
	_, pubB := generateTestKeyPair(t, peerBDir)
	require.NoError(t, SavePeerPublicKey(knownDevicesDir, "peer-b", string(readPub(t, peerBDir))),
		"SavePeerPublicKey peer-b")

	// Before reload: no keys are loaded, so both peers are denied.
	_, err := d.sshServer.publicKeyCallback(nil, pubA)
	require.Error(t, err, "peer-a should be denied before reload")

	d.reloadSSHAllowedKeys()

	// After reload: both peers authenticate successfully.
	permsA, err := d.sshServer.publicKeyCallback(nil, pubA)
	require.NoError(t, err, "peer-a should be allowed after reload")
	assert.NotNil(t, permsA, "peer-a permissions")

	permsB, err := d.sshServer.publicKeyCallback(nil, pubB)
	require.NoError(t, err, "peer-b should be allowed after reload")
	assert.NotNil(t, permsB, "peer-b permissions")

	// Silence unused linter for signerA (captured but not exercised —
	// retained for symmetry in case the test grows to dial the server).
	_ = signerA
}

// TestReloadSSHAllowedKeys_EmptyDir is safe on a fresh daemon with no peers.
func TestReloadSSHAllowedKeys_EmptyDir(t *testing.T) {
	d, _ := buildTestDaemon(t)

	// Should not panic or fail when known_devices is empty.
	d.reloadSSHAllowedKeys()

	peerDir := t.TempDir()
	_, pub := generateTestKeyPair(t, peerDir)
	_, err := d.sshServer.publicKeyCallback(nil, pub)
	assert.Error(t, err, "no peers paired: all keys should be denied")
}

// TestReloadSSHAllowedKeys_ReplacesPrevious verifies that a subsequent reload
// reflects the current on-disk state: a key removed from disk must no longer
// authenticate.
func TestReloadSSHAllowedKeys_ReplacesPrevious(t *testing.T) {
	d, dir := buildTestDaemon(t)
	knownDevicesDir := filepath.Join(dir, "known_devices")

	peerDir := t.TempDir()
	_, pub := generateTestKeyPair(t, peerDir)
	require.NoError(t, SavePeerPublicKey(knownDevicesDir, "peer", string(readPub(t, peerDir))),
		"SavePeerPublicKey")

	d.reloadSSHAllowedKeys()
	_, err := d.sshServer.publicKeyCallback(nil, pub)
	require.NoError(t, err, "peer allowed after first reload")

	// Remove the key file and reload; the peer must now be denied.
	require.NoError(t, os.Remove(filepath.Join(knownDevicesDir, "peer.pub")),
		"remove peer pub file")
	d.reloadSSHAllowedKeys()

	_, err = d.sshServer.publicKeyCallback(nil, pub)
	assert.Error(t, err, "peer should be denied after key removal + reload")
}

// readPub returns the raw contents of dir/id_ed25519.pub.
func readPub(t *testing.T, dir string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "id_ed25519.pub"))
	require.NoError(t, err, "read pub file")
	return data
}
