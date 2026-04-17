package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResolveDeviceRetention_ConfigZeroOverridesDefault(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.kdl")
	if err := os.WriteFile(cfgPath, []byte(`device-retention "0s"`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ret, err := resolveDeviceRetention("168h", false, cfgPath)
	if err != nil {
		t.Fatalf("resolveDeviceRetention: %v", err)
	}
	if ret != 0 {
		t.Fatalf("retention = %v, want 0", ret)
	}
}

func TestResolveDeviceRetention_FlagBeatsConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.kdl")
	if err := os.WriteFile(cfgPath, []byte(`device-retention "0s"`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ret, err := resolveDeviceRetention("24h", true, cfgPath)
	if err != nil {
		t.Fatalf("resolveDeviceRetention: %v", err)
	}
	if ret != 24*time.Hour {
		t.Fatalf("retention = %v, want 24h", ret)
	}
}
