//go:build unix

package daemonize

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// SpawnOpts configures Spawn.
type SpawnOpts struct {
	// LogPath is the file the detached child's stdout and stderr are
	// appended to. Resolved to an absolute path before fork.
	LogPath string

	// PIDFilePath is the file whose appearance signals that the child
	// is ready to serve. Spawn polls for this file.
	PIDFilePath string

	// ReadyTimeout bounds how long Spawn waits for the PID file to
	// appear before giving up and killing the child. Defaults to 5s.
	ReadyTimeout time.Duration
}

// Spawn re-execs the current binary as a detached child and waits for
// it to either (a) write the PID file while still alive — success — or
// (b) exit — failure — or (c) exceed ReadyTimeout — failure.
//
// The child is started with HUBFUSE_DAEMONIZED=1 in its environment and
// Setsid=true on its SysProcAttr, so it becomes its own session leader
// and is detached from the caller's controlling terminal. Its stdin is
// wired to /dev/null; its stdout and stderr are appended to LogPath.
//
// Must only be called when IsChild() is false.
func Spawn(opts SpawnOpts) error {
	if IsChild() {
		return errors.New("daemonize.Spawn called from child process (refusing to recurse)")
	}
	if opts.ReadyTimeout <= 0 {
		opts.ReadyTimeout = 5 * time.Second
	}

	absLogPath, err := filepath.Abs(opts.LogPath)
	if err != nil {
		return fmt.Errorf("resolve log path %q: %w", opts.LogPath, err)
	}
	absPIDPath, err := filepath.Abs(opts.PIDFilePath)
	if err != nil {
		return fmt.Errorf("resolve pid path %q: %w", opts.PIDFilePath, err)
	}

	if err := os.MkdirAll(filepath.Dir(absLogPath), 0o755); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}
	logFile, err := os.OpenFile(absLogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open log %q: %w", absLogPath, err)
	}
	defer logFile.Close()

	devNull, err := os.OpenFile(os.DevNull, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("open /dev/null: %w", err)
	}
	defer devNull.Close()

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}

	childArgs := stripDaemonFlag(os.Args[1:])

	cmd := exec.Command(exe, childArgs...)
	cmd.Env = append(os.Environ(), EnvDaemonized+"=1")
	cmd.Stdin = devNull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}

	// Release the child: we don't want to reap it. Parent exits soon.
	// Keeping cmd.Wait available means we still notice early exit
	// during the readiness poll below; we do NOT Wait afterwards.
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	deadline := time.Now().Add(opts.ReadyTimeout)
	for {
		select {
		case werr := <-waitErr:
			tail := tailFile(absLogPath, 20)
			if werr != nil {
				return fmt.Errorf("daemon exited during startup: %v\n%s", werr, tail)
			}
			return fmt.Errorf("daemon exited during startup\n%s", tail)
		default:
		}

		pid, alive, err := CheckRunning(absPIDPath)
		if err != nil {
			return fmt.Errorf("check pidfile: %w", err)
		}
		if alive && pid == cmd.Process.Pid {
			fmt.Fprintf(os.Stdout, "started (pid %d, logs: %s)\n", pid, absLogPath)
			return nil
		}

		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			// Drain Wait so we don't leak the goroutine.
			<-waitErr
			return fmt.Errorf("daemon did not become ready within %s (check %s)", opts.ReadyTimeout, absLogPath)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// stripDaemonFlag removes every form of the --daemon / -d flag from
// argv so the re-exec'd child never recurses even if the IsChild guard
// were removed. Handles:
//
//	--daemon, --daemon=true, --daemon=false, -d
//
// Does NOT handle the space-separated form "--daemon true" because
// --daemon is a bool flag in cobra; that form is never emitted and
// users cannot produce it via normal CLI usage.
func stripDaemonFlag(argv []string) []string {
	out := argv[:0:0]
	for _, a := range argv {
		if a == "--daemon" || a == "-d" {
			continue
		}
		if strings.HasPrefix(a, "--daemon=") {
			continue
		}
		out = append(out, a)
	}
	return out
}

// tailMaxBytes caps how much of a log file tailFile reads from disk.
// Startup-failure logs are usually tiny, but a daemon sharing a long-
// lived log file across restarts can accumulate megabytes — we only
// need the last few lines, so bound the read.
const tailMaxBytes = 64 * 1024

// tailFile returns the last n lines of path, or a short diagnostic
// string if the file cannot be read. It reads at most tailMaxBytes
// from the end of the file to keep memory bounded regardless of log
// size.
func tailFile(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Sprintf("(log unavailable: %v)", err)
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return fmt.Sprintf("(log unavailable: %v)", err)
	}

	readFrom := int64(0)
	if stat.Size() > tailMaxBytes {
		readFrom = stat.Size() - tailMaxBytes
	}
	if _, err := f.Seek(readFrom, 0); err != nil {
		return fmt.Sprintf("(log unavailable: %v)", err)
	}

	data, err := io.ReadAll(f)
	if err != nil {
		return fmt.Sprintf("(log unavailable: %v)", err)
	}

	// If we truncated from the middle of a line, drop that partial
	// first line so we don't emit garbage.
	if readFrom > 0 {
		if i := strings.IndexByte(string(data), '\n'); i >= 0 {
			data = data[i+1:]
		}
	}

	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return "--- last " + fmt.Sprint(n) + " log lines ---\n" + strings.Join(lines, "\n")
}
