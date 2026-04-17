package scenarios_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ykhdr/hubfuse/tests/scenarios/helpers"
)

// TestStaleDevicePruned launches a hub with a very short retention, registers
// two agents, kills one, and verifies the dead one is pruned entirely from
// the device list of the survivor.
func TestStaleDevicePruned(t *testing.T) {
	hub := helpers.StartHubWithRetention(t, 5*time.Second)

	alice := helpers.StartAgent(t, hub, "alice")
	alice.Join(t)
	alice.StartDaemon(t)

	bob := helpers.StartAgent(t, hub, "bob")
	bob.Join(t)
	bob.StartDaemon(t)

	// Sanity: bob sees alice online.
	require.Eventually(t, func() bool {
		row, ok := bob.PeerStatus(t, "alice")
		return ok && row.Status == "online"
	}, 5*time.Second, 200*time.Millisecond, "bob should see alice online")

	alice.Stop(t)

	// With retention=5s, alice should disappear from bob's device list
	// within ~10s (retention + one prune-cycle tick).
	require.Eventually(t, func() bool {
		_, ok := bob.PeerStatus(t, "alice")
		return !ok
	}, 30*time.Second, 1*time.Second, "alice should be pruned from bob's device list")
}
