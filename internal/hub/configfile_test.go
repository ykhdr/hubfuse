package hub

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadHubConfigFile_Missing(t *testing.T) {
	cfg, err := LoadHubConfigFile(filepath.Join(t.TempDir(), "config.kdl"))
	require.NoError(t, err, "LoadHubConfigFile missing")
	assert.Nil(t, cfg.DeviceRetention, "DeviceRetention = %v, want nil", cfg.DeviceRetention)
}

func TestLoadHubConfigFile_DeviceRetention(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.kdl")
	require.NoError(t, os.WriteFile(path, []byte(`device-retention "72h"`), 0o644), "write config")

	cfg, err := LoadHubConfigFile(path)
	require.NoError(t, err, "LoadHubConfigFile")
	require.NotNil(t, cfg.DeviceRetention, "DeviceRetention is nil, want 72h")
	assert.Equal(t, 72*time.Hour, *cfg.DeviceRetention, "DeviceRetention = %v, want 72h", cfg.DeviceRetention)
}

func TestLoadHubConfigFile_InvalidDuration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.kdl")
	require.NoError(t, os.WriteFile(path, []byte(`device-retention "notaduration"`), 0o644), "write config")

	_, err := LoadHubConfigFile(path)
	assert.Error(t, err, "expected error for invalid duration, got nil")
}

func TestLoadHubConfigFile_ZeroDuration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.kdl")
	require.NoError(t, os.WriteFile(path, []byte(`device-retention "0s"`), 0o644), "write config")

	cfg, err := LoadHubConfigFile(path)
	require.NoError(t, err, "LoadHubConfigFile")
	require.NotNil(t, cfg.DeviceRetention, "DeviceRetention is nil, want explicit zero")
	assert.Equal(t, time.Duration(0), *cfg.DeviceRetention, "DeviceRetention = %v, want 0", *cfg.DeviceRetention)
}
