package scenarios_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/ykhdr/hubfuse/tests/scenarios/helpers"
)

// TestMonitorRemountsDeadMount reproduces the user's issue #67 scenario
// end-to-end: an sshfs process dies while its peer stays registered at an
// UNCHANGED endpoint. The hub emits no event whatsoever (nobody went offline,
// nothing roamed), so before #67 the mount stayed a "Transport endpoint is not
// connected" zombie forever. The mount monitor must confirm the mount dead on
// its next tick and heal it through Mount's same-endpoint dead branch — so
// this scenario also drives that branch end-to-end (reconcileMounts → Mount →
// probe says dead → teardown → fresh mount at the same IP/port).
func TestMonitorRemountsDeadMount(t *testing.T) {
	hub := helpers.StartHub(t)

	exportDir := t.TempDir()
	require.NoError(t, writeTestFile(exportDir, "hello.txt", "hello"), "seed export")

	alice := helpers.StartAgent(t, hub, "alice", helpers.WithExport(exportDir, "docs"))
	alice.Join(t)
	alice.StartDaemon(t)

	// bob (the mounting side) runs the monitor at a 1s cadence so healing is
	// observable within seconds instead of the 15s production default.
	bob := helpers.StartAgent(t, hub, "bob",
		helpers.WithEnv("HUBFUSE_MOUNT_MONITOR_INTERVAL=1s"))
	bob.Join(t)
	bob.StartDaemon(t)

	require.Eventually(t, func() bool { return alice.HasPeer(t, "bob") && bob.HasPeer(t, "alice") },
		5*time.Second, 200*time.Millisecond, "hub should see both devices online")

	code := alice.RequestPairing(t, "bob")
	bob.ConfirmPairing(t, code)
	require.True(t, alice.WaitForPairedWith(t, 5*time.Second),
		"alice's daemon should have saved bob's public key before mounting")

	mountPoint := filepath.Join(t.TempDir(), "alice-docs")
	bob.Mount(t, "alice:docs", mountPoint)

	// Mount() returns on marker appearance, up to one verify poll-interval
	// BEFORE bob's daemon records the mount as active. Killing the stub inside
	// that window would abort the in-flight mount (no activeMounts entry, so
	// nothing for the monitor to heal) — wait for the daemon's own confirmation.
	bob.WaitForDaemonLog(t, "mounted share", 10*time.Second)

	markerPath := bob.MountMarker(mountPoint)
	oldPID := helpers.ReadMarker(t, markerPath).PID

	// Simulate "sshfs died": SIGTERM the stub. KillStubMount returns only after
	// the marker is gone, so any marker observed below belongs to a NEW stub.
	bob.KillStubMount(t, mountPoint)

	// No offline/online cycle, no event — only the monitor can notice. Within
	// a tick (1s) plus the mount verify-poll it must re-mount: a fresh stub
	// appears with a new PID. 15s gives ample slack for the probe budget
	// (mountProbeTimeout 3s) plus the SSH+SFTP handshake of the new stub.
	require.Eventually(t, func() bool {
		marker, ok := helpers.TryReadMarker(markerPath)
		return ok && marker.PID > 0 && marker.PID != oldPID
	}, 15*time.Second, 200*time.Millisecond,
		"mount monitor should re-mount the dead mount without any hub event (new stub PID expected)")

	// The healed mount must target the same, unchanged endpoint.
	marker := helpers.ReadMarker(t, markerPath)
	require.Equal(t, alice.SSHPort, marker.RemotePort, "healed mount must target alice's unchanged SSH port")
	require.Equal(t, "docs", marker.RemotePath, "healed mount must target the same share alias")
}

// TestOfflineOnlineCycleRemountsMount verifies the event-driven heal chain
// that the truthful stub harness (#67 Task 5) made testable: a graceful peer
// restart at the SAME endpoint delivers DeviceOffline (which must REALLY tear
// the mount down — stubUnmount kills the stub and the marker disappears) and
// then DeviceOnline (which must mount cleanly from scratch — a fresh stub with
// a new PID). The monitor is parked at 10m so no tick can fire within the
// test: every observed transition is attributable to the event path alone.
//
// Review note: the event path through Mount's same-endpoint DEAD branch is
// unreachable in this scenario by design — with a live hub a graceful restart
// always broadcasts DeviceOffline first, so the mount entry is torn down
// before DeviceOnline arrives. That branch is covered by the Task 1 units and
// by TestMonitorRemountsDeadMount above.
func TestOfflineOnlineCycleRemountsMount(t *testing.T) {
	hub := helpers.StartHub(t)

	exportDir := t.TempDir()
	require.NoError(t, writeTestFile(exportDir, "hello.txt", "hello"), "seed export")

	alice := helpers.StartAgent(t, hub, "alice", helpers.WithExport(exportDir, "docs"))
	alice.Join(t)
	alice.StartDaemon(t)

	bob := helpers.StartAgent(t, hub, "bob",
		helpers.WithEnv("HUBFUSE_MOUNT_MONITOR_INTERVAL=10m"))
	bob.Join(t)
	bob.StartDaemon(t)

	require.Eventually(t, func() bool { return alice.HasPeer(t, "bob") && bob.HasPeer(t, "alice") },
		5*time.Second, 200*time.Millisecond, "hub should see both devices online")

	code := alice.RequestPairing(t, "bob")
	bob.ConfirmPairing(t, code)
	require.True(t, alice.WaitForPairedWith(t, 5*time.Second),
		"alice's daemon should have saved bob's public key before mounting")

	mountPoint := filepath.Join(t.TempDir(), "alice-docs")
	bob.Mount(t, "alice:docs", mountPoint)

	// Do not stop alice while bob's daemon is still inside its mount
	// verify-poll — the DeviceOffline unmount would then race the in-flight
	// mount. Wait until the mount is recorded as active.
	bob.WaitForDaemonLog(t, "mounted share", 10*time.Second)

	markerPath := bob.MountMarker(mountPoint)
	oldPID := helpers.ReadMarker(t, markerPath).PID

	// Graceful stop: alice deregisters, the hub broadcasts DeviceOffline to
	// bob, and bob's handleDeviceOffline unmounts alice's shares. Under the
	// truthful stub harness that unmount SIGTERMs the stub, whose defer removes
	// the marker — pinning the previously untestable offline-reap step.
	alice.Stop(t)

	require.Eventually(t, func() bool {
		_, err := os.Stat(markerPath)
		return os.IsNotExist(err)
	}, 10*time.Second, 100*time.Millisecond,
		"DeviceOffline should really unmount bob's mount (stub killed, marker removed)")

	// Bring alice back at the SAME endpoint. RestartDaemon reuses the SSH port
	// from the first start and does not re-run `share add` — the export is
	// already persisted in config.kdl.
	alice.RestartDaemon(t)

	// DeviceOnline must mount cleanly from scratch: a fresh stub, new PID.
	require.Eventually(t, func() bool {
		marker, ok := helpers.TryReadMarker(markerPath)
		return ok && marker.PID > 0 && marker.PID != oldPID
	}, 20*time.Second, 200*time.Millisecond,
		"DeviceOnline after alice's restart should re-mount bob's share cleanly (new stub PID expected)")

	marker := helpers.ReadMarker(t, markerPath)
	require.Equal(t, alice.SSHPort, marker.RemotePort, "re-mount must target alice's unchanged SSH port")
	require.Contains(t, marker.RemoteFiles, "hello.txt",
		"fresh stub should have listed the export via a real sftp handshake against the restarted alice")
}
