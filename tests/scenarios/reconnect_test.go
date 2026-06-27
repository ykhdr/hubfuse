package scenarios_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ykhdr/hubfuse/tests/scenarios/helpers"
)

// TestAgentReconnectsAfterHubRestart verifies that alice's daemon, with its
// 1s-starting backoff, re-registers with the hub after a restart and is reported
// "online" again. Success is observed by `hubfuse devices` returning alice's
// entry with status "online", which requires both the gRPC transport channel to
// have re-established AND the daemon to have called Register a second time.
//
// The hub stores device state in SQLite. On restart the hub reloads the store
// from disk; alice's status is "offline" (set during graceful Stop). The daemon's
// session supervisor detects the dead Subscribe stream and re-runs the
// Register → Subscribe handshake, which re-marks alice "online" on the hub. This
// test asserts that full re-registration round-trip, not just transport
// reconnect.
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

	// Allow up to 20s: the supervisor backoff starts at 1s and doubles, so
	// worst-case is roughly 1+2+4+8 = 15s before the fourth re-register attempt
	// succeeds, plus the hub's own startup and the devices RPC round-trip. We
	// verify the daemon re-registered by confirming alice is reported "online"
	// again — transport reconnect alone would leave the stale "offline" row.
	require.Eventually(t, func() bool {
		row, ok := alice.PeerStatus(t, "alice")
		return ok && row.Status == "online"
	}, 20*time.Second, 500*time.Millisecond, "alice should re-register and be online again after restart")
}
