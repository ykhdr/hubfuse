// Package daemonize control commands: helpers that implement the
// common "stop" and "status" CLI subcommands by inspecting a pidfile
// and signalling the recorded process.
package daemonize

import (
	"fmt"
	"os"
	"syscall"
)

// SignalStop reads a PID file and sends SIGTERM to the recorded
// process. On success it prints "sent SIGTERM to <name> (pid N)" to
// stdout and returns nil. `name` is the process label shown in
// messages (e.g. "agent" or "hub").
func SignalStop(pidPath, name string) error {
	pid, err := ReadPID(pidPath)
	if err != nil {
		return err
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process %d: %w", pid, err)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("send SIGTERM to %d: %w", pid, err)
	}
	fmt.Printf("sent SIGTERM to %s (pid %d)\n", name, pid)
	return nil
}

// ReportStatus prints a human-readable liveness report for the
// process recorded in pidPath. `name` is the process label. Returns
// nil unconditionally — status is informational. Absence or
// staleness of the pidfile is surfaced as a "not running" line.
func ReportStatus(pidPath, name string) error {
	pid, alive, err := CheckRunning(pidPath)
	if err != nil {
		return fmt.Errorf("inspect pidfile: %w", err)
	}
	if !alive {
		fmt.Printf("%s is not running\n", name)
		return nil
	}
	fmt.Printf("%s is running (pid %d)\n", name, pid)
	return nil
}
