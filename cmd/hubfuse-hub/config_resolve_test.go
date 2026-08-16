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

func TestResolveHeartbeatTimeout_DefaultWhenNothingSet(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.kdl") // no file on disk

	timeout, err := resolveHeartbeatTimeout("30s", false, cfgPath)
	require.NoError(t, err, "resolveHeartbeatTimeout")
	assert.Equal(t, 30*time.Second, timeout)
}

func TestResolveHeartbeatTimeout_ConfigOverridesDefault(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.kdl")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`heartbeat-timeout "90s"`), 0o644), "write config")

	timeout, err := resolveHeartbeatTimeout("30s", false, cfgPath)
	require.NoError(t, err, "resolveHeartbeatTimeout")
	assert.Equal(t, 90*time.Second, timeout)
}

func TestResolveHeartbeatTimeout_FlagBeatsConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.kdl")
	require.NoError(t, os.WriteFile(cfgPath, []byte(`heartbeat-timeout "90s"`), 0o644), "write config")

	timeout, err := resolveHeartbeatTimeout("5s", true, cfgPath)
	require.NoError(t, err, "resolveHeartbeatTimeout")
	assert.Equal(t, 5*time.Second, timeout)
}

func TestResolveHeartbeatTimeout_RejectsNegative(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "config.kdl")

	_, err := resolveHeartbeatTimeout("-5s", true, cfgPath)
	require.Error(t, err, "a negative timeout would demote every device on the first sweep")

	require.NoError(t, os.WriteFile(cfgPath, []byte(`heartbeat-timeout "-5s"`), 0o644), "write config")
	_, err = resolveHeartbeatTimeout("30s", false, cfgPath)
	require.Error(t, err, "the config path must reject a negative timeout too")
}
