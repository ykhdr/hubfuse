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
