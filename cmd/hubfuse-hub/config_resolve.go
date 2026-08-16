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

// resolveHeartbeatTimeout determines how long a device may go without a
// heartbeat before the monitor demotes it. Same precedence as
// resolveDeviceRetention: an explicitly set flag wins, otherwise config.kdl,
// otherwise the flag default.
//
// Zero is accepted and means "use the hub default" (hub.DefaultHeartbeatTimeout
// — NewHeartbeatMonitor substitutes it). A negative value is rejected outright:
// it has no such meaning and would demote every device on the first sweep. (#69)
func resolveHeartbeatTimeout(flagValue string, flagChanged bool, configPath string) (time.Duration, error) {
	flagDuration, err := time.ParseDuration(flagValue)
	if err != nil {
		return 0, fmt.Errorf("parse heartbeat-timeout flag: %w", err)
	}
	if flagDuration < 0 {
		return 0, fmt.Errorf("heartbeat-timeout cannot be negative")
	}

	if flagChanged {
		return flagDuration, nil
	}

	cfg, err := hub.LoadHubConfigFile(configPath)
	if err != nil {
		return 0, err
	}
	if cfg.HeartbeatTimeout != nil {
		if *cfg.HeartbeatTimeout < 0 {
			return 0, fmt.Errorf("heartbeat-timeout cannot be negative")
		}
		return *cfg.HeartbeatTimeout, nil
	}

	return flagDuration, nil
}

// resolveJoinTokenTTL determines the effective join-token TTL from the config
// file only (no CLI flag). Returns 10 minutes when the config file does not
// set the value.
func resolveJoinTokenTTL(configPath string) (time.Duration, error) {
	const defaultTTL = 10 * time.Minute

	cfg, err := hub.LoadHubConfigFile(configPath)
	if err != nil {
		return 0, err
	}
	if cfg.JoinTokenTTL != nil {
		if *cfg.JoinTokenTTL <= 0 {
			return 0, fmt.Errorf("join-token-ttl must be positive")
		}
		return *cfg.JoinTokenTTL, nil
	}

	return defaultTTL, nil
}
