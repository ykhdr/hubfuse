package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	kdl "github.com/sblinch/kdl-go"
	"github.com/sblinch/kdl-go/document"
)

// Config holds the full agent configuration.
type Config struct {
	Device DeviceConfig
	Hub    HubConfig
	Agent  AgentConfig
	Shares []ShareConfig
	Mounts []MountConfig
}

// DeviceConfig holds per-device identity settings.
type DeviceConfig struct {
	Nickname string
}

// HubConfig holds hub connection settings.
type HubConfig struct {
	Address string
}

// AgentConfig holds agent-level settings.
type AgentConfig struct {
	// SSHPort is the port the agent's SSH server listens on (default: 2222).
	SSHPort int
}

// ShareConfig describes a directory shared by this device.
type ShareConfig struct {
	Path           string
	Alias          string
	Permissions    string   // "ro" | "rw"
	AllowedDevices []string
}

// MountConfig describes a remote share to be mounted locally.
type MountConfig struct {
	Device string
	Share  string
	To     string
}

// DefaultConfig returns a Config with default values applied.
func DefaultConfig() *Config {
	return &Config{
		Agent: AgentConfig{
			SSHPort: 2222,
		},
	}
}

// Load reads and parses a KDL config file at path.
// Default values are applied before parsing, so any field omitted in the
// file retains its default.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("load config %q: %w", path, err)
	}

	doc, err := kdl.Parse(strings.NewReader(string(data)))
	if err != nil {
		return nil, fmt.Errorf("load config %q: failed to unmarshal KDL: %w", path, err)
	}

	cfg := DefaultConfig()

	for _, node := range doc.Nodes {
		switch nodeName(node) {
		case "device":
			cfg.Device = parseDeviceConfig(node)
		case "hub":
			cfg.Hub = parseHubConfig(node)
		case "agent":
			parseAgentConfig(node, &cfg.Agent)
		case "shares":
			cfg.Shares = parseSharesBlock(node)
		case "mounts":
			cfg.Mounts = parseMountsBlock(node)
		}
	}

	// Normalise permissions and expand tildes.
	for i := range cfg.Shares {
		cfg.Shares[i].Permissions = NormalizePermissions(cfg.Shares[i].Permissions)
		cfg.Shares[i].Path = ExpandTilde(cfg.Shares[i].Path)
	}
	for i := range cfg.Mounts {
		cfg.Mounts[i].To = ExpandTilde(cfg.Mounts[i].To)
	}

	return cfg, nil
}

// parseDeviceConfig extracts DeviceConfig from a "device { ... }" node.
func parseDeviceConfig(node *document.Node) DeviceConfig {
	var dc DeviceConfig
	for _, child := range node.Children {
		switch nodeName(child) {
		case "nickname":
			dc.Nickname = firstArgString(child)
		}
	}
	return dc
}

// parseHubConfig extracts HubConfig from a "hub { ... }" node.
func parseHubConfig(node *document.Node) HubConfig {
	var hc HubConfig
	for _, child := range node.Children {
		switch nodeName(child) {
		case "address":
			hc.Address = firstArgString(child)
		}
	}
	return hc
}

// parseAgentConfig fills ac from an "agent { ... }" node.
// Existing values in ac are only overwritten when the field is explicitly set.
func parseAgentConfig(node *document.Node, ac *AgentConfig) {
	for _, child := range node.Children {
		switch nodeName(child) {
		case "ssh-port":
			if v := firstArgInt(child); v != 0 {
				ac.SSHPort = v
			}
		}
	}
}

// parseSharesBlock extracts []ShareConfig from a "shares { share ... }" node.
func parseSharesBlock(node *document.Node) []ShareConfig {
	var shares []ShareConfig
	for _, child := range node.Children {
		if nodeName(child) != "share" {
			continue
		}
		sc := ShareConfig{
			Path:        firstArgString(child),
			Alias:       propString(child, "alias"),
			Permissions: propString(child, "permissions"),
		}
		// Parse allowed-devices grandchild.
		for _, gc := range child.Children {
			if nodeName(gc) == "allowed-devices" {
				for _, arg := range gc.Arguments {
					if s, ok := arg.Value.(string); ok {
						sc.AllowedDevices = append(sc.AllowedDevices, s)
					}
				}
			}
		}
		shares = append(shares, sc)
	}
	return shares
}

// parseMountsBlock extracts []MountConfig from a "mounts { mount ... }" node.
func parseMountsBlock(node *document.Node) []MountConfig {
	var mounts []MountConfig
	for _, child := range node.Children {
		if nodeName(child) != "mount" {
			continue
		}
		mc := MountConfig{
			Device: propString(child, "device"),
			Share:  propString(child, "share"),
			To:     propString(child, "to"),
		}
		mounts = append(mounts, mc)
	}
	return mounts
}

// nodeName returns the string name of a node, or "" if the name value is nil.
func nodeName(node *document.Node) string {
	if node.Name == nil {
		return ""
	}
	s, _ := node.Name.Value.(string)
	return s
}

// firstArgString returns the first argument of node as a string, or "".
func firstArgString(node *document.Node) string {
	if len(node.Arguments) == 0 {
		return ""
	}
	s, _ := node.Arguments[0].Value.(string)
	return s
}

// firstArgInt returns the first argument of node as an int, or 0.
func firstArgInt(node *document.Node) int {
	if len(node.Arguments) == 0 {
		return 0
	}
	switch v := node.Arguments[0].Value.(type) {
	case int64:
		return int(v)
	case int:
		return v
	}
	return 0
}

// propString returns the string value of the named property of node, or "".
func propString(node *document.Node, key string) string {
	val, ok := node.Properties[key]
	if !ok || val == nil {
		return ""
	}
	s, _ := val.Value.(string)
	return s
}

// NormalizePermissions converts verbose permission strings to their short form.
// "read-only" → "ro", "read-write" → "rw". "ro" and "rw" pass through unchanged.
func NormalizePermissions(perm string) string {
	switch perm {
	case "read-only":
		return "ro"
	case "read-write":
		return "rw"
	default:
		return perm
	}
}

// ExpandTilde replaces a leading "~" in path with the current user's home
// directory. If the home directory cannot be determined the path is returned
// unchanged.
func ExpandTilde(path string) string {
	if !strings.HasPrefix(path, "~") {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, path[1:])
}
