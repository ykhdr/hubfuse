package integration

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ykhdr/hubfuse/internal/agent"
	"github.com/ykhdr/hubfuse/internal/hub/hubtest"
	pb "github.com/ykhdr/hubfuse/proto"
	"github.com/ykhdr/hubfuse/tests/testwedge"
)

// TestIntegration_WedgeIsConnectionScoped is the WIRE PHYSICS behind issue #77,
// observed on a real socket rather than asserted about a fake one. It proves
// exactly one thing: a wedged RPC and a healthy Subscribe stream can coexist on
// the SAME connection, and only a NEW connection escapes the wedge.
//
// What this test does NOT prove: that a production hub can actually get stuck
// this way. testwedge.Wedge DEFINES the failure as connection-scoped by
// construction (calls from a connection already seen hang; calls from a new one
// are served) and this test then observes that definition holding — so it shows
// "IF a wedge is connection-scoped, a fresh connection is the only way out", not
// "production wedges are of this kind". The daemon-side reaction to that shape
// is exercised separately, with fakes, in internal/agent's #77 tests.
func TestIntegration_WedgeIsConnectionScoped(t *testing.T) {
	h := hubtest.StartTestHub(t)
	w := testwedge.Start(t, h)
	ctx := context.Background()

	// Join through the REAL hub, not the wedge: the wedge has no Join handling
	// of its own, and the device row has to exist in the store or Register is
	// refused regardless of which front end answers it.
	token, _, err := h.Registry.IssueJoinToken(ctx)
	require.NoError(t, err, "IssueJoinToken")

	unauth := dialNoClientCert(t, h)
	joinResp, err := unauth.Join(ctx, &pb.JoinRequest{
		DeviceId:  "wedge-dev",
		Nickname:  "wedge",
		JoinToken: token,
	})
	require.NoError(t, err, "Join RPC")
	require.True(t, joinResp.GetSuccess(), "Join refused: %s", joinResp.GetError())

	dir := t.TempDir()
	caPath := writeTempFile(t, dir, "ca.crt", joinResp.CaCert)
	certPath := writeTempFile(t, dir, "client.crt", joinResp.ClientCert)
	keyPath := writeTempFile(t, dir, "client.key", joinResp.ClientKey)

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	// Talk to the WEDGE address, not the hub's — the wedge is what decides
	// whether a call comes back.
	client, err := agent.DialWithMTLS(w.Addr, caPath, certPath, keyPath, logger)
	require.NoError(t, err, "DialWithMTLS")
	t.Cleanup(func() { _ = client.Close() })

	_, err = client.Register(ctx, nil, 2222)
	require.NoError(t, err, "register before wedging")
	require.NoError(t, client.Heartbeat(ctx), "heartbeat before wedging")

	// A live subscription is what makes this the daemon's real shape, and its
	// socket is the one whose fate the rest of the test is about.
	subCtx, cancelSub := context.WithCancel(ctx)
	t.Cleanup(cancelSub)
	stream, err := client.Subscribe(subCtx)
	require.NoError(t, err, "subscribe before wedging")
	ready, err := stream.Recv()
	require.NoError(t, err, "subscribe ready")
	require.NotNil(t, ready.GetSubscribeReady(), "expected SubscribeReady, got %T", ready.GetPayload())

	// The connection has now carried three RPCs, so it is "seen" and becomes
	// wedgeable.
	w.Wedge()

	// A caller deadline well short of the ~15s keepalive detection floor
	// (hubKeepaliveTime + hubKeepaliveTimeout), so the two are never in the same
	// order of magnitude: the point is that OUR deadline is what ends the call,
	// not the hub's, and 6s leaves headroom for scheduling jitter on a loaded
	// machine without approaching the window keepalive would need to fire.
	const callDeadline = 6 * time.Second
	callCtx, cancelCall := context.WithTimeout(ctx, callDeadline)
	defer cancelCall()

	start := time.Now()
	err = client.Heartbeat(callCtx)
	elapsed := time.Since(start)

	require.Error(t, err, "a heartbeat on a wedged connection must not return")
	// The mirror image of TestIntegration_ClientDetectsSilentlyDeadConnection:
	// there, the CALLER's deadline survives and keepalive kills the transport.
	// Here nothing on the wire is broken, so keepalive never fires — only the
	// caller's own budget can end the call.
	assertEndedOnOurDeadline(t, err, elapsed, callDeadline)
	assert.Less(t, elapsed, callDeadline+3*time.Second, "the call must not outlive its own deadline by much")

	// The load-bearing observation: the event stream must NOT have died. A
	// transport gRPC considers broken kills the stream too (see the relayed
	// test); a wedge kills nothing on the wire, so the stream's socket is
	// exactly as healthy after Wedge() as before it — which is why gRPC will
	// never repair this on its own.
	recvErr := make(chan error, 1)
	go func() {
		_, recvErr2 := stream.Recv()
		recvErr <- recvErr2
	}()
	select {
	case err := <-recvErr:
		t.Fatalf("the event stream must not die while wedged; got %v", err)
	case <-time.After(2 * time.Second):
		// still alive — the transport is healthy, which is exactly why the only
		// way out is a NEW connection.
	}

	// A fresh connection to the same wedge address, with the SAME certificate,
	// reaches the same hub and is untouched by the wedge — because the wedge
	// only silences connections it has already seen.
	fresh, err := agent.DialWithMTLS(w.Addr, caPath, certPath, keyPath, logger)
	require.NoError(t, err, "DialWithMTLS (fresh)")
	t.Cleanup(func() { _ = fresh.Close() })

	freshCtx, cancelFresh := context.WithTimeout(ctx, 5*time.Second)
	defer cancelFresh()
	_, err = fresh.Register(freshCtx, nil, 2222)
	require.NoError(t, err, "register on the fresh connection must succeed quickly")
	require.NoError(t, fresh.Heartbeat(freshCtx), "heartbeat on the fresh connection must succeed quickly")

	// And the old connection is still exactly as stuck as it was — the wedge
	// silences a connection, not a device.
	oldCtx, cancelOld := context.WithTimeout(ctx, callDeadline)
	defer cancelOld()
	oldStart := time.Now()
	err = client.Heartbeat(oldCtx)
	oldElapsed := time.Since(oldStart)
	require.Error(t, err, "the old connection must still fail")
	assertEndedOnOurDeadline(t, err, oldElapsed, callDeadline)
}

// assertEndedOnOurDeadline pins the distinction the whole #77 classifier rests
// on: the call died because OUR budget ran out, not because the hub answered.
//
// It reads the returned error and the time spent, NOT ctx.Err(). Reading the
// context is a race: gRPC runs its own deadline timer, so it can return
// DeadlineExceeded a moment before the caller's goroutine observes ctx.Err()
// as set — which showed up as an intermittent "expected deadline, got nil" at
// exactly 6.004s of a 6s budget.
//
// The pair of assertions is what keeps it strong. A hub that answered
// DeadlineExceeded itself — a real possibility, and the case the classifier
// must NOT treat as a wedge — would come back long before the budget was gone,
// so spending the whole budget is the half that says the deadline was ours.
func assertEndedOnOurDeadline(t *testing.T, err error, elapsed, budget time.Duration) {
	t.Helper()
	assert.Equal(t, codes.DeadlineExceeded, status.Code(err),
		"the call must end on a deadline; got %v", err)
	assert.GreaterOrEqual(t, elapsed, budget,
		"the call must have spent its whole budget — a hub that answered would have returned sooner")
}
