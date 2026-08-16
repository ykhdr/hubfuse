package scenarios_test

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ykhdr/hubfuse/tests/scenarios/helpers"
)

// TestAgentStaysOnlineWhileStaleMountBlocks reproduces the liveness half of
// issue #69 end-to-end.
//
// The reported setup: a device had a leftover outbound mount to a share it
// could no longer mount. Every attempt burned the full mount-verify window
// (10s) inside the daemon's session setup, and the heartbeat loop only started
// after that — so the hub declared the device offline, its peer unmounted the
// share it was serving, and the share vanished ~30 seconds after appearing.
// Nothing about the peer or the network was wrong.
//
// Here alice plays the affected device: she exports "docs" (which bob mounts)
// and carries a stale mount of bob's "blocked" share. "blocked" is exported
// with no --allow, so the SFTP handler denies the listing, stub-sshfs exits
// before writing its marker, and alice's mounter genuinely waits out the whole
// verify window on every attempt — a faithful, sleep-free stand-in for the
// user's unreachable mount. With the mount monitor at 1s, alice is inside a
// failing mount almost continuously.
//
// The assertion is the absence of a transient, not eventual recovery: alice
// must stay online for several heartbeat timeouts AND bob's mount must be the
// same stub process throughout. A DeviceOffline would kill that stub, so an
// unchanged PID proves the share never flapped.
func TestAgentStaysOnlineWhileStaleMountBlocks(t *testing.T) {
	const heartbeatTimeout = 3 * time.Second

	hub := helpers.StartHub(t, helpers.WithHeartbeatTimeout(heartbeatTimeout))

	aliceExport := t.TempDir()
	require.NoError(t, writeTestFile(aliceExport, "hello.txt", "hello"), "seed alice's export")

	// alice beats well inside the shortened timeout and re-tries the doomed
	// mount every second, so the mounter is busy for most of the observation.
	alice := helpers.StartAgent(t, hub, "alice",
		helpers.WithExport(aliceExport, "docs"),
		helpers.WithEnv(
			"HUBFUSE_HEARTBEAT_INTERVAL=500ms",
			"HUBFUSE_MOUNT_MONITOR_INTERVAL=1s",
		))
	alice.Join(t)
	alice.StartDaemon(t)

	// bob exports "blocked" with no --allow: visible to the hub (so alice will
	// try to mount it) but denied by the SFTP handler (so the mount never
	// completes).
	bobExport := t.TempDir()
	require.NoError(t, writeTestFile(bobExport, "secret.txt", "nope"), "seed bob's export")

	bob := helpers.StartAgent(t, hub, "bob",
		helpers.WithExportACL(bobExport, "blocked", "ro"),
		helpers.WithEnv("HUBFUSE_HEARTBEAT_INTERVAL=500ms"))
	bob.Join(t)
	bob.StartDaemon(t)

	require.Eventually(t, func() bool { return alice.HasPeer(t, "bob") && bob.HasPeer(t, "alice") },
		10*time.Second, 200*time.Millisecond, "hub should see both devices online")

	code := alice.RequestPairing(t, "bob")
	bob.ConfirmPairing(t, code)
	require.True(t, alice.WaitForPairedWith(t, 10*time.Second),
		"alice's daemon should have saved bob's public key")

	// Configure the doomed mount while alice is down, so it runs during her
	// session setup on the next start — exactly where the blocking used to
	// delay liveness.
	alice.Stop(t)
	alice.AddMount(t, "bob:blocked", filepath.Join(t.TempDir(), "bob-blocked"))
	alice.RestartDaemon(t)

	// bob mounts alice's share only after she is back, so the mount under
	// observation is not the one her restart tore down.
	require.Eventually(t, func() bool {
		row, ok := bob.PeerStatus(t, "alice")
		return ok && row.Status == "online"
	}, 15*time.Second, 250*time.Millisecond, "alice should be online again after the restart")

	mountPoint := filepath.Join(t.TempDir(), "alice-docs")
	bob.Mount(t, "alice:docs", mountPoint)
	bob.WaitForDaemonLog(t, "mounted share", 15*time.Second)

	markerPath := bob.MountMarker(mountPoint)
	pid := helpers.ReadMarker(t, markerPath).PID
	require.Positive(t, pid, "bob's mount should be backed by a live stub")

	// Watch for more than three heartbeat timeouts. Before the fix alice was
	// demoted after the first one.
	deadline := time.Now().Add(3*heartbeatTimeout + 2*time.Second)
	for time.Now().Before(deadline) {
		// PeerStatus shells out to `hubfuse devices`, so a single failed call
		// says nothing about alice — retry once before believing it. A real
		// demotion persists across both attempts.
		row, ok := bob.PeerStatus(t, "alice")
		if !ok {
			time.Sleep(200 * time.Millisecond)
			row, ok = bob.PeerStatus(t, "alice")
		}
		require.True(t, ok, "alice must remain in the hub's device list")
		require.Equal(t, "online", row.Status,
			"alice must stay online while her own stale mount keeps failing")

		marker, ok := helpers.TryReadMarker(markerPath)
		require.True(t, ok, "bob's mount of alice:docs must stay up (marker gone = unmounted)")
		require.Equal(t, pid, marker.PID,
			"bob's mount must not flap — a new stub PID means it was torn down and re-established")

		time.Sleep(500 * time.Millisecond)
	}
}
