package agent

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/hub/hubtest"
	"github.com/ykhdr/hubfuse/internal/hub/store"
	pb "github.com/ykhdr/hubfuse/proto"
	"github.com/ykhdr/hubfuse/tests/testrelay"
	"github.com/ykhdr/hubfuse/tests/testwedge"
)

// This file drives reconnectSession against a REAL hub, over a REAL socket, in
// package agent — not with the fakes swap_test.go uses. That is on purpose:
// swap_test.go pins the classifier's decisions against a fully-controlled fake
// transport, and these tests pin the same decisions against the actual gRPC
// stack (testwedge, an in-process hub façade with the production server
// options, and testrelay, a TCP forwarder), so a change to gRPC's own behaviour
// — not just to this package's code — would still be caught. Package agent is
// required because the assertions are about unexported state (reconnectSession,
// d.hub(), registerTimeout). There is no import cycle: internal/hub/** imports
// nothing from internal/agent. (#77)

// newWireTestDaemon builds a *Daemon wired with the PRODUCTION seams —
// registerFn/subscribeFn/heartbeatFn delegate through d.hub(), dialFn is the
// real Connector.Dial — talking to target over real mTLS, and drives one
// successful session against it before returning. That first session is what
// makes the connection "seen" by a testwedge.Wedge (only a connection that has
// already carried an RPC is wedgeable) and it seeds the device row Register
// needs either way.
//
// The initial session runs on a context this function owns and cancels at test
// end, so the heartbeat loop it starts (sessionOnce → startHeartbeat, guarded by
// a sync.Once for the daemon's lifetime) does not keep beating — and logging
// failures — against a hub or relay the test has already torn down.
func newWireTestDaemon(t *testing.T, h *hubtest.Harness, target string) *Daemon {
	t.Helper()

	const deviceID = "wedge-wire-dev"
	require.NoError(t, h.Store.CreateDevice(context.Background(), &store.Device{
		DeviceID: deviceID,
		Nickname: "wedge-wire",
		Status:   store.StatusOffline,
	}), "seed the device row Register needs")

	certPEM, keyPEM, err := common.SignClientCert(h.CACert, h.CAKey, deviceID)
	require.NoError(t, err, "SignClientCert")

	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.crt")
	certPath := filepath.Join(dir, "client.crt")
	keyPath := filepath.Join(dir, "client.key")
	require.NoError(t, os.WriteFile(caPath, h.CAPEM, 0o600), "write ca")
	require.NoError(t, os.WriteFile(certPath, certPEM, 0o600), "write cert")
	require.NoError(t, os.WriteFile(keyPath, keyPEM, 0o600), "write key")

	connector := NewConnector(target, caPath, certPath, keyPath, discardLogger())

	d, _ := buildTestDaemon(t)
	// Tiny, not zero: reconnectSession floors a non-positive value to
	// backoffInitial (1s) as its retry backoff, which would make every test
	// here pay at least a second per retry round for nothing.
	d.minReconnectInterval = time.Millisecond

	initial, err := connector.Dial()
	require.NoError(t, err, "initial Dial")
	d.setHubClient(initial)

	// Exactly the wiring NewDaemon does — no fakes anywhere in this file.
	d.registerFn = func(ctx context.Context, s []*pb.Share, p int) (*pb.RegisterResponse, error) {
		return d.hub().Register(ctx, s, p)
	}
	d.subscribeFn = func(ctx context.Context) (pb.HubFuse_SubscribeClient, error) {
		return d.hub().Subscribe(ctx)
	}
	d.heartbeatFn = func(ctx context.Context) error {
		return d.hub().Heartbeat(ctx)
	}
	d.dialFn = connector.Dial

	fixtureCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	stream, err := d.sessionOnce(fixtureCtx)
	require.NoError(t, err, "initial sessionOnce must succeed before any wedge/break")
	require.NotNil(t, stream)

	t.Cleanup(func() {
		if c := d.hub(); c != nil {
			_ = c.Close()
		}
	})

	return d
}

// countingDial wraps dial with a call counter, so a test can assert exactly how
// many replacement connections reconnectSession actually dialled.
func countingDial(dial func() (*HubClient, error)) (func() (*HubClient, error), *atomic.Int32) {
	var n atomic.Int32
	return func() (*HubClient, error) {
		n.Add(1)
		return dial()
	}, &n
}

// countingRegister wraps register with a call counter and a failure counter, so
// a test can assert both how many rounds the retry loop ran and that at least
// one of them genuinely failed (a test that never entered the loop must not
// pass by accident).
func countingRegister(
	register func(ctx context.Context, s []*pb.Share, p int) (*pb.RegisterResponse, error),
) (func(ctx context.Context, s []*pb.Share, p int) (*pb.RegisterResponse, error), *atomic.Int32, *atomic.Int32) {
	var calls, failures atomic.Int32
	wrapped := func(ctx context.Context, s []*pb.Share, p int) (*pb.RegisterResponse, error) {
		calls.Add(1)
		resp, err := register(ctx, s, p)
		if err != nil {
			failures.Add(1)
		}
		return resp, err
	}
	return wrapped, &calls, &failures
}

// TestWedgeWire_RecoversOnADifferentConnection is issue #77's whole point, on a
// real socket: a wedged-but-live connection must not strand the session
// forever, and the only way out the daemon has is a NEW connection.
func TestWedgeWire_RecoversOnADifferentConnection(t *testing.T) {
	// No t.Parallel(): shortenBudget rewrites the package-level registerTimeout.
	h := hubtest.StartTestHub(t)
	w := testwedge.Start(t, h)
	d := newWireTestDaemon(t, h, w.Addr)
	before := d.hub()

	w.Wedge()
	shortenBudget(t, &registerTimeout, 300*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	stream := d.reconnectSession(ctx)
	require.NotNil(t, stream,
		"a wedged-but-live connection must not strand the session forever — that is the failure #77 is about")
	assert.NotSame(t, before, d.hub(),
		"recovery must land on a DIFFERENT connection than the one the wedge silenced")
}

// TestWedgeWire_WedgeAllCostsExactlyOneReplacement is the damping bound on a
// real socket: a wedge no new connection escapes must still cost exactly one
// replacement dial, not one per retry round, or an outage the daemon cannot
// possibly recover from becomes a dial storm against an already-sick hub.
func TestWedgeWire_WedgeAllCostsExactlyOneReplacement(t *testing.T) {
	// No t.Parallel(): shortenBudget rewrites the package-level registerTimeout.
	h := hubtest.StartTestHub(t)
	w := testwedge.Start(t, h)
	d := newWireTestDaemon(t, h, w.Addr)

	dialFn, dials := countingDial(d.dialFn)
	d.dialFn = dialFn
	registerFn, registers, failures := countingRegister(d.registerFn)
	d.registerFn = registerFn

	w.WedgeAll()
	shortenBudget(t, &registerTimeout, 100*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	stream := d.reconnectSession(ctx)
	assert.Nil(t, stream,
		"WedgeAll silences every connection, including replacements, so no session can ever come up")
	assert.Equal(t, int32(1), dials.Load(),
		"a wedge no new connection escapes must still cost exactly one replacement, not one per round")
	assert.GreaterOrEqual(t, registers.Load(), int32(3),
		"the loop must have kept retrying well past the single replacement")
	assert.GreaterOrEqual(t, failures.Load(), int32(3), "every one of those retries must have genuinely failed")
}

// TestWedgeWire_WedgeFastCausesNoReplacement is the negative that keeps the
// classifier honest: a hub that answers — even with codes.DeadlineExceeded — is
// a hub whose connection demonstrably works. A daemon that replaced it here
// would be reading the status code instead of asking whose deadline expired.
func TestWedgeWire_WedgeFastCausesNoReplacement(t *testing.T) {
	// No t.Parallel(): this test shares a package with others that rewrite
	// registerTimeout, so it stays off t.Parallel() too even though it does not
	// touch the var itself.
	h := hubtest.StartTestHub(t)
	w := testwedge.Start(t, h)
	d := newWireTestDaemon(t, h, w.Addr)
	before := d.hub()

	dialFn, dials := countingDial(d.dialFn)
	d.dialFn = dialFn
	registerFn, registers, _ := countingRegister(d.registerFn)
	d.registerFn = registerFn

	w.WedgeFast()

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()

	stream := d.reconnectSession(ctx)
	assert.Nil(t, stream, "a hub that only ever refuses fast never produces a session")
	assert.GreaterOrEqual(t, registers.Load(), int32(3),
		"the loop must have retried several rounds against a hub that keeps answering")
	assert.Zero(t, dials.Load(),
		"a hub that answers proves the connection works; replacing it means the classifier read the "+
			"status code instead of asking whose deadline expired")
	assert.Same(t, before, d.hub(), "the connection must be unchanged")
}

// TestWedgeWire_SilentBreakCausesNoReplacement is the negative for an ORDINARY
// disconnect, joined on a real socket: a transport nobody answers must be left
// to gRPC's own keepalive-driven repair, never replaced by the daemon.
//
// The ordering invariant that makes this true (hubKeepaliveTime +
// hubKeepaliveTimeout < defaultRegisterTimeout, pinned by
// TestRegisterBudgetOutlastsKeepaliveDetection in bounds_test.go) means
// keepalive kills the dead transport and the call comes back Unavailable
// BEFORE the daemon's own 20s budget can expire — so registerTimeout is left at
// its production value here on purpose, not shortened like the tests above.
// Without that ordering, every wifi drop and every laptop sleep — the commonest
// failures on this hardware — would start replacing the connection, and a
// replaced channel returns to IDLE and pays the full 20s connect budget again
// on every subsequent attempt instead of self-correcting at 0ms.
//
// This test therefore takes roughly 15-25s (keepalive detection alone is ~15s)
// and deliberately does not call t.Parallel().
func TestWedgeWire_SilentBreakCausesNoReplacement(t *testing.T) {
	h := hubtest.StartTestHub(t)
	relay := testrelay.Start(t, h.Addr)
	d := newWireTestDaemon(t, h, relay.Addr)
	before := d.hub()

	dialFn, dials := countingDial(d.dialFn)
	d.dialFn = dialFn
	registerFn, _, failures := countingRegister(d.registerFn)
	d.registerFn = registerFn

	// Break() silences only the connection that already exists — the one the
	// daemon's fixture session is running on — while leaving the relay able to
	// forward brand new connections normally. That is exactly the shape gRPC's
	// own transparent reconnect needs to self-heal the SAME *HubClient once
	// keepalive notices the old transport is dead.
	relay.Break()

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()

	stream := d.reconnectSession(ctx)
	require.NotNil(t, stream,
		"keepalive detects the break well inside the register budget, so gRPC's own reconnect must be "+
			"what heals the session")
	assert.GreaterOrEqual(t, failures.Load(), int32(1),
		"at least one Register attempt must have run — and failed — on the broken connection, or this "+
			"test proves nothing")
	assert.Zero(t, dials.Load(),
		"a transport keepalive kills gives Unavailable, our budget never expires, and the daemon must "+
			"keep the connection gRPC is already repairing")
	assert.Same(t, before, d.hub(),
		"the connection identity must not change: the same *HubClient recovers, redialled internally by "+
			"gRPC through the relay")
}
