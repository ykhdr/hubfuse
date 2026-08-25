package scenarios_test

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ykhdr/hubfuse/tests/scenarios/helpers"
	"github.com/ykhdr/hubfuse/tests/testrelay"
)

const (
	// idleWindow is how long the daemon is watched while its hub is unreachable.
	//
	// Thirty seconds and not less, because by the time the window closes the
	// daemon must have been through BOTH shapes of the outage: gRPC keepalive
	// tears the silenced transport down at about 15s (hubKeepaliveTime +
	// hubKeepaliveTimeout), and everything after that is the reconnect loop
	// dialling a relay that will never answer. A shorter window would sample only
	// the first of the two. Not more, because the scenario package pays for every
	// second of it in wall-clock.
	idleWindow = 30 * time.Second

	// idleCPUBudget is what the daemon is allowed to consume in that window.
	//
	// Both ends of the range it sits between are measured rather than guessed. On
	// Linux, a daemon held in exactly this state spent 0.010s of CPU over 120s
	// and 0.030s over 300s — 0.01% of one core. Issue #73 reports ~180% of a core
	// on macOS, which across this window would be ~54s of CPU. One second is
	// ~100x above the measured idle and ~54x below the reported spin: comfortably
	// out of reach of a busy runner, and impossible for anything spin-shaped to
	// slip under.
	//
	// It is a budget of CONSUMED CPU (utime+stime), not of wall-clock, so a
	// loaded runner stretches the window without inflating the number.
	idleCPUBudget = time.Second

	// userHZ is the unit of the utime/stime fields in /proc/<pid>/stat. It is 100
	// and fixed by the kernel's userspace ABI: /proc reports these in USER_HZ
	// whatever the kernel's internal CONFIG_HZ is, so this is a constant of the
	// interface being read, not a property of the machine reading it.
	userHZ = 100
)

// TestDaemonStaysIdleWhileTheHubIsUnreachable is the regression for the half of
// issue #73 that can be turned into an invariant: while the hub cannot be
// reached, the daemon waits — it does not work.
//
// WHAT IT GUARDS. The paths a daemon really walks during an outage: heartbeat
// calls going nowhere, the transport being torn down by keepalive, and the
// reconnect loop dialling a relay that never answers. A regression that made any
// of those busy-wait, retry without a backoff, or spin on a failing dial shows up
// here as consumed CPU and nowhere else in the suite.
//
// WHAT IT DOES NOT GUARD — worth being blunt about, since the issue is a spin
// report. It does not cover the transparent-retry loop inside grpc-go that the
// issue's goroutine dump names: http2_client.go:887 returns a NewStreamError with
// AllowTransparentRetry, stream.go:692-693 short-circuits to "retry" ahead of
// both the throttler and MaxAttempts, and stream.go:783-802 then loops
// finish → shouldRetry → newAttemptLocked with no sleep anywhere. Reaching it
// needs the picker to hand out a READY transport that then refuses the stream,
// and under BreakAll there is no READY transport at all. That branch was closed
// by direct measurement instead of by this test: driven continuously with a hub
// sending GOAWAY twice a second, 118 calls produced 0 failures, a slowest call of
// 13ms and a worst single call of 0.030s of CPU; with the upstream listener
// closed so no replacement transport could exist, the call failed in 1ms at
// 0.000s. It cannot persist because handleGoAway calls t.onClose BEFORE setting
// t.state = draining, so clientconn drops the transport before the picker can
// hand it back, and the window is milliseconds wide.
//
// It also does not reproduce macOS. Every measurement behind this test is Linux;
// the report is macOS 26.4 arm64, where per issue #74 the machine withdraws LAN
// access from the daemon after ~35s. The claim this test supports is "not
// reproducible on Linux", never "does not happen".
//
// AND WHY IT IS NOT testwedge, which is closer to the reported trigger: testwedge
// is an in-process façade over hubtest.Harness, while the assertion here is the
// CPU of one specific PROCESS read out of /proc. In-process, the same number
// would include the hub and the test binary, and the daemon's share of it would
// not be separable.
func TestDaemonStaysIdleWhileTheHubIsUnreachable(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the assertion is the CPU consumed by the daemon PROCESS, read from /proc/<pid>/stat; " +
			"there is no portable equivalent, and measuring the test process instead would assert nothing")
	}

	hub := helpers.StartHub(t)
	relay := testrelay.Start(t, hub.Address)

	// bob's entire hub conversation goes through the relay, and nothing else
	// does: no exports, no mounts, so the process being measured has no children
	// and its own utime+stime is the whole story.
	bob := helpers.StartAgent(t, hub, "bob", helpers.WithHubAddress(relay.Addr))
	bob.Join(t)
	bob.StartDaemon(t)
	bob.WaitForDaemonLog(t, "registered with hub", 15*time.Second)

	// BreakAll, not Break: Break forwards connections opened after it, so the
	// daemon would be back on a healthy hub within ~15s and the second half of
	// the window would be measuring an ordinary, connected daemon.
	relay.BreakAll()

	// The positive control, and the test is worthless without it. A green budget
	// is equally green for a daemon that idles correctly through an outage and
	// for one the outage never reached — a broken fixture, a daemon that failed
	// to start, a relay pointed somewhere else. The daemon has to be seen
	// noticing. The heartbeat is on its production 10s cadence here, so the first
	// failure lands within ~20s (one tick plus a per-call deadline, or sooner
	// once keepalive kills the transport at ~15s).
	bob.WaitForDaemonLog(t, "heartbeat failed", 45*time.Second)

	pid := bob.DaemonPID(t)
	before, state := readProcCPU(t, pid)
	require.NotEqual(t, "Z", state, "the daemon must be alive at the start of the window")

	// A plain sleep, deliberately: unlike everywhere else in this package there
	// is nothing to wait FOR here. The window is not a timeout hiding a missing
	// handle — it is the measurement itself, and shortening it would shrink the
	// sample rather than speed up a wait.
	time.Sleep(idleWindow)

	after, state := readProcCPU(t, pid)

	// A daemon that died mid-window would be a zombie whose /proc entry still
	// reads fine and whose CPU counters simply stopped — the greenest possible
	// result for the worst possible outcome. This is the same trap #75 hit with
	// signal-0 against an unreaped child.
	require.NotEqual(t, "Z", state, "the daemon died during the window; a frozen CPU counter is not idleness")

	spent := after - before
	t.Logf("daemon CPU over %s with an unreachable hub: %s (budget %s)", idleWindow, spent, idleCPUBudget)
	assert.Less(t, spent, idleCPUBudget,
		"the daemon must WAIT out an unreachable hub, not work at it: %s of CPU in %s", spent, idleWindow)
}

// readProcCPU returns the CPU a process has consumed since it started, together
// with its scheduler state so the caller can tell a live process from a zombie.
func readProcCPU(t *testing.T, pid int) (time.Duration, string) {
	t.Helper()

	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	require.NoError(t, err, "read /proc/%d/stat: the daemon must exist at both ends of the window", pid)
	stat := string(raw)

	// Field 2 is the executable name in parentheses, and it may itself contain
	// spaces and parentheses, so the only safe split point in this line is the
	// LAST ')'. Everything after it begins at field 3 (state) — which is why the
	// indices below are shifted by two.
	comm := strings.LastIndex(stat, ")")
	require.Positive(t, comm, "malformed /proc/%d/stat: %q", pid, stat)

	fields := strings.Fields(stat[comm+1:])
	require.GreaterOrEqual(t, len(fields), 13, "truncated /proc/%d/stat: %q", pid, stat)

	// utime and stime are fields 14 and 15 one-indexed; fields[0] is field 3.
	utime, err := strconv.ParseInt(fields[11], 10, 64)
	require.NoError(t, err, "parse utime from %q", stat)
	stime, err := strconv.ParseInt(fields[12], 10, 64)
	require.NoError(t, err, "parse stime from %q", stat)

	return time.Duration(utime+stime) * time.Second / userHZ, fields[0]
}
