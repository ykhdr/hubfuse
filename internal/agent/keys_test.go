package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─── GenerateSSHKeyPair ───────────────────────────────────────────────────────

func TestGenerateSSHKeyPair_CreatesFiles(t *testing.T) {
	dir := t.TempDir()

	pubKey, err := GenerateSSHKeyPair(dir)
	require.NoError(t, err, "GenerateSSHKeyPair()")
	require.NotEmpty(t, pubKey, "GenerateSSHKeyPair() returned empty public key")

	privPath := filepath.Join(dir, "id_ed25519")
	pubPath := filepath.Join(dir, "id_ed25519.pub")

	_, err = os.Stat(privPath)
	assert.NoError(t, err, "private key file not found")
	_, err = os.Stat(pubPath)
	assert.NoError(t, err, "public key file not found")
}

func TestGenerateSSHKeyPair_PrivateKeyPermissions(t *testing.T) {
	dir := t.TempDir()

	_, err := GenerateSSHKeyPair(dir)
	require.NoError(t, err, "GenerateSSHKeyPair()")

	privPath := filepath.Join(dir, "id_ed25519")
	info, err := os.Stat(privPath)
	require.NoError(t, err, "Stat(private key)")
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm(), "private key permissions")
}

func TestGenerateSSHKeyPair_PublicKeyFormat(t *testing.T) {
	dir := t.TempDir()

	pubKey, err := GenerateSSHKeyPair(dir)
	require.NoError(t, err, "GenerateSSHKeyPair()")

	// OpenSSH ed25519 public keys start with "ssh-ed25519 ".
	assert.True(t, strings.HasPrefix(pubKey, "ssh-ed25519 "), "public key %q does not start with \"ssh-ed25519 \"", pubKey)
}

func TestGenerateSSHKeyPair_PublicKeyMatchesFile(t *testing.T) {
	dir := t.TempDir()

	pubKey, err := GenerateSSHKeyPair(dir)
	require.NoError(t, err, "GenerateSSHKeyPair()")

	// LoadPublicKey should return the same string.
	pubPath := filepath.Join(dir, "id_ed25519.pub")
	loaded, err := LoadPublicKey(pubPath)
	require.NoError(t, err, "LoadPublicKey()")

	assert.Equal(t, pubKey, loaded, "loaded public key != generated public key")
}

func TestGenerateSSHKeyPair_CreatesDirectory(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "ssh")

	_, err := GenerateSSHKeyPair(dir)
	require.NoError(t, err, "GenerateSSHKeyPair() with non-existent dir")

	_, err = os.Stat(dir)
	assert.NoError(t, err, "directory not created")
}

// ─── LoadPublicKey ────────────────────────────────────────────────────────────

func TestLoadPublicKey_Success(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.pub")

	content := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItest test-key"
	require.NoError(t, os.WriteFile(path, []byte(content+"\n"), 0644), "WriteFile()")

	got, err := LoadPublicKey(path)
	require.NoError(t, err, "LoadPublicKey()")
	assert.Equal(t, content, got)
}

func TestLoadPublicKey_NonExistentFile(t *testing.T) {
	_, err := LoadPublicKey("/does/not/exist/key.pub")
	assert.Error(t, err, "expected error for non-existent file")
}

// ─── SavePeerPublicKey / LoadPeerPublicKey ────────────────────────────────────

func TestSaveLoadPeerPublicKey_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	deviceID := "device-abc-123"
	pubKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItest test-key"

	require.NoError(t, SavePeerPublicKey(dir, deviceID, pubKey), "SavePeerPublicKey()")

	loaded, err := LoadPeerPublicKey(dir, deviceID)
	require.NoError(t, err, "LoadPeerPublicKey()")

	assert.Equal(t, pubKey, loaded)
}

func TestSaveLoadPeerPublicKey_FileNaming(t *testing.T) {
	dir := t.TempDir()
	deviceID := "my-device"
	pubKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItest key"

	require.NoError(t, SavePeerPublicKey(dir, deviceID, pubKey), "SavePeerPublicKey()")

	expectedPath := filepath.Join(dir, "my-device.pub")
	_, err := os.Stat(expectedPath)
	assert.NoError(t, err, "expected file %q not found", expectedPath)
}

func TestSaveLoadPeerPublicKey_CreatesDirectory(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "known_devices")

	require.NoError(t, SavePeerPublicKey(dir, "dev1", "ssh-ed25519 key"), "SavePeerPublicKey() with non-existent dir")

	_, err := os.Stat(dir)
	assert.NoError(t, err, "directory not created")
}

func TestLoadPeerPublicKey_NonExistentDevice(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadPeerPublicKey(dir, "no-such-device")
	assert.Error(t, err, "expected error for non-existent device key")
}

// ─── ListPairedDevices ────────────────────────────────────────────────────────

func TestListPairedDevices_Empty(t *testing.T) {
	dir := t.TempDir()
	devices, err := ListPairedDevices(dir)
	require.NoError(t, err, "ListPairedDevices() on empty dir")
	assert.Empty(t, devices)
}

func TestListPairedDevices_NonExistentDir(t *testing.T) {
	devices, err := ListPairedDevices("/does/not/exist")
	require.NoError(t, err, "ListPairedDevices() on non-existent dir")
	assert.Empty(t, devices)
}

func TestListPairedDevices_Multiple(t *testing.T) {
	dir := t.TempDir()
	deviceIDs := []string{"device-a", "device-b", "device-c"}

	for _, id := range deviceIDs {
		require.NoError(t, SavePeerPublicKey(dir, id, "ssh-ed25519 key"), "SavePeerPublicKey(%q)", id)
	}

	got, err := ListPairedDevices(dir)
	require.NoError(t, err, "ListPairedDevices()")

	assert.ElementsMatch(t, deviceIDs, got)
}

func TestListPairedDevices_IgnoresNonPubFiles(t *testing.T) {
	dir := t.TempDir()

	// Write a .pub file and a non-.pub file.
	require.NoError(t, SavePeerPublicKey(dir, "real-device", "ssh-ed25519 key"), "SavePeerPublicKey()")
	require.NoError(t, os.WriteFile(filepath.Join(dir, "notakey.txt"), []byte("data"), 0644), "WriteFile()")

	got, err := ListPairedDevices(dir)
	require.NoError(t, err, "ListPairedDevices()")

	assert.Len(t, got, 1)
	assert.Equal(t, "real-device", got[0])
}

func TestListPairedDevices_IgnoresSubdirectories(t *testing.T) {
	dir := t.TempDir()

	// Create a subdirectory with a .pub name.
	subdir := filepath.Join(dir, "fake.pub")
	require.NoError(t, os.Mkdir(subdir, 0755), "Mkdir()")

	// Also create a real key.
	require.NoError(t, SavePeerPublicKey(dir, "real-device", "ssh-ed25519 key"), "SavePeerPublicKey()")

	got, err := ListPairedDevices(dir)
	require.NoError(t, err, "ListPairedDevices()")

	assert.Len(t, got, 1)
	assert.Equal(t, "real-device", got[0])
}

// ─── Integration: full SSH key flow ──────────────────────────────────────────

func TestSSHKeyFlow_GenerateAndPair(t *testing.T) {
	keyDir := t.TempDir()
	knownDir := t.TempDir()

	// Generate key pair.
	pubKey, err := GenerateSSHKeyPair(keyDir)
	require.NoError(t, err, "GenerateSSHKeyPair()")

	// Simulate a peer saving our key.
	deviceID := "peer-device-42"
	require.NoError(t, SavePeerPublicKey(knownDir, deviceID, pubKey), "SavePeerPublicKey()")

	// Load it back.
	loaded, err := LoadPeerPublicKey(knownDir, deviceID)
	require.NoError(t, err, "LoadPeerPublicKey()")
	assert.Equal(t, pubKey, loaded)

	// Verify it appears in the list.
	paired, err := ListPairedDevices(knownDir)
	require.NoError(t, err, "ListPairedDevices()")
	assert.Len(t, paired, 1)
	assert.Equal(t, deviceID, paired[0])
}
