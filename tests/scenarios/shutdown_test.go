//go:build unix

package scenarios_test

import (
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ykhdr/hubfuse/internal/hub"
	"github.com/ykhdr/hubfuse/tests/scenarios/helpers"
)

// TestHubExitsOnSIGTERMWithLiveAgent is the end-to-end reproduction of issue
// #75: a hub with one healthy, connected agent must exit on SIGTERM by
// itself, promptly, with exit code 0 — no SIGKILL needed.
//
// Before the fix, Hub.Stop called grpcServer.GracefulStop(), which waits for
// every in-flight RPC — and the agent's Subscribe stream is exactly that, a
// long-lived RPC that only ends when its client goes away. Measured on real
// binaries:
//
//	hub, no subscribers                       10.5ms
//	hub + ONE HEALTHY agent daemon             29.99s, then SIGKILLed
//	hub + agent that had already deregistered  11ms
//
// 29.99s is almost exactly 3 x 10s — the agent's own maxHeartbeatFailures x
// heartbeat interval. The hub was waiting for the AGENT to notice its own
// heartbeats were going nowhere and give up, not the other way around: a
// perfectly healthy agent triggered this as reliably as a dead one, which is
// why the fix lives entirely on the hub side (CloseAllSubscribers ends every
// subscriber stream itself — see lifecycle.go and hub.go).
func TestHubExitsOnSIGTERMWithLiveAgent(t *testing.T) {
	hubProc := helpers.StartHub(t)

	peer := helpers.StartAgent(t, hubProc, "sigterm-peer")
	peer.Join(t)
	peer.StartDaemon(t)

	// Confirm the daemon's hub session is actually up before signalling the
	// hub — otherwise a race could send SIGTERM before Subscribe ever opens
	// and this would quietly degrade into the "already deregistered" 11ms
	// case instead of the healthy-subscriber 29.99s case this guards
	// against. "registered with hub" logs right before the daemon opens its
	// Subscribe stream (see daemon.go's sessionOnce), and by the time this
	// polling loop observes it, the daemon process has had further real
	// wall-clock time to complete that immediate next, non-blocking step —
	// the same synchronisation idiom tests/scenarios/connection_test.go
	// relies on before its own relay.Break() depends on the stream being
	// live.
	peer.WaitForDaemonLog(t, "registered with hub", 5*time.Second)

	// A generous absolute ceiling so a genuine hang fails the test instead of
	// hanging the suite; the real regression signal is the tighter assertion
	// on elapsed below.
	elapsed, state := hubProc.SignalAndWait(t, hub.DefaultShutdownBudget+10*time.Second)

	assert.LessOrEqual(t, elapsed, hub.DefaultShutdownBudget,
		"the hub must exit within its own declared shutdown budget with a healthy subscriber attached — "+
			"before the fix this took 29.99s (3x the agent's own heartbeat-failure budget), because "+
			"GracefulStop waited for the agent's long-lived Subscribe stream instead of ending it itself")
	require.NotNil(t, state, "SignalAndWait must report a ProcessState for an observed exit")
	assert.Equal(t, 0, state.ExitCode(), "the hub must exit cleanly, not be killed")
	if ws, ok := state.Sys().(syscall.WaitStatus); ok {
		assert.False(t, ws.Signaled(), "the hub must exit on its own rather than being SIGKILLed")
	}

	// A positive witness, not just an inference from timing: this proves the
	// fast exit happened WHILE the hub actually had a live subscriber to
	// close, rather than the daemon's Subscribe having raced ahead and ended
	// on its own first (which would silently degrade this into the
	// already-deregistered 11ms case above and defeat the point of the
	// test). CloseAllSubscribers logs exactly this line, with the count, right
	// before it closes each subscriber's channel (see lifecycle.go / hub.go).
	assert.Contains(t, hubProc.Log(), "closed subscriber streams for shutdown",
		"the hub's own shutdown log should show it closed a live subscriber, not that there was none to close")
	assert.Contains(t, hubProc.Log(), "count=1",
		"exactly one subscriber (this test's single peer) should have been closed")
}
