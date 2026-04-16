package hub

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadHubConfigFile_Missing(t *testing.T) {
	cfg, err := LoadHubConfigFile(filepath.Join(t.TempDir(), "config.kdl"))
	if err != nil {
		t.Fatalf("LoadHubConfigFile missing: %v", err)
	}
	if cfg.DeviceRetention != 0 {
		t.Fatalf("DeviceRetention = %v, want 0", cfg.DeviceRetention)
	}
}

func TestLoadHubConfigFile_DeviceRetention(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.kdl")
	if err := os.WriteFile(path, []byte(`device-retention "72h"`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadHubConfigFile(path)
	if err != nil {
		t.Fatalf("LoadHubConfigFile: %v", err)
	}
	if cfg.DeviceRetention != 72*time.Hour {
		t.Fatalf("DeviceRetention = %v, want 72h", cfg.DeviceRetention)
	}
}

func TestLoadHubConfigFile_InvalidDuration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.kdl")
	if err := os.WriteFile(path, []byte(`device-retention "notaduration"`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if _, err := LoadHubConfigFile(path); err == nil {
		t.Fatal("expected error for invalid duration, got nil")
	}
}
