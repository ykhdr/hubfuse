package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

// ─── GenerateDeviceID ─────────────────────────────────────────────────────────

func TestGenerateDeviceID_Format(t *testing.T) {
	id := GenerateDeviceID()
	if id == "" {
		t.Fatal("GenerateDeviceID() returned empty string")
	}

	// Must be parseable as a UUID v4.
	parsed, err := uuid.Parse(id)
	if err != nil {
		t.Fatalf("GenerateDeviceID() = %q is not a valid UUID: %v", id, err)
	}
	if parsed.Version() != 4 {
		t.Errorf("UUID version = %d, want 4", parsed.Version())
	}
}

func TestGenerateDeviceID_Unique(t *testing.T) {
	ids := make(map[string]struct{})
	for i := 0; i < 10; i++ {
		id := GenerateDeviceID()
		if _, seen := ids[id]; seen {
			t.Fatalf("GenerateDeviceID() returned duplicate ID: %q", id)
		}
		ids[id] = struct{}{}
	}
}

// ─── SaveIdentity / LoadIdentity ──────────────────────────────────────────────

func TestSaveLoadIdentity_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "device.json")

	original := &DeviceIdentity{
		DeviceID: "test-device-id-123",
		Nickname: "my-laptop",
	}

	if err := SaveIdentity(path, original); err != nil {
		t.Fatalf("SaveIdentity(): %v", err)
	}

	loaded, err := LoadIdentity(path)
	if err != nil {
		t.Fatalf("LoadIdentity(): %v", err)
	}

	if loaded.DeviceID != original.DeviceID {
		t.Errorf("DeviceID = %q, want %q", loaded.DeviceID, original.DeviceID)
	}
	if loaded.Nickname != original.Nickname {
		t.Errorf("Nickname = %q, want %q", loaded.Nickname, original.Nickname)
	}
}

func TestSaveIdentity_CreatesParentDirectories(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "subdir", "nested", "device.json")

	id := &DeviceIdentity{DeviceID: "abc", Nickname: "test"}
	if err := SaveIdentity(path, id); err != nil {
		t.Fatalf("SaveIdentity() with nested path: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file not found after SaveIdentity: %v", err)
	}
}

func TestSaveIdentity_FilePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "device.json")

	id := &DeviceIdentity{DeviceID: "abc", Nickname: "test"}
	if err := SaveIdentity(path, id); err != nil {
		t.Fatalf("SaveIdentity(): %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(): %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Errorf("file permissions = %o, want 0600", info.Mode().Perm())
	}
}

func TestLoadIdentity_NonExistentFile(t *testing.T) {
	_, err := LoadIdentity("/does/not/exist/device.json")
	if err == nil {
		t.Fatal("expected error for non-existent file, got nil")
	}
}

func TestLoadIdentity_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "device.json")

	if err := os.WriteFile(path, []byte("not valid json"), 0600); err != nil {
		t.Fatalf("WriteFile(): %v", err)
	}

	_, err := LoadIdentity(path)
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestSaveLoadIdentity_WithGeneratedID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "device.json")

	deviceID := GenerateDeviceID()
	original := &DeviceIdentity{
		DeviceID: deviceID,
		Nickname: "generated-device",
	}

	if err := SaveIdentity(path, original); err != nil {
		t.Fatalf("SaveIdentity(): %v", err)
	}

	loaded, err := LoadIdentity(path)
	if err != nil {
		t.Fatalf("LoadIdentity(): %v", err)
	}

	if loaded.DeviceID != deviceID {
		t.Errorf("DeviceID = %q, want %q", loaded.DeviceID, deviceID)
	}

	// Validate that the loaded ID is still a valid UUID.
	if _, err := uuid.Parse(loaded.DeviceID); err != nil {
		t.Errorf("loaded DeviceID %q is not a valid UUID: %v", loaded.DeviceID, err)
	}
}
