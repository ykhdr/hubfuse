package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolveDeviceRetention_ConfigZeroOverridesDefault(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.kdl")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`device-retention "0s"`), 0o644), "write config")

	ret, err := resolveDeviceRetention("168h", false, cfgPath)
	require.NoError(t, err, "resolveDeviceRetention")
	assert.Equal(t, time.Duration(0), ret, "retention = %v, want 0", ret)
}

func TestResolveDeviceRetention_FlagBeatsConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.kdl")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`device-retention "0s"`), 0o644), "write config")

	ret, err := resolveDeviceRetention("24h", true, cfgPath)
	require.NoError(t, err, "resolveDeviceRetention")
	assert.Equal(t, 24*time.Hour, ret, "retention = %v, want 24h", ret)
}
