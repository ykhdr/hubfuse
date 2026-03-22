package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

// DeviceIdentity holds the persistent identity of this device.
type DeviceIdentity struct {
	DeviceID string `json:"device_id"`
	Nickname string `json:"nickname"`
}

// LoadIdentity reads a DeviceIdentity from the JSON file at path.
func LoadIdentity(path string) (*DeviceIdentity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read identity file %q: %w", path, err)
	}

	var id DeviceIdentity
	if err := json.Unmarshal(data, &id); err != nil {
		return nil, fmt.Errorf("parse identity file %q: %w", path, err)
	}

	return &id, nil
}

// SaveIdentity writes id as JSON to path, creating parent directories as needed.
// The file is written with 0600 permissions to protect the identity data.
func SaveIdentity(path string, id *DeviceIdentity) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create identity directory: %w", err)
	}

	data, err := json.MarshalIndent(id, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal identity: %w", err)
	}

	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write identity file %q: %w", path, err)
	}

	return nil
}

// GenerateDeviceID returns a new random UUID v4 string.
func GenerateDeviceID() string {
	return uuid.New().String()
}
