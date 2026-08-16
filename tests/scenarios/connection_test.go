package scenarios_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ykhdr/hubfuse/tests/scenarios/helpers"
	"github.com/ykhdr/hubfuse/tests/testrelay"
)

// TestAgentRecoversFromSilentlyDeadHubConnection is the end-to-end regression
// for issue #72: the whole daemon, not just a client, must come back on its own
// after its hub connection stops carrying traffic without either side being
// told.
//
// This is what happened on real hardware: the agent kept an ESTABLISHED socket
// to a hub that had already forgotten it, the event stream never ended, the
// supervisor never reconnected, and the device stayed offline — with its shares
// unmounted on every peer — until someone restarted the daemon by hand.
//
// The relay silences the connection without closing a socket, so nothing errors
// on its own. Connections opened after the break are forwarded normally, which
// is what lets the daemon recover once it notices.
func TestAgentRecoversFromSilentlyDeadHubConnection(t *testing.T) {
	hub := helpers.StartHub(t)
	relay := testrelay.Start(t, hub.Address)

	// alice talks to the hub directly and is the independent observer of what
	// the hub thinks of bob.
	alice := helpers.StartAgent(t, hub, "alice")
	alice.Join(t)
	alice.StartDaemon(t)

	// bob's entire hub conversation goes through the relay.
	bob := helpers.StartAgent(t, hub, "bob", helpers.WithHubAddress(relay.Addr))
	bob.Join(t)
	bob.StartDaemon(t)

	require.Eventually(t, func() bool {
		row, ok := alice.PeerStatus(t, "bob")
		return ok && row.Status == "online"
	}, 15*time.Second, 250*time.Millisecond, "bob should be online through the relay")

	bob.WaitForDaemonLog(t, "registered with hub", 10*time.Second)

	// Silence bob's connection: no FIN, no RST, nothing to observe.
	relay.Break()

	// Only a keepalive ping going unanswered can notice this. Without it the
	// daemon sits here forever.
	// Budgets are deliberately tight-but-sufficient (detection ~15s, then one
	// reconnect): a regression must surface as these assertions, not as a
	// package-wide timeout panic that also swallows the other scenarios.
	bob.WaitForDaemonLog(t, "hub session lost", 30*time.Second)
	bob.WaitForDaemonLogCount(t, "registered with hub", 2, 30*time.Second)

	require.Eventually(t, func() bool {
		row, ok := alice.PeerStatus(t, "bob")
		return ok && row.Status == "online"
	}, 30*time.Second, 500*time.Millisecond,
		"bob must be back online without a restart")
}
