package helpers

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ykhdr/hubfuse/internal/hub"
)

// stopCleanupMargin pads hub.DefaultShutdownBudget for Stop's cleanup wait,
// not a second hardcoded absolute duration: the wait tracks whatever the
// budget is, plus enough slack to absorb process-scheduling and signal-
// delivery jitter around it, rather than sitting at a fixed number that can
// silently fall below the budget again (issue #75: the original 5s wait was
// itself below the 6s budget it was meant to be watching).
const stopCleanupMargin = 4 * time.Second

// HubBinaryPath is set by tests/scenarios/main_test.go before any test runs.
var HubBinaryPath string

// AgentBinaryPath will be populated by main_test.go as well; declared here so
// the package compiles in Task 4 even though its consumers arrive in Task 5.
var AgentBinaryPath string

// StubSSHFSBinaryPath points to the test-only sshfs replacement. Tests that
// trigger a mount should prepend filepath.Dir(StubSSHFSBinaryPath) to PATH so
// the agent's mounter invokes the stub instead of any real sshfs on the host.
var StubSSHFSBinaryPath string

type Hub struct {
	Address string // "127.0.0.1:NNNN"
	DataDir string

	port   int
	cmd    *exec.Cmd
	logBuf *LogBuffer
	cancel context.CancelFunc
}

// HubOption tunes the hub process a scenario starts. Options translate to
// `hubfuse-hub start` flags, so a scenario expresses timing requirements
// (retention, liveness timeout) instead of duplicating the launch code.
type HubOption func(*[]string)

// WithRetention prunes offline devices older than the given duration. Use a
// short duration (e.g. 5s) so pruning is observable within CI timelines.
func WithRetention(retention time.Duration) HubOption {
	return func(args *[]string) {
		*args = append(*args, "--device-retention", retention.String())
	}
}

// WithHeartbeatTimeout shortens how long a device may go without a heartbeat
// before the hub demotes it. The production default is 30s, which no scenario
// can afford to wait out several times; pair it with the agent's
// HUBFUSE_HEARTBEAT_INTERVAL so agents beat well inside the window. (#69)
func WithHeartbeatTimeout(timeout time.Duration) HubOption {
	return func(args *[]string) {
		*args = append(*args, "--heartbeat-timeout", timeout.String())
	}
}

// StartHub launches a hub in a temp data dir and returns once it is listening.
func StartHub(t *testing.T, opts ...HubOption) *Hub {
	t.Helper()
	port := FreePort(t)
	dataDir := t.TempDir()

	args := []string{"start",
		"--listen", fmt.Sprintf("127.0.0.1:%d", port),
		"--data-dir", dataDir,
	}
	for _, o := range opts {
		o(&args)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, HubBinaryPath, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	logBuf := &LogBuffer{}
	cmd.Stdout = logBuf
	cmd.Stderr = logBuf

	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start hub: %v", err)
	}

	h := &Hub{
		Address: fmt.Sprintf("127.0.0.1:%d", port),
		DataDir: dataDir,
		port:    port,
		cmd:     cmd,
		logBuf:  logBuf,
		cancel:  cancel,
	}
	DumpOnFailure(t, "hub", logBuf)
	t.Cleanup(func() { h.Stop(t) })

	WaitForPort(t, port, 5*time.Second)
	return h
}

// StartHubWithRetention launches a hub with the given device-retention duration.
// Devices offline longer than retention will be pruned from the store entirely.
func StartHubWithRetention(t *testing.T, retention time.Duration) *Hub {
	t.Helper()
	return StartHub(t, WithRetention(retention))
}

// Log returns the hub process's captured stdout/stderr so far. Scenarios use
// it to assert on a specific log line as a positive witness for hub-internal
// state that has no other externally observable signal — e.g. that a
// shutdown actually found a live subscriber to close (issue #75).
func (h *Hub) Log() string {
	return h.logBuf.String()
}

// IssueJoinToken runs `hubfuse-hub issue-join --data-dir <DataDir>` against
// this hub's data directory and returns the token printed on stdout. The hub
// process must already be running (WAL mode permits concurrent access).
func (h *Hub) IssueJoinToken(t *testing.T) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, HubBinaryPath, "issue-join", "--data-dir", h.DataDir)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("hubfuse-hub issue-join: %v", err)
	}
	token := strings.TrimSpace(string(out))
	if token == "" {
		t.Fatal("hubfuse-hub issue-join: empty token")
	}
	return token
}

// Stop signals the hub to exit and waits for it to do so. It runs in
// t.Cleanup of every scenario test, so its wait has to sit comfortably above
// hub.DefaultShutdownBudget — Hub.Stop's own upper bound — or a legitimately
// slow-but-still-within-budget shutdown gets SIGKILLed here before it ever
// gets the chance to finish on its own (issue #75: the original 5s wait was
// below the 6s budget it should have been watching). A SIGKILL past that
// point is a genuine bug — the hub failed to exit within its own declared
// budget — so it is surfaced with t.Errorf rather than swallowed; t.Errorf,
// not t.Fatalf, because Stop runs during cleanup and must not abort whatever
// other cleanup steps run after it for other resources.
func (h *Hub) Stop(t *testing.T) {
	t.Helper()
	if h.cmd == nil || h.cmd.Process == nil {
		return
	}
	_ = h.cmd.Process.Signal(syscall.SIGTERM)
	done := make(chan error, 1)
	go func() { done <- h.cmd.Wait() }()
	wait := hub.DefaultShutdownBudget + stopCleanupMargin
	select {
	case <-done:
	case <-time.After(wait):
		t.Errorf("hub did not exit within %s of SIGTERM (its own shutdown budget is %s) — forcing SIGKILL",
			wait, hub.DefaultShutdownBudget)
		_ = h.cmd.Process.Kill()
		<-done
	}
	h.cancel()
	h.cmd = nil
}

// SignalAndWait sends SIGTERM and waits for the process to exit on its own,
// returning how long it took and the resulting ProcessState. It fails the
// test (and kills the process so the suite can continue) if the process does
// not exit within timeout — this is the direct regression check for issue
// #75: a hub that never exits on its own is exactly the bug this proves
// fixed, distinct from Stop's cleanup role of tolerating a slow-but-correct
// shutdown quietly.
//
// Exit is observed via cmd.Wait(), not syscall.Kill(pid, 0): this harness is
// the process's parent, and signal-0 against an unreaped zombie still
// returns nil, which would report a process that has already exited as
// alive.
//
// Unlike Stop, this consumes h.cmd itself (via cmd.Wait()), so a test that
// calls SignalAndWait leaves nothing for the Stop registered in t.Cleanup to
// do — Stop's own nil-cmd guard makes that a no-op.
func (h *Hub) SignalAndWait(t *testing.T, timeout time.Duration) (time.Duration, *os.ProcessState) {
	t.Helper()
	if h.cmd == nil || h.cmd.Process == nil {
		t.Fatal("SignalAndWait: hub is not running")
	}

	start := time.Now()
	if err := h.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SignalAndWait: send SIGTERM: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- h.cmd.Wait() }()

	select {
	case waitErr := <-done:
		elapsed := time.Since(start)
		if waitErr != nil {
			if _, ok := waitErr.(*exec.ExitError); !ok {
				t.Fatalf("SignalAndWait: cmd.Wait: %v", waitErr)
			}
		}
		state := h.cmd.ProcessState
		h.cancel()
		h.cmd = nil
		return elapsed, state
	case <-time.After(timeout):
		_ = h.cmd.Process.Kill()
		<-done
		h.cancel()
		h.cmd = nil
		t.Fatalf("SignalAndWait: hub did not exit within %s of SIGTERM", timeout)
		return 0, nil
	}
}

// Restart stops and starts the hub on the same data dir and the same port.
// Reusing the port ensures that agents whose config.kdl still references the
// original address can reconnect without any config change.
func (h *Hub) Restart(t *testing.T) {
	t.Helper()
	h.Stop(t)

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, HubBinaryPath, "start",
		"--listen", fmt.Sprintf("127.0.0.1:%d", h.port),
		"--data-dir", h.DataDir,
	)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = h.logBuf
	cmd.Stderr = h.logBuf
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("restart hub: %v", err)
	}
	h.cmd = cmd
	h.cancel = cancel
	// h.port and h.Address are unchanged — same endpoint as before.
	WaitForPort(t, h.port, 5*time.Second)
}
