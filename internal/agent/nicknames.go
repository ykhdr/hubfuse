package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const nicknamesFile = "nicknames.json"

// LoadNicknames reads the device_id → nickname map from
// <knownDevicesDir>/nicknames.json.  An absent file is not an error — it
// simply means no nicknames have been persisted yet; the caller receives an
// empty (non-nil) map and a nil error.
func LoadNicknames(knownDevicesDir string) (map[string]string, error) {
	path := filepath.Join(knownDevicesDir, nicknamesFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return make(map[string]string), nil
		}
		return nil, fmt.Errorf("read nicknames file %q: %w", path, err)
	}

	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse nicknames file %q: %w", path, err)
	}
	if m == nil {
		m = make(map[string]string)
	}

	// Validate every key before returning; reject any that would be unsafe
	// to use as a device identity.
	for id := range m {
		if err := validateDeviceID(id); err != nil {
			return nil, fmt.Errorf("nicknames file %q: invalid device_id key: %w", path, err)
		}
	}

	return m, nil
}

// SaveNicknames atomically writes the device_id → nickname map to
// <knownDevicesDir>/nicknames.json using a temp-file + rename so a crash
// during the write never leaves a truncated file.  Each device_id key is
// validated before the write; the function returns an error if any key is
// invalid.  The directory is created if it does not yet exist.
func SaveNicknames(knownDevicesDir string, m map[string]string) error {
	for id := range m {
		if err := validateDeviceID(id); err != nil {
			return fmt.Errorf("SaveNicknames: invalid device_id key: %w", err)
		}
	}

	if err := os.MkdirAll(knownDevicesDir, 0700); err != nil {
		return fmt.Errorf("create known devices directory %q: %w", knownDevicesDir, err)
	}

	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal nicknames: %w", err)
	}
	data = append(data, '\n')

	// Write to a temp file in the same directory so the rename is atomic on
	// POSIX systems (same filesystem).
	tmp, err := os.CreateTemp(knownDevicesDir, ".nicknames-*.json.tmp")
	if err != nil {
		return fmt.Errorf("create temp nicknames file: %w", err)
	}
	tmpName := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write temp nicknames file: %w", err)
	}
	if err := tmp.Chmod(0644); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("chmod temp nicknames file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close temp nicknames file: %w", err)
	}

	dest := filepath.Join(knownDevicesDir, nicknamesFile)
	if err := os.Rename(tmpName, dest); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("rename temp nicknames file to %q: %w", dest, err)
	}

	return nil
}

// SetNickname is a load-modify-save convenience used by the CLI
// (pair-confirm) to record a peer's nickname alongside its public key.
// It is a no-op when nickname is empty.  Callers that hold an in-memory
// daemon map should use (*Daemon).rememberNickname instead so that
// concurrent updates are not lost.
func SetNickname(knownDevicesDir, deviceID, nickname string) error {
	if nickname == "" {
		return nil
	}
	if err := validateDeviceID(deviceID); err != nil {
		return err
	}

	m, err := LoadNicknames(knownDevicesDir)
	if err != nil {
		return fmt.Errorf("SetNickname: load: %w", err)
	}

	m[deviceID] = nickname

	if err := SaveNicknames(knownDevicesDir, m); err != nil {
		return fmt.Errorf("SetNickname: save: %w", err)
	}

	return nil
}
