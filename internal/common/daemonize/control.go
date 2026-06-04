// Package daemonize control commands: helpers that implement the
// common "stop" and "status" CLI subcommands by inspecting a pidfile
// and signalling the recorded process.
package daemonize

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

const (
	stopGracefulTimeout = 10 * time.Second
	stopKillTimeout     = 3 * time.Second
	stopPollInterval    = 100 * time.Millisecond
)

// SignalStop reads a PID file, sends SIGTERM to the recorded process,
// waits for it to exit, and escalates to SIGKILL if the graceful
// deadline expires. Returns nil once the process is gone.
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
		// Process already gone — the caller's goal is achieved.
		if errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
			fmt.Printf("%s stopped (pid %d)\n", name, pid)
			return nil
		}
		return fmt.Errorf("send SIGTERM to %d: %w", pid, err)
	}
	fmt.Printf("sent SIGTERM to %s (pid %d)\n", name, pid)

	if waitForExit(pid, stopGracefulTimeout) {
		fmt.Printf("%s stopped (pid %d)\n", name, pid)
		return nil
	}

	fmt.Fprintf(os.Stderr, "%s did not exit after SIGTERM; escalating to SIGKILL (pid %d)\n", name, pid)
	_ = proc.Signal(syscall.SIGKILL)

	if waitForExit(pid, stopKillTimeout) {
		fmt.Printf("%s stopped (pid %d)\n", name, pid)
		return nil
	}

	return fmt.Errorf("%s (pid %d) refused to exit after SIGKILL", name, pid)
}

// waitForExit polls the given pid with syscall.Kill(pid, 0) until the process
// is gone (ESRCH) or the deadline expires. Returns true if the process exited.
// Using the raw syscall avoids the os.Process zombie caveat where Signal(0)
// returns nil until the caller has called Wait.
func waitForExit(pid int, deadline time.Duration) bool {
	end := time.Now().Add(deadline)
	for time.Now().Before(end) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return true
		}
		time.Sleep(stopPollInterval)
	}
	return false
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
