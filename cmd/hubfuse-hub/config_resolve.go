package main

import (
	"fmt"
	"time"

	"github.com/ykhdr/hubfuse/internal/hub"
)

// resolveDeviceRetention determines the effective retention duration based on
// the CLI flag and optional config file. If the flag was explicitly set, it
// wins. Otherwise, a value from config.kdl (including zero) overrides the flag
// default.
func resolveDeviceRetention(flagValue string, flagChanged bool, configPath string) (time.Duration, error) {
	flagDuration, err := time.ParseDuration(flagValue)
	if err != nil {
		return 0, fmt.Errorf("parse device-retention flag: %w", err)
	}
	if flagDuration < 0 {
		return 0, fmt.Errorf("device-retention cannot be negative")
	}

	if flagChanged {
		return flagDuration, nil
	}

	cfg, err := hub.LoadHubConfigFile(configPath)
	if err != nil {
		return 0, err
	}
	if cfg.DeviceRetention != nil {
		if *cfg.DeviceRetention < 0 {
			return 0, fmt.Errorf("device-retention cannot be negative")
		}
		return *cfg.DeviceRetention, nil
	}

	return flagDuration, nil
}
