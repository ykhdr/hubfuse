// Package common paths: canonical filenames and default data
// directories used across hubfuse binaries. Every on-disk name the
// project currently hardcodes as a string literal should live here.
package common

import (
	"os"
	"path/filepath"
)

// Default per-binary data directories, expanded via ExpandHome.
const (
	HubDataDir   = "~/.hubfuse-hub"
	AgentDataDir = "~/.hubfuse"
)

// Subdirectories inside a data dir.
const (
	TLSDir          = "tls"
	KeysDir         = "keys"
	KnownDevicesDir = "known_devices"
	KnownHostsDir   = "known_hosts"
)

// Files inside a data dir.
const (
	IdentityFile = "device.json"
	ConfigFile   = "config.kdl"
	DBFile       = "hubfuse.db"
	HubPIDFile   = "hubfuse-hub.pid"
	AgentPIDFile = "hubfuse.pid"
	HubLogFile   = "hub.log"
	AgentLogFile = "agent.log"
)

// Files inside <dataDir>/tls.
const (
	CACertFile     = "ca.crt"
	CAKeyFile      = "ca.key"
	ServerCertFile = "server.crt"
	ServerKeyFile  = "server.key"
	ClientCertFile = "client.crt"
	ClientKeyFile  = "client.key"
)

// Files inside <dataDir>/keys.
const (
	PrivateKeyFile = "id_ed25519"
	PublicKeyFile  = "id_ed25519.pub"
)

// ExpandHome replaces a leading "~" with the user's home directory.
// Returns path unchanged if it doesn't start with "~" or if the
// home directory cannot be determined.
func ExpandHome(path string) string {
	if len(path) == 0 || path[0] != '~' {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, path[1:])
}
