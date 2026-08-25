package testrelay

import (
	"errors"
	"net"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These are the first tests this fixture has ever had, and they exist because
// the fixture is load-bearing for assertions made elsewhere. A scenario test
// built on BreakAll asserts an ABSENCE — the daemon consumes almost no CPU while
// its hub is unreachable — and an absence is exactly what a relay that quietly
// stopped silencing anything would also produce. Without these tests that
// scenario would go green on a broken fixture and report the opposite of what it
// was measuring. (#73)

// silenceWindow is how long a "nothing arrives" assertion waits before calling
// the connection silent. Every test that uses it first completes a round trip
// through the same relay over the same loopback, which takes milliseconds, so
// this is roughly two orders of magnitude of margin over a working path — chosen
// that way because the cost of a false green here is a regression test elsewhere
// watching a relay that no longer silences anything.
const silenceWindow = 500 * time.Millisecond

// ─── fixture: a stand-in for the hub ──────────────────────────────────────────

// echoTarget is a minimal TCP server standing in for the hub. It records
// everything it is sent, and it can push unsolicited bytes back.
//
// The push is what makes the two directions separately observable. A plain echo
// server cannot do that: once the relay has silenced client → target, the target
// never sees the request, so it never echoes, and "nothing came back" is
// consistent with either direction being broken. Pushing from the target proves
// target → client on its own.
type echoTarget struct {
	addr string

	mu    sync.Mutex
	conns []net.Conn
	got   strings.Builder
}

func startEchoTarget(t *testing.T) *echoTarget {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "echo target: listen")

	tgt := &echoTarget{addr: lis.Addr().String()}
	go func() {
		for {
			conn, acceptErr := lis.Accept()
			if acceptErr != nil {
				return // listener closed
			}
			tgt.mu.Lock()
			tgt.conns = append(tgt.conns, conn)
			tgt.mu.Unlock()
			go tgt.consume(conn)
		}
	}()

	t.Cleanup(func() {
		_ = lis.Close()
		tgt.mu.Lock()
		conns := tgt.conns
		tgt.conns = nil
		tgt.mu.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
	})
	return tgt
}

func (e *echoTarget) consume(conn net.Conn) {
	buf := make([]byte, 4096)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			e.mu.Lock()
			_, _ = e.got.Write(buf[:n])
			e.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

// got returns everything the target has received on any connection so far.
func (e *echoTarget) received() string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.got.String()
}

// connCount reports how many connections the target is currently holding — i.e.
// how many times the relay has dialled upstream.
func (e *echoTarget) connCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.conns)
}

// push writes msg down every connection the target currently holds.
func (e *echoTarget) push(t *testing.T, msg string) {
	t.Helper()
	e.mu.Lock()
	conns := append([]net.Conn(nil), e.conns...)
	e.mu.Unlock()

	require.NotEmpty(t, conns, "the target has no connection to push down")
	for _, conn := range conns {
		_, err := conn.Write([]byte(msg))
		require.NoError(t, err, "echo target: push %q", msg)
	}
}

// ─── fixture: assertions about a socket ───────────────────────────────────────

func dialRelay(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err, "dial relay")
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func send(t *testing.T, conn net.Conn, msg string) {
	t.Helper()
	_, err := conn.Write([]byte(msg))
	require.NoError(t, err, "write %q", msg)
}

// expectSilence asserts that nothing arrives on conn within window AND that the
// reason is a read TIMEOUT rather than an error.
//
// The second half is the whole point of the fixture and not a detail: an EOF or
// a reset is a clean, observable end of a connection, and gRPC repairs those on
// its own within milliseconds. The failure being reproduced never produces an
// error at all — that is why it needs a fixture in the first place — so a relay
// that closed sockets instead of going quiet would satisfy "nothing arrived"
// while reproducing something else entirely.
func expectSilence(t *testing.T, conn net.Conn, window time.Duration) {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(window)))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()

	n, err := conn.Read(make([]byte, 256))
	require.Zero(t, n, "expected silence, but bytes arrived")

	var netErr net.Error
	require.True(t, errors.As(err, &netErr) && netErr.Timeout(),
		"the socket must stay OPEN and simply carry nothing; got %v", err)
}

func expectDelivery(t *testing.T, conn net.Conn, want string) {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	defer func() { _ = conn.SetReadDeadline(time.Time{}) }()

	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	require.NoError(t, err, "expected %q to be delivered", want)
	assert.Equal(t, want, string(buf[:n]))
}

// ─── tests ────────────────────────────────────────────────────────────────────

// TestRelay_ForwardsBothDirectionsUntilBreakAll pins the baseline the other
// tests rest on: while the relay is intact it is a plain forwarder, and after
// BreakAll neither direction carries anything while both sockets stay open.
func TestRelay_ForwardsBothDirectionsUntilBreakAll(t *testing.T) {
	target := startEchoTarget(t)
	relay := Start(t, target.addr)

	conn := dialRelay(t, relay.Addr)

	send(t, conn, "ping")
	require.Eventually(t, func() bool { return strings.Contains(target.received(), "ping") },
		2*time.Second, 10*time.Millisecond,
		"an intact relay must forward client → target")
	target.push(t, "pong")
	expectDelivery(t, conn, "pong")

	relay.BreakAll()

	send(t, conn, "after-break")
	target.push(t, "unheard")

	// One window covers both directions: waiting out the read below is also the
	// time in which "after-break" would have had to reach the target.
	expectSilence(t, conn, silenceWindow)
	assert.NotContains(t, target.received(), "after-break",
		"BreakAll must silence client → target as well, not just the answers")
}

// TestRelay_BreakAllSilencesConnectionsMadeAfterIt is the property BreakAll
// exists for, and the only one Break does not have.
//
// Without it the #73 idle regression would hold its daemon in an outage for
// about fifteen seconds — until gRPC keepalive tears the silent transport down —
// and then spend the rest of its measurement window watching a healthy,
// reconnected daemon. The budget would pass for the wrong reason.
func TestRelay_BreakAllSilencesConnectionsMadeAfterIt(t *testing.T) {
	target := startEchoTarget(t)
	relay := Start(t, target.addr)

	relay.BreakAll()

	conn := dialRelay(t, relay.Addr)
	send(t, conn, "after-break-all")

	// The relay must still ACCEPT and still dial upstream: it silences traffic,
	// it does not refuse connections. A relay that stopped accepting would give
	// the client an immediate, clean connection error — a completely different
	// failure, and one gRPC handles well.
	require.Eventually(t, func() bool { return target.connCount() > 0 },
		2*time.Second, 10*time.Millisecond,
		"BreakAll must not stop the relay from accepting and dialling upstream")

	target.push(t, "unheard")
	expectSilence(t, conn, silenceWindow)
	assert.NotContains(t, target.received(), "after-break-all",
		"a connection made after BreakAll must be born silent")
}

// TestRelay_BreakLeavesLaterConnectionsAlive pins Break's own contract — the one
// thing BreakAll changes.
//
// It belongs here rather than only in the #72 recovery scenarios because the two
// methods are now one struct field apart: a BreakAll that also set the flag for
// Break would leave every recovery scenario timing out somewhere far away, with
// nothing pointing back at this file.
func TestRelay_BreakLeavesLaterConnectionsAlive(t *testing.T) {
	target := startEchoTarget(t)
	relay := Start(t, target.addr)

	old := dialRelay(t, relay.Addr)
	send(t, old, "old-before")
	require.Eventually(t, func() bool { return strings.Contains(target.received(), "old-before") },
		2*time.Second, 10*time.Millisecond)

	relay.Break()
	send(t, old, "old-after")

	fresh := dialRelay(t, relay.Addr)
	send(t, fresh, "fresh")
	require.Eventually(t, func() bool { return strings.Contains(target.received(), "fresh") },
		2*time.Second, 10*time.Millisecond,
		"Break must keep forwarding connections opened after it — #72's recovery scenarios depend on it")

	target.push(t, "reply")
	expectSilence(t, old, silenceWindow)
	expectDelivery(t, fresh, "reply")
	assert.NotContains(t, target.received(), "old-after",
		"the connection Break silenced must stay silenced")
}

// TestRelay_StopClosesWhatBreakAllLeftHanging — stop is the only thing that ends
// a silenced connection, so it has to actually end it. Every silenced pair holds
// two goroutines parked in Read on sockets that nobody will ever write to or
// close on their own; leaving one behind strands them for the rest of the test
// binary's life, and a package run under -count=5 accumulates them.
func TestRelay_StopClosesWhatBreakAllLeftHanging(t *testing.T) {
	target := startEchoTarget(t)

	// Baseline AFTER the target is up: the target's goroutines are not the
	// relay's to clean up and outlive stop by design. Everything the relay adds
	// on top — its accept loop, two pipes per connection, and the target-side
	// reader its upstream dial created — must be gone once stop returns.
	baseline := runtime.NumGoroutine()

	relay := Start(t, target.addr)
	conn := dialRelay(t, relay.Addr)
	send(t, conn, "hello")
	require.Eventually(t, func() bool { return strings.Contains(target.received(), "hello") },
		2*time.Second, 10*time.Millisecond)

	relay.BreakAll()
	relay.stop()

	require.NoError(t, conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, err := conn.Read(make([]byte, 16))
	require.Error(t, err, "stop must close the client socket BreakAll left silent")
	var netErr net.Error
	require.False(t, errors.As(err, &netErr) && netErr.Timeout(),
		"the socket was still open after stop: the read timed out instead of reporting a closed connection")

	// Polled by hand rather than with require.Eventually, which runs its
	// condition in a goroutine of its own and adds a ticker beside it: counting
	// goroutines from inside it counts the counter, and the assertion can never
	// come back to the baseline it is comparing against.
	live := runtime.NumGoroutine()
	for deadline := time.Now().Add(2 * time.Second); live > baseline && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
		live = runtime.NumGoroutine()
	}
	assert.LessOrEqual(t, live, baseline,
		"stop must leave no relay goroutine behind: its accept loop, two pipes per connection, "+
			"and the target-side reader its upstream dial created")
}
