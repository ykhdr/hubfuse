package helpers

import (
	"encoding/json"
	"os"
	"testing"
)

// StubMountMarker is the JSON structure written by stub-sshfs for each active mount.
type StubMountMarker struct {
	Src         string   `json:"src"`
	Dst         string   `json:"dst"`
	RemoteUser  string   `json:"remote_user"`
	RemoteHost  string   `json:"remote_host"`
	RemotePort  int      `json:"remote_port"`
	RemotePath  string   `json:"remote_path"`
	KeyPath     string   `json:"key_path"`
	RemoteFiles []string `json:"remote_files"`
	PID         int      `json:"pid"`
}

// TryReadMarker loads a marker JSON file written by stub-sshfs, returning
// ok=false when the file is missing, unreadable, or not (yet) valid JSON.
// stub-sshfs writes markers non-atomically, so pollers (require.Eventually
// loops waiting for a fresh mount to appear) must tolerate a transiently
// half-written file instead of fataling like ReadMarker does.
func TryReadMarker(path string) (StubMountMarker, bool) {
	var m StubMountMarker
	data, err := os.ReadFile(path)
	if err != nil {
		return m, false
	}
	if err := json.Unmarshal(data, &m); err != nil {
		return m, false
	}
	return m, true
}

// ReadMarker loads a marker JSON file written by stub-sshfs. Fatals on parse or read errors.
func ReadMarker(t *testing.T, path string) StubMountMarker {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read marker %s: %v", path, err)
	}
	var m StubMountMarker
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse marker %s: %v\n%s", path, err, data)
	}
	return m
}
