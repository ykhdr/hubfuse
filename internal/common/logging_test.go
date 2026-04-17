package common

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSetupLogger_DefaultConsole(t *testing.T) {
	logger, err := SetupLogger(LoggerOptions{})
	require.NoError(t, err)
	require.NotNil(t, logger, "SetupLogger returned nil")
	assert.True(t, logger.Enabled(context.TODO(), slog.LevelInfo), "Info should be enabled by default")
	assert.False(t, logger.Enabled(context.TODO(), slog.LevelDebug), "Debug should not be enabled by default")
}

func TestSetupLogger_Verbose(t *testing.T) {
	logger, err := SetupLogger(LoggerOptions{Verbose: true})
	require.NoError(t, err)
	assert.True(t, logger.Enabled(context.TODO(), slog.LevelDebug), "Debug should be enabled in verbose mode")
}

func TestSetupLogger_WithLogFile(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")

	logger, err := SetupLogger(LoggerOptions{
		LogFile:   logPath,
		FileLevel: slog.LevelDebug,
	})
	require.NoError(t, err)

	logger.Info("test message", "key", "value")

	raw, err := os.ReadFile(logPath)
	require.NoError(t, err)
	require.NotEmpty(t, raw, "log file is empty")

	var entry map[string]any
	require.NoError(t, json.Unmarshal(raw, &entry), "not valid JSON\nraw: %s", raw)
	assert.Equal(t, "test message", entry["msg"])
	assert.Equal(t, "value", entry["key"])
}

func TestSetupLogger_FileLevelFiltering(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "warn.log")

	logger, err := SetupLogger(LoggerOptions{
		LogFile:   logPath,
		FileLevel: slog.LevelWarn,
	})
	require.NoError(t, err)

	logger.Info("should not appear in file")
	logger.Warn("should appear in file")

	raw, err := os.ReadFile(logPath)
	require.NoError(t, err)

	content := string(raw)
	assert.NotContains(t, content, "should not appear", "info message should not be in warn-level file")

	var entry map[string]any
	require.NoError(t, json.Unmarshal(raw, &entry), "not valid JSON")
	assert.Equal(t, "should appear in file", entry["msg"])
}

func TestSetupLogger_InvalidFilePath(t *testing.T) {
	_, err := SetupLogger(LoggerOptions{
		LogFile: "/nonexistent/dir/app.log",
	})
	assert.Error(t, err, "expected error for unwritable path")
}

func TestSetupLogger_CreatesLogDir(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "subdir", "nested", "app.log")

	_, err := SetupLogger(LoggerOptions{LogFile: logPath})
	require.NoError(t, err)

	_, err = os.Stat(filepath.Dir(logPath))
	assert.False(t, os.IsNotExist(err), "expected log directory to be created")
}
