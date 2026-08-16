package scenarios_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
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

// TestPrunedIdentityFailsToStart covers the first half of issue #69: a device
// whose row the hub has pruned still holds valid TLS material, so its daemon
// connects and registers successfully at the transport level — and the hub
// answers Success=false. That refusal used to be ignored: the daemon logged
// "registered with hub", wrote its PID file, and ran on as a ghost that no peer
// could ever see.
//
// Starting such a daemon must now fail loudly and say what to do about it.
func TestPrunedIdentityFailsToStart(t *testing.T) {
	hub := helpers.StartHubWithRetention(t, 5*time.Second)

	alice := helpers.StartAgent(t, hub, "alice")
	alice.Join(t)
	alice.StartDaemon(t)

	// bob is the observer: the prune is only visible through the device list.
	bob := helpers.StartAgent(t, hub, "bob")
	bob.Join(t)
	bob.StartDaemon(t)

	require.Eventually(t, func() bool {
		row, ok := bob.PeerStatus(t, "alice")
		return ok && row.Status == "online"
	}, 10*time.Second, 200*time.Millisecond, "bob should see alice online")

	alice.Stop(t)

	require.Eventually(t, func() bool {
		_, ok := bob.PeerStatus(t, "alice")
		return !ok
	}, 30*time.Second, 1*time.Second, "alice should be pruned from the hub")

	// alice's certificates, identity and config are all still on disk — only
	// the hub's row is gone.
	out := alice.StartDaemonExpectFailure(t, 30*time.Second)

	assert.Contains(t, out, "not registered on the hub",
		"the failure must name the cause: the hub does not know this device")
	assert.Contains(t, out, "hubfuse join",
		"the failure must tell the operator how to recover")
	assert.NotContains(t, out, "registered with hub",
		"a refused registration must never be reported as a successful one")
}
