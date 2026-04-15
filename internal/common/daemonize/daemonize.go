// Package daemonize provides primitives for turning a foreground process
// into a detached Unix daemon: self-respawn with Setsid, PID-file
// lifecycle, and log-output redirection. Unix-only for the Spawn path;
// helpers here are OS-portable.
package daemonize

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// EnvDaemonized is the environment variable the parent sets on the
// detached child before re-execing. A non-empty value "1" marks the
// process as the child so it does not try to re-daemonize.
const EnvDaemonized = "HUBFUSE_DAEMONIZED"

// IsChild reports whether the current process is the detached child
// spawned by Spawn. Only the exact value "1" is treated as truthy.
func IsChild() bool {
	return os.Getenv(EnvDaemonized) == "1"
}

// WritePIDFile atomically writes os.Getpid() to path. It writes to a
// sibling tmp file first and renames on success, so readers never see a
// half-written PID. The file mode is 0644 and parent directories must
// already exist.
func WritePIDFile(path string) error {
	pid := strconv.Itoa(os.Getpid())

	tmp, err := os.CreateTemp(filepath.Dir(path), ".pid-*")
	if err != nil {
		return fmt.Errorf("create tmp pidfile: %w", err)
	}
	tmpName := tmp.Name()

	cleanup := func() { _ = os.Remove(tmpName) }

	if _, err := tmp.WriteString(pid + "\n"); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("write tmp pidfile: %w", err)
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		cleanup()
		return fmt.Errorf("chmod tmp pidfile: %w", err)
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return fmt.Errorf("close tmp pidfile: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return fmt.Errorf("rename tmp pidfile to %q: %w", path, err)
	}
	return nil
}
