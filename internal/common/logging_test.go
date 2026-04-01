package common

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestSetupLogger_Levels(t *testing.T) {
	levels := []struct {
		name string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
	}

	for _, tc := range levels {
		t.Run(tc.name, func(t *testing.T) {
			logger, err := SetupLogger(tc.name, "stderr")
			if err != nil {
				t.Fatalf("SetupLogger(%q, stderr): %v", tc.name, err)
			}
			if logger == nil {
				t.Fatal("SetupLogger returned nil logger")
			}
			if !logger.Enabled(context.TODO(), tc.want) {
				t.Errorf("logger does not have level %v enabled", tc.want)
			}
		})
	}
}

func TestSetupLogger_InvalidLevel(t *testing.T) {
	_, err := SetupLogger("verbose", "stderr")
	if err == nil {
		t.Fatal("expected error for unsupported level, got nil")
	}
}

func TestSetupLogger_FileOutput(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "app.log")

	logger, err := SetupLogger("debug", logPath)
	if err != nil {
		t.Fatalf("SetupLogger(debug, %q): %v", logPath, err)
	}
	if logger == nil {
		t.Fatal("SetupLogger returned nil logger")
	}

	logger.Info("test message", "key", "value")

	// The JSON handler flushes synchronously — file should have content.
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", logPath, err)
	}
	if len(raw) == 0 {
		t.Fatal("log file is empty after writing a message")
	}

	// Verify the output is valid JSON with the expected fields.
	var entry map[string]any
	if err := json.Unmarshal(raw, &entry); err != nil {
		t.Fatalf("log line is not valid JSON: %v\nraw: %s", err, raw)
	}
	if entry["msg"] != "test message" {
		t.Errorf("msg = %v, want %q", entry["msg"], "test message")
	}
	if entry["key"] != "value" {
		t.Errorf("key = %v, want %q", entry["key"], "value")
	}
}

func TestSetupLogger_InvalidFilePath(t *testing.T) {
	_, err := SetupLogger("info", "/nonexistent/dir/app.log")
	if err == nil {
		t.Fatal("expected error for unwritable path, got nil")
	}
}

func TestSetupLogger_LevelFiltering(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "warn.log")

	logger, err := SetupLogger("warn", logPath)
	if err != nil {
		t.Fatalf("SetupLogger(): %v", err)
	}

	// Info messages should be dropped.
	logger.Info("this should not appear")
	// Warn messages should be kept.
	logger.Warn("this should appear")

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("ReadFile(): %v", err)
	}

	content := string(raw)
	if len(content) == 0 {
		t.Fatal("expected at least one line in log file")
	}

	// Only one JSON line should be present (the warn message).
	var entry map[string]any
	if err := json.Unmarshal(raw, &entry); err != nil {
		t.Fatalf("log line is not valid JSON: %v\nraw: %s", err, raw)
	}
	if entry["msg"] != "this should appear" {
		t.Errorf("unexpected log message: %v", entry["msg"])
	}
}
