package helpers

import (
	"context"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Agent wraps invocation of the `hubfuse` binary against a hub, with an
// isolated HOME directory so agents do not touch each other's state.
type Agent struct {
	Nickname string
	HomeDir  string

	hub      *Hub
	logBuf   *LogBuffer
	envExtra []string
}

type AgentOption func(*Agent)

func WithEnv(kv ...string) AgentOption {
	return func(a *Agent) { a.envExtra = append(a.envExtra, kv...) }
}

// StartAgent prepares an isolated HOME for the agent. It does NOT launch a
// daemon process — use Join / run / runExpectFail for one-shot invocations.
// A long-running daemon lifecycle is added in Task 7.
func StartAgent(t *testing.T, hub *Hub, nickname string, opts ...AgentOption) *Agent {
	t.Helper()
	home := t.TempDir()
	a := &Agent{
		Nickname: nickname,
		HomeDir:  home,
		hub:      hub,
		logBuf:   &LogBuffer{},
	}
	for _, o := range opts {
		o(a)
	}
	DumpOnFailure(t, "agent:"+nickname, a.logBuf)
	return a
}

// run executes `hubfuse <args...>` with the agent's HOME and returns combined
// output. Test fails on non-zero exit.
func (a *Agent) run(t *testing.T, args ...string) string {
	t.Helper()
	return a.runWithStdin(t, nil, args...)
}

// runWithStdin variant that can pipe bytes to the child process stdin.
func (a *Agent) runWithStdin(t *testing.T, stdin []byte, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, AgentBinaryPath, args...)
	cmd.Env = a.env()
	if stdin != nil {
		cmd.Stdin = strings.NewReader(string(stdin))
	}
	out, err := cmd.CombinedOutput()
	a.logBuf.Write([]byte("$ hubfuse " + strings.Join(args, " ") + "\n"))
	a.logBuf.Write(out)
	if err != nil {
		t.Fatalf("hubfuse %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// runExpectFail executes `hubfuse <args...>` and returns combined output; it
// does NOT fail the test on non-zero exit. Useful for asserting error paths.
func (a *Agent) runExpectFail(t *testing.T, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, AgentBinaryPath, args...)
	cmd.Env = a.env()
	out, _ := cmd.CombinedOutput()
	a.logBuf.Write([]byte("$ hubfuse " + strings.Join(args, " ") + "  (expecting failure)\n"))
	a.logBuf.Write(out)
	return string(out)
}

// Join runs `hubfuse join <hub-addr>` with the nickname fed via stdin (the
// command prompts "Enter nickname for this device: ").
func (a *Agent) Join(t *testing.T) {
	t.Helper()
	stdin := []byte(a.Nickname + "\n")
	a.runWithStdin(t, stdin, "join", a.hub.Address)
}

// Stop is a no-op placeholder; expanded in Task 7 for daemon lifecycle.
func (a *Agent) Stop(t *testing.T) { //nolint:unused // used by later tasks
	t.Helper()
	_ = syscall.SIGTERM
}

// env builds the environment for a subprocess invocation of hubfuse. HOME is
// the agent's isolated dir; PATH is inherited from the test process.
func (a *Agent) env() []string {
	base := []string{
		"HOME=" + a.HomeDir,
		"PATH=" + existingPath(),
	}
	return append(base, a.envExtra...)
}
