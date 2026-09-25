package scenarios_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ykhdr/hubfuse/tests/scenarios/helpers"
)

// TestConfigWatcherStartsBeforeTheHubIsReachable pins the WIRING, which the unit
// tests cannot: they call startConfigWatcher directly and would keep passing if
// Run stopped calling it.
//
// The watcher used to be created in runServices, which Run reaches only once a
// hub session exists. fsnotify reports only events that occur after watching
// begins, so a config edit made before that was not delayed — it was lost, with
// nothing to replay it (#103). Since #74 the window has no bound, because a
// daemon whose hub is unreachable now retries forever instead of exiting.
//
// The same gap is half of #85: helpers.launchDaemon waits for the daemon's
// `ssh server listening` line, which startSSH emits BEFORE registration, and
// StartDaemon then writes config. On a loaded runner the write lands first and
// TestACL_ReadOnlyRejectsWrites waits out its whole timeout against an empty
// SFTP listing — which is what `last listing: (empty)` was saying.
//
// Reverted, this fails at the log wait: with the hub down the daemon never
// reaches runServices, so the line never appears.
func TestConfigWatcherStartsBeforeTheHubIsReachable(t *testing.T) {
	hub := helpers.StartHub(t)

	// Join needs a live hub — the certificate exchange is the one step that
	// cannot happen without one.
	alice := helpers.StartAgent(t, hub, "alice")
	alice.Join(t)

	hub.Stop(t)

	alice.StartDaemon(t)

	// With the hub down the daemon is in its retry loop and will never reach
	// runServices. The watcher must already be up regardless.
	alice.WaitForDaemonLog(t, "config watcher started", 20*time.Second)

	// And it must be genuinely retrying rather than having got a session from
	// somewhere — otherwise the assertion above proves nothing about ordering.
	alice.WaitForDaemonLogCount(t, "hub session reconnect failed, retrying", 1, 20*time.Second)

	// The payoff: the hub comes back and the daemon registers, carrying whatever
	// the config held. The share CONTENT half is covered by the unit tests, which
	// can observe the ACL snapshot directly.
	hub.Restart(t)

	require.Eventually(t, func() bool {
		row, ok := alice.PeerStatus(t, "alice")
		return ok && row.Status == "online"
	}, 45*time.Second, 500*time.Millisecond,
		"the daemon must still come up normally once the hub returns")
}
