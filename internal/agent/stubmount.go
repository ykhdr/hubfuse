package agent

// stubmount.go — the scenario-test mount harness, active when
// HUBFUSE_STUB_MOUNT_DIR is set (see NewMounter).
//
// In scenario tests the real sshfs binary is replaced by stub-sshfs
// (tests/tools/stub-sshfs), which performs a real SSH+SFTP handshake but never
// creates a FUSE mount; instead it writes a JSON marker file (containing its
// PID) into HUBFUSE_STUB_MOUNT_DIR and blocks until SIGTERM, removing the
// marker via defer on exit. The real isMountpoint/unmountPath are therefore
// meaningless under the stub: the mountpoint never exists, and fusermount on a
// non-mountpoint always fails.
//
// Historically stub mode hardwired checkMountpoint to (true, nil), which made
// the stub lifecycle unfalsifiable: Mount "verified" instantly even when the
// stub crashed, a killed stub was undetectable (so dead-mount healing could
// not be scenario-tested), and every unmount failed while its entry lingered.
// The two functions below replace the hardcode with faithful semantics driven
// by the stub's own marker protocol (issue #67):
//
//   - stubMountpointCheck — "mounted" means the marker exists AND its PID is
//     alive. The AUTHORITATIVE death signal is marker absence (the stub's
//     defer removes it on SIGTERM); the PID probe is a secondary guard that
//     catches a stub that died without running its defer.
//   - stubUnmount — SIGTERM the marker's PID and wait for the marker to
//     disappear, mirroring how a real unmount tears down the sshfs process.
//
// Zombie caveat: kill(pid, 0) does NOT return ESRCH for an unreaped zombie,
// so a SIGKILLed stub whose marker survived (defer never ran) still reads as
// ALIVE until its parent reaps it. Scenario tests MUST kill stubs with
// SIGTERM (which runs the defer and removes the marker) — never SIGKILL.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// stubUnmount timing: how long to wait for the stub's marker to disappear
// after SIGTERM, and how often to re-check. 2s comfortably fits inside every
// unmount caller's budget (3s interactive, 5s device-offline/shutdown) while
// giving the stub's signal handler + defer ample time to run.
const (
	stubUnmountWait         = 2 * time.Second
	stubUnmountPollInterval = 25 * time.Millisecond
)

// stubMarker is the subset of the stub-sshfs JSON marker this harness needs.
// The full shape is defined by the writer (tests/tools/stub-sshfs) and
// mirrored by the test-side reader (tests/scenarios/helpers/marker.go); only
// the PID matters for liveness and teardown.
type stubMarker struct {
	PID int `json:"pid"`
}

// stubSanitizePath converts a mount destination path into the stub marker
// filename stem.
//
// KEEP IN SYNC — this transformation exists in THREE packages that cannot
// import each other (a main package, a test helper package, and this one):
//   - sanitize() in tests/tools/stub-sshfs/main.go (marker writer)
//   - sanitizeForMarker() in tests/scenarios/helpers/agent.go (test-side reader)
//   - stubSanitizePath() here (agent-side liveness check + stub unmount, #67)
//
// Any drift makes the agent look for markers at the wrong path: every mount
// verify-poll would time out and every liveness probe would read "dead".
func stubSanitizePath(p string) string {
	r := strings.NewReplacer("/", "_", `\`, "_", ":", "_", " ", "_")
	return r.Replace(strings.TrimPrefix(p, "/"))
}

// stubMarkerPath returns the marker file path stub-sshfs writes for a mount
// at mountPath (dst on its command line, i.e. the Mounter's mc.To).
func stubMarkerPath(markerDir, mountPath string) string {
	return filepath.Join(markerDir, stubSanitizePath(mountPath)+".json")
}

// readStubMarkerPID reads the marker for mountPath and returns its PID.
// ok is false when the marker is absent, unreadable, unparsable, or carries a
// non-positive PID — all of which the harness treats as "no live stub" (a
// corrupt marker cannot vouch for a process, and kill(0, 0)/kill(-n, 0) would
// probe a process GROUP, not a process).
func readStubMarkerPID(markerDir, mountPath string) (pid int, ok bool) {
	data, err := os.ReadFile(stubMarkerPath(markerDir, mountPath))
	if err != nil {
		return 0, false
	}
	var m stubMarker
	if err := json.Unmarshal(data, &m); err != nil || m.PID <= 0 {
		return 0, false
	}
	return m.PID, true
}

// stubPIDAlive reports whether pid refers to an existing process, using the
// classic kill(pid, 0) probe. Only ESRCH is positive evidence of death; any
// other outcome (nil, or e.g. EPERM for a process we cannot signal) means the
// process exists. NOTE: an unreaped zombie "exists" — see the zombie caveat
// in the file header.
func stubPIDAlive(pid int) bool {
	return !errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
}

// stubMountpointCheck returns a checkMountpoint implementation for stub mode:
// the mount at path is "mounted" iff its marker exists and the recorded stub
// PID is alive. All failure shapes — no marker, unreadable/corrupt marker,
// dead PID — report (false, nil), i.e. positively "not a mountpoint", never
// an error: under the stub protocol marker absence IS the well-defined
// unmounted state (the stub's defer removes it on SIGTERM), unlike a real
// stat error which is ambiguous. This is what lets scenario tests express
// "sshfs died" (kill the stub with SIGTERM) and observe self-healing. (#67)
func stubMountpointCheck(markerDir string) func(string) (bool, error) {
	return func(path string) (bool, error) {
		pid, ok := readStubMarkerPID(markerDir, path)
		if !ok {
			return false, nil
		}
		return stubPIDAlive(pid), nil
	}
}

// stubUnmount returns an unmount implementation for stub mode: read the
// marker for path, SIGTERM the recorded stub process, and wait (bounded by
// stubUnmountWait and the caller's ctx) for the marker to disappear — the
// stub's defer removes it on the way out, so marker disappearance is the
// stub-protocol equivalent of "the mountpoint detached". No marker means the
// stub is already gone: success, mirroring unmountKey's reap semantics for an
// already-dead mount. A marker that persists is an error, so unmountKey's
// retain-and-retry / re-check machinery is exercised exactly as with a real
// wedged unmount. force is ignored — there is no escalation ladder for a
// stub; SIGTERM is the one true teardown (SIGKILL would skip the defer and
// strand the marker; see the zombie caveat in the file header). (#67)
func stubUnmount(markerDir string) func(ctx context.Context, path string, force bool) error {
	return stubUnmountWithWait(markerDir, stubUnmountWait, stubUnmountPollInterval)
}

// stubUnmountWithWait is stubUnmount with injectable timing so unit tests can
// exercise the "marker never disappears" timeout path without a 2s stall.
func stubUnmountWithWait(markerDir string, wait, poll time.Duration) func(ctx context.Context, path string, force bool) error {
	return func(ctx context.Context, path string, _ bool) error {
		markerPath := stubMarkerPath(markerDir, path)

		data, err := os.ReadFile(markerPath)
		if err != nil {
			if os.IsNotExist(err) {
				// Marker already gone — the stub exited and cleaned up.
				// Unmounting an already-dead stub mount is success.
				return nil
			}
			return fmt.Errorf("stub unmount %q: read marker %s: %w", path, markerPath, err)
		}
		var m stubMarker
		if jsonErr := json.Unmarshal(data, &m); jsonErr != nil || m.PID <= 0 {
			// A marker with no usable PID cannot be torn down (nothing to
			// signal) and will never disappear on its own — fail fast instead
			// of a hopeless wait. unmountKey's re-check then classifies the
			// path via stubMountpointCheck (which reads a corrupt marker as
			// "not mounted") and reaps the entry.
			return fmt.Errorf("stub unmount %q: marker %s has no usable pid (corrupt harness state)", path, markerPath)
		}

		if killErr := syscall.Kill(m.PID, syscall.SIGTERM); killErr != nil {
			if errors.Is(killErr, syscall.ESRCH) {
				// The stub died between our marker read and the signal. If
				// its defer got the marker off disk we merely raced a clean
				// exit — done. A persisting marker with a dead PID means the
				// stub was killed without SIGTERM (defer never ran): stale
				// harness state we refuse to paper over — scenarios must
				// SIGTERM stubs (zombie caveat, file header).
				if _, statErr := os.Stat(markerPath); os.IsNotExist(statErr) {
					return nil
				}
				return fmt.Errorf("stub unmount %q: pid %d is already dead but marker %s persists (stub killed without SIGTERM?)",
					path, m.PID, markerPath)
			}
			return fmt.Errorf("stub unmount %q: signal pid %d: %w", path, m.PID, killErr)
		}

		deadline := time.Now().Add(wait)
		for {
			if _, statErr := os.Stat(markerPath); os.IsNotExist(statErr) {
				return nil
			}
			if ctx.Err() != nil {
				return fmt.Errorf("stub unmount %q: %w", path, ctx.Err())
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("stub unmount %q: marker %s still present %s after SIGTERM to pid %d",
					path, markerPath, wait, m.PID)
			}
			time.Sleep(poll)
		}
	}
}
