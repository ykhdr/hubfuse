// Package daemonize provides primitives for turning a foreground process
// into a detached Unix daemon: self-respawn with Setsid, PID-file
// lifecycle, and log-output redirection. Unix-only for the Spawn path;
// helpers here are OS-portable.
package daemonize

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
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

// ReadPID reads an integer PID from a PID file. Trailing whitespace
// is trimmed. Returns a wrapped error that includes the path on any
// failure.
func ReadPID(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read pidfile %q: %w", path, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("parse pidfile %q: %w", path, err)
	}
	return pid, nil
}

// CheckRunning inspects path and reports whether it contains a live PID.
//
//   - If path does not exist → returns (0, false, nil).
//   - If path exists and the PID is alive → (pid, true, nil).
//   - If path exists but the PID is gone (stale, ESRCH) → the file is
//     removed and (0, false, nil) is returned.
//   - If the PID exists but we lack permission to signal it (EPERM, e.g.
//     the process runs as a different user) → treat it as alive and
//     return (pid, true, nil) without removing the file. Reporting
//     "stale" here would let a second instance start while the original
//     is still running.
//   - I/O problems (permission errors, malformed contents) surface as
//     non-nil err with (0, false, err).
//
// Liveness is probed with os.FindProcess followed by proc.Signal(0),
// which is the POSIX idiom for "does this process exist and can I send
// it signals?". On Unix, FindProcess never fails, so the Signal call is
// authoritative.
func CheckRunning(path string) (int, bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("read pidfile %q: %w", path, err)
	}

	trimmed := strings.TrimSpace(string(data))
	pid, err := strconv.Atoi(trimmed)
	if err != nil {
		return 0, false, fmt.Errorf("parse pidfile %q (contents %q): %w", path, trimmed, err)
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		// Not reachable on Unix (FindProcess always succeeds) but we
		// handle it defensively.
		_ = os.Remove(path)
		return 0, false, nil
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		// EPERM means the process exists but we can't signal it — it's
		// alive and the pidfile is NOT stale.
		if errors.Is(err, syscall.EPERM) {
			return pid, true, nil
		}
		// Anything else (typically ESRCH) means the process is gone;
		// clean up the stale pidfile so the next start can proceed.
		_ = os.Remove(path)
		return 0, false, nil
	}
	return pid, true, nil
}
