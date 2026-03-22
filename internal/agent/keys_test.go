package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ─── GenerateSSHKeyPair ───────────────────────────────────────────────────────

func TestGenerateSSHKeyPair_CreatesFiles(t *testing.T) {
	dir := t.TempDir()

	pubKey, err := GenerateSSHKeyPair(dir)
	if err != nil {
		t.Fatalf("GenerateSSHKeyPair(): %v", err)
	}

	if pubKey == "" {
		t.Fatal("GenerateSSHKeyPair() returned empty public key")
	}

	privPath := filepath.Join(dir, "id_ed25519")
	pubPath := filepath.Join(dir, "id_ed25519.pub")

	if _, err := os.Stat(privPath); err != nil {
		t.Errorf("private key file not found: %v", err)
	}
	if _, err := os.Stat(pubPath); err != nil {
		t.Errorf("public key file not found: %v", err)
	}
}

func TestGenerateSSHKeyPair_PrivateKeyPermissions(t *testing.T) {
	dir := t.TempDir()

	if _, err := GenerateSSHKeyPair(dir); err != nil {
		t.Fatalf("GenerateSSHKeyPair(): %v", err)
	}

	privPath := filepath.Join(dir, "id_ed25519")
	info, err := os.Stat(privPath)
	if err != nil {
		t.Fatalf("Stat(private key): %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("private key permissions = %o, want 0600", info.Mode().Perm())
	}
}

func TestGenerateSSHKeyPair_PublicKeyFormat(t *testing.T) {
	dir := t.TempDir()

	pubKey, err := GenerateSSHKeyPair(dir)
	if err != nil {
		t.Fatalf("GenerateSSHKeyPair(): %v", err)
	}

	// OpenSSH ed25519 public keys start with "ssh-ed25519 ".
	if !strings.HasPrefix(pubKey, "ssh-ed25519 ") {
		t.Errorf("public key %q does not start with \"ssh-ed25519 \"", pubKey)
	}
}

func TestGenerateSSHKeyPair_PublicKeyMatchesFile(t *testing.T) {
	dir := t.TempDir()

	pubKey, err := GenerateSSHKeyPair(dir)
	if err != nil {
		t.Fatalf("GenerateSSHKeyPair(): %v", err)
	}

	// LoadPublicKey should return the same string.
	pubPath := filepath.Join(dir, "id_ed25519.pub")
	loaded, err := LoadPublicKey(pubPath)
	if err != nil {
		t.Fatalf("LoadPublicKey(): %v", err)
	}

	if loaded != pubKey {
		t.Errorf("loaded public key %q != generated public key %q", loaded, pubKey)
	}
}

func TestGenerateSSHKeyPair_CreatesDirectory(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "ssh")

	if _, err := GenerateSSHKeyPair(dir); err != nil {
		t.Fatalf("GenerateSSHKeyPair() with non-existent dir: %v", err)
	}

	if _, err := os.Stat(dir); err != nil {
		t.Errorf("directory not created: %v", err)
	}
}

// ─── LoadPublicKey ────────────────────────────────────────────────────────────

func TestLoadPublicKey_Success(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.pub")

	content := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItest test-key"
	if err := os.WriteFile(path, []byte(content+"\n"), 0644); err != nil {
		t.Fatalf("WriteFile(): %v", err)
	}

	got, err := LoadPublicKey(path)
	if err != nil {
		t.Fatalf("LoadPublicKey(): %v", err)
	}
	if got != content {
		t.Errorf("LoadPublicKey() = %q, want %q", got, content)
	}
}

func TestLoadPublicKey_NonExistentFile(t *testing.T) {
	_, err := LoadPublicKey("/does/not/exist/key.pub")
	if err == nil {
		t.Fatal("expected error for non-existent file, got nil")
	}
}

// ─── SavePeerPublicKey / LoadPeerPublicKey ────────────────────────────────────

func TestSaveLoadPeerPublicKey_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	deviceID := "device-abc-123"
	pubKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItest test-key"

	if err := SavePeerPublicKey(dir, deviceID, pubKey); err != nil {
		t.Fatalf("SavePeerPublicKey(): %v", err)
	}

	loaded, err := LoadPeerPublicKey(dir, deviceID)
	if err != nil {
		t.Fatalf("LoadPeerPublicKey(): %v", err)
	}

	if loaded != pubKey {
		t.Errorf("loaded key %q != saved key %q", loaded, pubKey)
	}
}

func TestSaveLoadPeerPublicKey_FileNaming(t *testing.T) {
	dir := t.TempDir()
	deviceID := "my-device"
	pubKey := "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAItest key"

	if err := SavePeerPublicKey(dir, deviceID, pubKey); err != nil {
		t.Fatalf("SavePeerPublicKey(): %v", err)
	}

	expectedPath := filepath.Join(dir, "my-device.pub")
	if _, err := os.Stat(expectedPath); err != nil {
		t.Errorf("expected file %q not found: %v", expectedPath, err)
	}
}

func TestSaveLoadPeerPublicKey_CreatesDirectory(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "known_devices")

	if err := SavePeerPublicKey(dir, "dev1", "ssh-ed25519 key"); err != nil {
		t.Fatalf("SavePeerPublicKey() with non-existent dir: %v", err)
	}

	if _, err := os.Stat(dir); err != nil {
		t.Errorf("directory not created: %v", err)
	}
}

func TestLoadPeerPublicKey_NonExistentDevice(t *testing.T) {
	dir := t.TempDir()
	_, err := LoadPeerPublicKey(dir, "no-such-device")
	if err == nil {
		t.Fatal("expected error for non-existent device key, got nil")
	}
}

// ─── ListPairedDevices ────────────────────────────────────────────────────────

func TestListPairedDevices_Empty(t *testing.T) {
	dir := t.TempDir()
	devices, err := ListPairedDevices(dir)
	if err != nil {
		t.Fatalf("ListPairedDevices() on empty dir: %v", err)
	}
	if len(devices) != 0 {
		t.Errorf("ListPairedDevices() = %v, want empty", devices)
	}
}

func TestListPairedDevices_NonExistentDir(t *testing.T) {
	devices, err := ListPairedDevices("/does/not/exist")
	if err != nil {
		t.Fatalf("ListPairedDevices() on non-existent dir: %v", err)
	}
	if len(devices) != 0 {
		t.Errorf("ListPairedDevices() = %v, want empty", devices)
	}
}

func TestListPairedDevices_Multiple(t *testing.T) {
	dir := t.TempDir()
	deviceIDs := []string{"device-a", "device-b", "device-c"}

	for _, id := range deviceIDs {
		if err := SavePeerPublicKey(dir, id, "ssh-ed25519 key"); err != nil {
			t.Fatalf("SavePeerPublicKey(%q): %v", id, err)
		}
	}

	got, err := ListPairedDevices(dir)
	if err != nil {
		t.Fatalf("ListPairedDevices(): %v", err)
	}

	if len(got) != len(deviceIDs) {
		t.Fatalf("ListPairedDevices() returned %d devices, want %d", len(got), len(deviceIDs))
	}

	// Build a set for order-independent comparison.
	gotSet := make(map[string]struct{}, len(got))
	for _, id := range got {
		gotSet[id] = struct{}{}
	}
	for _, want := range deviceIDs {
		if _, ok := gotSet[want]; !ok {
			t.Errorf("device %q not found in ListPairedDevices() result %v", want, got)
		}
	}
}

func TestListPairedDevices_IgnoresNonPubFiles(t *testing.T) {
	dir := t.TempDir()

	// Write a .pub file and a non-.pub file.
	if err := SavePeerPublicKey(dir, "real-device", "ssh-ed25519 key"); err != nil {
		t.Fatalf("SavePeerPublicKey(): %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notakey.txt"), []byte("data"), 0644); err != nil {
		t.Fatalf("WriteFile(): %v", err)
	}

	got, err := ListPairedDevices(dir)
	if err != nil {
		t.Fatalf("ListPairedDevices(): %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("ListPairedDevices() = %v, want [real-device]", got)
	}
	if got[0] != "real-device" {
		t.Errorf("ListPairedDevices()[0] = %q, want \"real-device\"", got[0])
	}
}

func TestListPairedDevices_IgnoresSubdirectories(t *testing.T) {
	dir := t.TempDir()

	// Create a subdirectory with a .pub name.
	subdir := filepath.Join(dir, "fake.pub")
	if err := os.Mkdir(subdir, 0755); err != nil {
		t.Fatalf("Mkdir(): %v", err)
	}

	// Also create a real key.
	if err := SavePeerPublicKey(dir, "real-device", "ssh-ed25519 key"); err != nil {
		t.Fatalf("SavePeerPublicKey(): %v", err)
	}

	got, err := ListPairedDevices(dir)
	if err != nil {
		t.Fatalf("ListPairedDevices(): %v", err)
	}

	if len(got) != 1 {
		t.Fatalf("ListPairedDevices() = %v, want only [real-device]", got)
	}
	if got[0] != "real-device" {
		t.Errorf("ListPairedDevices()[0] = %q, want \"real-device\"", got[0])
	}
}

// ─── Integration: full SSH key flow ──────────────────────────────────────────

func TestSSHKeyFlow_GenerateAndPair(t *testing.T) {
	keyDir := t.TempDir()
	knownDir := t.TempDir()

	// Generate key pair.
	pubKey, err := GenerateSSHKeyPair(keyDir)
	if err != nil {
		t.Fatalf("GenerateSSHKeyPair(): %v", err)
	}

	// Simulate a peer saving our key.
	deviceID := "peer-device-42"
	if err := SavePeerPublicKey(knownDir, deviceID, pubKey); err != nil {
		t.Fatalf("SavePeerPublicKey(): %v", err)
	}

	// Load it back.
	loaded, err := LoadPeerPublicKey(knownDir, deviceID)
	if err != nil {
		t.Fatalf("LoadPeerPublicKey(): %v", err)
	}
	if loaded != pubKey {
		t.Errorf("loaded peer key %q != original %q", loaded, pubKey)
	}

	// Verify it appears in the list.
	paired, err := ListPairedDevices(knownDir)
	if err != nil {
		t.Fatalf("ListPairedDevices(): %v", err)
	}
	if len(paired) != 1 || paired[0] != deviceID {
		t.Errorf("ListPairedDevices() = %v, want [%s]", paired, deviceID)
	}
}
