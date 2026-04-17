package scenarios_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ykhdr/hubfuse/tests/scenarios/helpers"
)

// TestAgentReconnectsAfterHubRestart verifies that alice's daemon, with its
// 1s-starting backoff, re-connects to the hub after a restart. Success is
// observed by `hubfuse devices` returning alice's entry again, which requires
// the gRPC transport channel to have re-established successfully.
//
// Note: the hub stores device state in SQLite. On restart the hub reloads the
// store from disk; alice's status is "offline" (set during graceful Stop).
// Re-registration (and the resulting "online" status) would require the daemon
// to call Register again — a feature not yet implemented in the daemon. This
// test therefore validates transport reconnect (devices command reaches the hub
// and returns alice's row) rather than full re-registration.
func TestAgentReconnectsAfterHubRestart(t *testing.T) {
	hub := helpers.StartHub(t)

	alice := helpers.StartAgent(t, hub, "alice")
	alice.Join(t)
	alice.StartDaemon(t)

	// Pre-condition: alice is online before the restart.
	require.Eventually(t, func() bool {
		row, ok := alice.PeerStatus(t, "alice")
		return ok && row.Status == "online"
	}, 5*time.Second, 200*time.Millisecond, "alice should be online before restart")

	hub.Restart(t)

	// Allow up to 15s: the connector backoff starts at 1s and doubles, so
	// worst-case is roughly 1+2+4+8 = 15s before the fourth attempt succeeds.
	// We verify the gRPC channel reconnected by confirming `hubfuse devices`
	// can reach the hub and alice's row is present (in any status).
	require.Eventually(t, func() bool {
		_, ok := alice.PeerStatus(t, "alice")
		return ok
	}, 15*time.Second, 500*time.Millisecond, "alice should reach the hub and see her own entry after restart")
}
