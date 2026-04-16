package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	kdl "github.com/sblinch/kdl-go"
	"github.com/sblinch/kdl-go/document"
	"github.com/ykhdr/hubfuse/internal/common"
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

	doc, err := kdl.Parse(bytes.NewReader(data))
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

// ExpandTilde is a thin alias for common.ExpandHome kept for KDL layer callers.
func ExpandTilde(path string) string {
	return common.ExpandHome(path)
}

// Save serialises cfg to a KDL file at path, creating parent directories as
// needed. The format matches what Load expects.
func Save(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	var sb strings.Builder

	// device block.
	fmt.Fprintf(&sb, "device {\n")
	fmt.Fprintf(&sb, "    nickname %q\n", cfg.Device.Nickname)
	fmt.Fprintf(&sb, "}\n\n")

	// hub block.
	fmt.Fprintf(&sb, "hub {\n")
	fmt.Fprintf(&sb, "    address %q\n", cfg.Hub.Address)
	fmt.Fprintf(&sb, "}\n\n")

	// agent block.
	fmt.Fprintf(&sb, "agent {\n")
	fmt.Fprintf(&sb, "    ssh-port %d\n", cfg.Agent.SSHPort)
	fmt.Fprintf(&sb, "}\n\n")

	// shares block.
	if len(cfg.Shares) > 0 {
		fmt.Fprintf(&sb, "shares {\n")
		for _, s := range cfg.Shares {
			fmt.Fprintf(&sb, "    share %q alias=%q permissions=%q", s.Path, s.Alias, s.Permissions)
			if len(s.AllowedDevices) > 0 {
				fmt.Fprintf(&sb, " {\n")
				fmt.Fprintf(&sb, "        allowed-devices")
				for _, d := range s.AllowedDevices {
					fmt.Fprintf(&sb, " %q", d)
				}
				fmt.Fprintf(&sb, "\n")
				fmt.Fprintf(&sb, "    }\n")
			} else {
				fmt.Fprintf(&sb, "\n")
			}
		}
		fmt.Fprintf(&sb, "}\n\n")
	}

	// mounts block.
	if len(cfg.Mounts) > 0 {
		fmt.Fprintf(&sb, "mounts {\n")
		for _, m := range cfg.Mounts {
			fmt.Fprintf(&sb, "    mount device=%q share=%q to=%q\n", m.Device, m.Share, m.To)
		}
		fmt.Fprintf(&sb, "}\n")
	}

	if err := os.WriteFile(path, []byte(sb.String()), 0644); err != nil {
		return fmt.Errorf("write config %q: %w", path, err)
	}
	return nil
}
