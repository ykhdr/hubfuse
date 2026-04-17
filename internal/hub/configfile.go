package hub

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	kdl "github.com/sblinch/kdl-go"
	"github.com/sblinch/kdl-go/document"
)

// HubConfigFile represents options parsed from config.kdl in the hub data dir.
type HubConfigFile struct {
	DeviceRetention *time.Duration
}

// LoadHubConfigFile parses an optional KDL config file. Missing files return
// an empty config and a nil error.
func LoadHubConfigFile(path string) (HubConfigFile, error) {
	var cfg HubConfigFile

	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return cfg, nil
	}
	if err != nil {
		return cfg, fmt.Errorf("read hub config: %w", err)
	}

	doc, err := kdl.Parse(bytes.NewReader(data))
	if err != nil {
		return cfg, fmt.Errorf("parse hub config: %w", err)
	}

	for _, node := range doc.Nodes {
		if nodeName(node) != "device-retention" {
			continue
		}
		value := firstArgString(node)
		if value == "" {
			return cfg, fmt.Errorf("device-retention requires a duration argument")
		}
		dur, err := time.ParseDuration(value)
		if err != nil {
			return cfg, fmt.Errorf("parse device-retention: %w", err)
		}
		cfg.DeviceRetention = &dur
	}

	return cfg, nil
}

func nodeName(node *document.Node) string {
	if node.Name == nil {
		return ""
	}
	s, _ := node.Name.Value.(string)
	return s
}

func firstArgString(node *document.Node) string {
	if len(node.Arguments) == 0 {
		return ""
	}
	s, _ := node.Arguments[0].Value.(string)
	return s
}
