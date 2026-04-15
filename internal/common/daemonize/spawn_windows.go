//go:build windows

package daemonize

import (
	"errors"
	"time"
)

// SpawnOpts configures Spawn. See the unix implementation for field
// semantics. The windows stub ignores the struct and always errors.
type SpawnOpts struct {
	LogPath      string
	PIDFilePath  string
	ReadyTimeout time.Duration
}

// Spawn returns an error on Windows: the daemon path relies on POSIX
// Setsid semantics that have no Windows equivalent worth shipping.
// Users on Windows can run the binary under a Windows service manager
// or skip --daemon entirely.
func Spawn(SpawnOpts) error {
	return errors.New("--daemon is not supported on windows")
}
