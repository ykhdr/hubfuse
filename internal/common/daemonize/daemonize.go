// Package daemonize provides primitives for turning a foreground process
// into a detached Unix daemon: self-respawn with Setsid, PID-file
// lifecycle, and log-output redirection. Unix-only for the Spawn path;
// helpers here are OS-portable.
package daemonize

import "os"

// EnvDaemonized is the environment variable the parent sets on the
// detached child before re-execing. A non-empty value "1" marks the
// process as the child so it does not try to re-daemonize.
const EnvDaemonized = "HUBFUSE_DAEMONIZED"

// IsChild reports whether the current process is the detached child
// spawned by Spawn. Only the exact value "1" is treated as truthy.
func IsChild() bool {
	return os.Getenv(EnvDaemonized) == "1"
}
