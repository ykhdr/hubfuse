package common

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupLogger_DefaultConsole(t *testing.T) {
	logger, err := SetupLogger(LoggerOptions{})
	if err != nil {
		t.Fatalf("SetupLogger: %v", err)
	}
	if logger == nil {
		t.Fatal("SetupLogger returned nil")
	}
	if !logger.Enabled(context.TODO(), slog.LevelInfo) {
		t.Error("Info should be enabled by default")
	}
	if logger.Enabled(context.TODO(), slog.LevelDebug) {
		t.Error("Debug should not be enabled by default")
	}
}

func TestSetupLogger_Verbose(t *testing.T) {
	logger, err := SetupLogger(LoggerOptions{Verbose: true})
	if err != nil {
		t.Fatalf("SetupLogger: %v", err)
	}
	if !logger.Enabled(context.TODO(), slog.LevelDebug) {
		t.Error("Debug should be enabled in verbose mode")
	}
}

func TestSetupLogger_WithLogFile(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "test.log")

	logger, err := SetupLogger(LoggerOptions{
		LogFile:   logPath,
		FileLevel: slog.LevelDebug,
	})
	if err != nil {
		t.Fatalf("SetupLogger: %v", err)
	}

	logger.Info("test message", "key", "value")

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("log file is empty")
	}

	var entry map[string]any
	if err := json.Unmarshal(raw, &entry); err != nil {
		t.Fatalf("not valid JSON: %v\nraw: %s", err, raw)
	}
	if entry["msg"] != "test message" {
		t.Errorf("msg = %v, want %q", entry["msg"], "test message")
	}
	if entry["key"] != "value" {
		t.Errorf("key = %v, want %q", entry["key"], "value")
	}
}

func TestSetupLogger_FileLevelFiltering(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "warn.log")

	logger, err := SetupLogger(LoggerOptions{
		LogFile:   logPath,
		FileLevel: slog.LevelWarn,
	})
	if err != nil {
		t.Fatalf("SetupLogger: %v", err)
	}

	logger.Info("should not appear in file")
	logger.Warn("should appear in file")

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	content := string(raw)
	if strings.Contains(content, "should not appear") {
		t.Error("info message should not be in warn-level file")
	}

	var entry map[string]any
	if err := json.Unmarshal(raw, &entry); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if entry["msg"] != "should appear in file" {
		t.Errorf("unexpected msg: %v", entry["msg"])
	}
}

func TestSetupLogger_InvalidFilePath(t *testing.T) {
	_, err := SetupLogger(LoggerOptions{
		LogFile: "/nonexistent/dir/app.log",
	})
	if err == nil {
		t.Fatal("expected error for unwritable path")
	}
}

func TestSetupLogger_CreatesLogDir(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "subdir", "nested", "app.log")

	_, err := SetupLogger(LoggerOptions{LogFile: logPath})
	if err != nil {
		t.Fatalf("SetupLogger: %v", err)
	}

	if _, err := os.Stat(filepath.Dir(logPath)); os.IsNotExist(err) {
		t.Error("expected log directory to be created")
	}
}
