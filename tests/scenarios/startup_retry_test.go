package scenarios_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ykhdr/hubfuse/tests/scenarios/helpers"
)

// TestAgentStartsWithoutAHubAndComesUpWhenItReturns is the user-facing shape of
// issue #74, expressed literally: the machine comes back, so the agent must come
// back — without anyone touching it.
//
// Before this change the daemon had exactly one chance. `Run` called
// registerAndSubscribe, which returned the first sessionOnce error, and the
// process exited. Every LATER session already got reconnectSession's infinite
// backoff, so the daemon survived any failure except the first one — and a
// hubless start is the most ordinary first failure there is: a laptop waking
// before its network, a hub still booting, or (the case that produced the issue)
// macOS refusing the first LAN connect of an identity it has not yet registered.
//
// The test is deliberately end-to-end rather than a unit test of the retry. A
// unit test can prove the loop runs; only this can prove the daemon is still a
// working agent at the end of it — registered, visible to the hub, and reporting
// online.
//
// Two things make it objective rather than decorative:
//
//   - the wait on repeated "reconnect failed" lines is a POSITIVE CONTROL. It
//     fails if the daemon never actually tried, which is the way a test like
//     this goes quietly vacuous — a daemon that raced the hub's return and got
//     lucky on its first attempt would prove nothing about retrying.
//   - aliveness is asserted through that same log, never through
//     syscall.Kill(pid, 0). CLAUDE.md rules the signal out for this harness: it
//     returns nil for an unreaped zombie and would report a dead daemon as alive
//     — precisely the failure this test exists to catch.
//
// Reverted, it fails at the log wait: the daemon is gone by then, so the lines
// never appear.
func TestAgentStartsWithoutAHubAndComesUpWhenItReturns(t *testing.T) {
	hub := helpers.StartHub(t)

	// Join needs a live hub — the certificate exchange is the one step that
	// genuinely cannot happen without one. Everything after this point is what a
	// daemon faces on an ordinary boot: identity on disk, hub unreachable.
	alice := helpers.StartAgent(t, hub, "alice")
	alice.Join(t)

	hub.Stop(t)

	// The launch wait is satisfied by the daemon's own "ssh server listening"
	// line, which startSSH emits before it ever talks to the hub — so this
	// returns even with nothing on the other end.
	alice.StartDaemon(t)

	// Positive control AND aliveness in one assertion: two failed attempts mean
	// the daemon lived through the first failure and came back for more. The
	// backoff starts at 1s and doubles, so two lines land within ~3s.
	alice.WaitForDaemonLogCount(t, "hub session reconnect failed, retrying", 2, 20*time.Second)

	hub.Restart(t)

	// Generous, and for a reason worth stating: by now the backoff is at 4-8s
	// and doubling, so the first attempt AFTER the hub returns can be several
	// seconds away. That is the retry working as designed, not slowness.
	require.Eventually(t, func() bool {
		row, ok := alice.PeerStatus(t, "alice")
		return ok && row.Status == "online"
	}, 45*time.Second, 500*time.Millisecond,
		"the daemon must register itself once the hub comes back, with no intervention")
}
