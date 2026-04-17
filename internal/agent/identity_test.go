package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─── GenerateDeviceID ─────────────────────────────────────────────────────────

func TestGenerateDeviceID_Format(t *testing.T) {
	id := GenerateDeviceID()
	require.NotEmpty(t, id, "GenerateDeviceID() returned empty string")

	// Must be parseable as a UUID v4.
	parsed, err := uuid.Parse(id)
	require.NoError(t, err, "GenerateDeviceID() = %q is not a valid UUID", id)
	assert.Equal(t, uuid.Version(4), parsed.Version(), "UUID version")
}

func TestGenerateDeviceID_Unique(t *testing.T) {
	ids := make(map[string]struct{})
	for i := 0; i < 10; i++ {
		id := GenerateDeviceID()
		_, seen := ids[id]
		require.False(t, seen, "GenerateDeviceID() returned duplicate ID: %q", id)
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

	require.NoError(t, SaveIdentity(path, original), "SaveIdentity()")

	loaded, err := LoadIdentity(path)
	require.NoError(t, err, "LoadIdentity()")

	assert.Equal(t, original.DeviceID, loaded.DeviceID, "DeviceID")
	assert.Equal(t, original.Nickname, loaded.Nickname, "Nickname")
}

func TestSaveIdentity_CreatesParentDirectories(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "subdir", "nested", "device.json")

	id := &DeviceIdentity{DeviceID: "abc", Nickname: "test"}
	require.NoError(t, SaveIdentity(path, id), "SaveIdentity() with nested path")

	_, err := os.Stat(path)
	require.NoError(t, err, "file not found after SaveIdentity")
}

func TestSaveIdentity_FilePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "device.json")

	id := &DeviceIdentity{DeviceID: "abc", Nickname: "test"}
	require.NoError(t, SaveIdentity(path, id), "SaveIdentity()")

	info, err := os.Stat(path)
	require.NoError(t, err, "Stat()")
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm(), "file permissions")
}

func TestLoadIdentity_NonExistentFile(t *testing.T) {
	_, err := LoadIdentity("/does/not/exist/device.json")
	assert.Error(t, err, "expected error for non-existent file")
}

func TestLoadIdentity_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "device.json")

	require.NoError(t, os.WriteFile(path, []byte("not valid json"), 0600), "WriteFile()")

	_, err := LoadIdentity(path)
	assert.Error(t, err, "expected error for invalid JSON")
}

func TestSaveLoadIdentity_WithGeneratedID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "device.json")

	deviceID := GenerateDeviceID()
	original := &DeviceIdentity{
		DeviceID: deviceID,
		Nickname: "generated-device",
	}

	require.NoError(t, SaveIdentity(path, original), "SaveIdentity()")

	loaded, err := LoadIdentity(path)
	require.NoError(t, err, "LoadIdentity()")

	assert.Equal(t, deviceID, loaded.DeviceID, "DeviceID")

	// Validate that the loaded ID is still a valid UUID.
	_, err = uuid.Parse(loaded.DeviceID)
	assert.NoError(t, err, "loaded DeviceID %q is not a valid UUID", loaded.DeviceID)
}
