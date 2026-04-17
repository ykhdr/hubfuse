package scenarios_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ykhdr/hubfuse/tests/scenarios/helpers"
)

// TestDeviceOnlineOfflineEvents verifies the hub's view of peer liveness as
// a second agent joins and then exits. We intentionally drive the observation
// through `hubfuse devices` (NOT `hubfuse status`, which only reports the
// LOCAL daemon's pid/alive status).
func TestDeviceOnlineOfflineEvents(t *testing.T) {
	hub := helpers.StartHub(t)

	alice := helpers.StartAgent(t, hub, "alice")
	alice.Join(t)
	alice.StartDaemon(t)

	bob := helpers.StartAgent(t, hub, "bob")
	bob.Join(t)
	bob.StartDaemon(t)

	// Both alice and bob should appear online to alice.
	require.Eventually(t, func() bool {
		row, ok := alice.PeerStatus(t, "bob")
		return ok && row.Status == "online"
	}, 5*time.Second, 200*time.Millisecond, "alice should see bob online after bob joins")

	// Bob shuts down; Deregister runs on SIGTERM, so the transition should be
	// near-instantaneous.
	bob.Stop(t)

	require.Eventually(t, func() bool {
		row, ok := alice.PeerStatus(t, "bob")
		return ok && row.Status == "offline"
	}, 15*time.Second, 500*time.Millisecond, "alice should see bob offline after bob.Stop")
}
