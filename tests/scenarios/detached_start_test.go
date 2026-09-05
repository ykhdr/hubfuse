package scenarios_test

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ykhdr/hubfuse/tests/scenarios/helpers"
)

// TestDetachedStartSurvivesAnUnreachableHub covers `hubfuse start -d`, which
// nothing exercised end to end before #102 — prune_test and sshbind_test both
// use the FOREGROUND path, and the daemonize unit tests drive a `sleep 60`
// stand-in rather than the real binary. That gap is why #101 shipped with the
// retry missing from the most interactive path there is.
//
// The mechanism: `Spawn` waits for the daemon's PID file and kills the child if
// it never appears. While the PID file was written only after the hub ACCEPTED a
// Register, a daemon retrying an unreachable hub never wrote one, so `start -d`
// killed the very daemon #101 taught to survive. Readiness now means "committed
// to running", so the daemon reports itself and retries in the background.
//
// Each assertion is chosen to be objective rather than convenient:
//
//   - the zero exit and the printed pid fail on revert, because `Spawn` returns
//     an error after its readiness timeout and kills the child;
//   - `hubfuse status` is asserted on its OUTPUT, not its exit code —
//     ReportStatus returns nil either way, so an exit-code check would pass for
//     a daemon it had just reported as absent;
//   - the post-stop assertion is the PID file's ABSENCE, which the daemon
//     removes itself on every exit path. Asserting through the process table
//     would mean signal-0, which CLAUDE.md rules out for this harness: it
//     answers nil for an unreaped zombie, so an orphan under a PID 1 that does
//     not reap would look alive.
//
// It deliberately does NOT re-assert hub-down → retry → online. That belongs to
// TestAgentStartsWithoutAHubAndComesUpWhenItReturns, and repeating it here would
// cost ~45s for a property this change is not about.
func TestDetachedStartSurvivesAnUnreachableHub(t *testing.T) {
	hub := helpers.StartHub(t)

	// Join needs a live hub: the certificate exchange is the one step that
	// cannot happen without one.
	alice := helpers.StartAgent(t, hub, "alice")
	alice.Join(t)

	hub.Stop(t)

	out, ok := alice.StartDaemonDetached(t)
	require.True(t, ok,
		"`hubfuse start -d` must succeed with the hub down — before #102 Spawn killed the child "+
			"after its readiness timeout, because a retrying daemon had no PID file. Output:\n%s", out)
	assert.Contains(t, out, "started (pid",
		"and it must report the pid it started, not merely exit zero")

	// The daemon is up and doing the one thing it should: asking again.
	alice.WaitForDetachedLog(t, "hub session reconnect failed, retrying", 20*time.Second)

	pidPath := alice.PIDFilePath()
	_, statErr := os.Stat(pidPath)
	require.NoError(t, statErr, "a running daemon must be visible through its PID file")

	status, statusOK := alice.TryRun(t, "status")
	assert.True(t, statusOK, "status: %s", status)
	assert.Contains(t, status, "is running (pid",
		"`hubfuse status` must report a retrying daemon as running — ReportStatus returns nil "+
			"whatever it finds, so only its text carries the answer")

	stopOut, stopOK := alice.TryRun(t, "stop")
	require.True(t, stopOK, "`hubfuse stop` must stop a retrying daemon: %s", stopOut)

	require.Eventually(t, func() bool {
		_, err := os.Stat(pidPath)
		return os.IsNotExist(err)
	}, 10*time.Second, 100*time.Millisecond,
		"the daemon removes its own PID file on the way out; its absence is the stop, "+
			"and unlike a signal-0 probe it cannot be fooled by an unreaped zombie")

	assert.NotContains(t, strings.ToLower(stopOut), "refused to exit")
}
