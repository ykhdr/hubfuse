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
	// Reject non-positive PIDs: on Unix, kill(0, sig) signals the entire
	// process group and kill(-1, sig) every process the caller can reach.
	// A corrupt pidfile must never escalate into either.
	if pid <= 0 {
		return fmt.Errorf("pidfile %q contains non-positive pid %d", pidPath, pid)
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
	if killErr := proc.Signal(syscall.SIGKILL); killErr != nil {
		// ESRCH here means the process exited between the wait loop and
		// the kill — treat as success rather than reporting a refusal.
		if errors.Is(killErr, syscall.ESRCH) || errors.Is(killErr, os.ErrProcessDone) {
			fmt.Printf("%s stopped (pid %d)\n", name, pid)
			return nil
		}
		return fmt.Errorf("send SIGKILL to %d: %w", pid, killErr)
	}

	if waitForExit(pid, stopKillTimeout) {
		fmt.Printf("%s stopped (pid %d)\n", name, pid)
		return nil
	}

	return fmt.Errorf("%s (pid %d) refused to exit after SIGKILL", name, pid)
}

// waitForExit polls the given pid with syscall.Kill(pid, 0) until the process
// is gone (ESRCH) or the deadline expires. Returns true if the process exited.
// Kill(pid, 0) does NOT dodge the zombie caveat — it returns nil for a zombie
// just as os.Process.Signal(0) would. This helper is correct only because
// `hubfuse-hub stop` is never the hub's parent: the zombie belongs to whoever
// IS the parent (init, once the daemonizing parent has gone), and it is that
// process's Wait that clears the pid — at which point this poll finally sees
// ESRCH. Reused from the parent of the signalled process, this would poll a
// zombie pid until the deadline and then wrongly report "refused to exit after
// SIGKILL" — a parent must call Wait, not poll Kill(pid, 0). The scenario-test
// harness IS a parent, which is why it uses cmd.Wait() instead (issue #75).
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
