package integration

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ykhdr/hubfuse/internal/hub/hubtest"
	pb "github.com/ykhdr/hubfuse/proto"
	"github.com/ykhdr/hubfuse/tests/testrelay"
)

// TestIntegration_ClientDetectsSilentlyDeadConnection is the regression for
// issue #72: a hub connection that stops carrying traffic without either side
// being told. Before keepalive, an RPC on such a connection blocked in
// grpc's stream setup forever — the daemon kept "running", the hub demoted the
// device 30 seconds later, and every peer unmounted its shares permanently.
//
// The relay reproduces the failure exactly: no socket is closed, so nothing
// errors on its own; the only thing that can notice is a keepalive ping going
// unanswered.
func TestIntegration_ClientDetectsSilentlyDeadConnection(t *testing.T) {
	h := hubtest.StartTestHub(t)
	relay := testrelay.Start(t, h.Addr)
	ctx := context.Background()

	// The device joins directly and then talks to the hub through the relay,
	// which is the connection under test.
	client := joinAgentClientAt(t, h, relay.Addr, "relayed-dev", "relayed")

	_, err := client.Register(ctx, []*pb.Share{{Alias: "docs", Permissions: "ro"}}, 2222)
	require.NoError(t, err, "register through a healthy relay")
	require.NoError(t, client.Heartbeat(ctx), "heartbeat through a healthy relay")

	// A live subscription makes this the daemon's real shape — and is what used
	// to hide the failure, since the stream never ends on its own.
	subCtx, cancelSub := context.WithCancel(ctx)
	t.Cleanup(cancelSub)
	stream, err := client.Subscribe(subCtx)
	require.NoError(t, err, "subscribe through a healthy relay")
	ready, err := stream.Recv()
	require.NoError(t, err, "subscribe ready")
	require.NotNil(t, ready.GetSubscribeReady())

	relay.Break()

	// Budgeted so a regression still produces this test's assertion rather than
	// a package-wide timeout panic: detection takes ~15s, the whole test has to
	// fit inside `make test-integration`'s 120s alongside the other tests.
	callCtx, cancelCall := context.WithTimeout(ctx, 45*time.Second)
	defer cancelCall()

	start := time.Now()
	err = client.Heartbeat(callCtx)
	elapsed := time.Since(start)

	require.Error(t, err, "a heartbeat on a dead connection must fail, not hang")
	require.NoError(t, callCtx.Err(),
		"the call must end because keepalive killed the transport, not because the test's deadline expired")
	assert.Less(t, elapsed, 30*time.Second,
		"detection must fit inside the hub's liveness timeout budget, got %s", elapsed)

	// The event stream must die too — that is the signal the daemon's
	// supervisor acts on to re-register.
	streamErr := make(chan error, 1)
	go func() {
		_, recvErr := stream.Recv()
		streamErr <- recvErr
	}()
	select {
	case recvErr := <-streamErr:
		require.Error(t, recvErr, "the event stream must end when the transport dies")
	case <-time.After(20 * time.Second):
		t.Fatal("the event stream never ended; the supervisor would never reconnect")
	}
}

// TestIntegration_KeepalivePingsAreNotPunished pins the other half of the
// contract: the hub's enforcement policy must tolerate the agent's ping
// cadence. grpc-go's default punishes anything more frequent than one ping per
// five minutes with GOAWAY too_many_pings, which would turn the fix into a
// disconnect loop.
func TestIntegration_KeepalivePingsAreNotPunished(t *testing.T) {
	if testing.Short() {
		t.Skip("waits out several keepalive intervals")
	}

	h := hubtest.StartTestHub(t)
	ctx := context.Background()

	client := joinAgentClientAt(t, h, h.Addr, "pinger-dev", "pinger")
	_, err := client.Register(ctx, nil, 2222)
	require.NoError(t, err, "register")

	subCtx, cancelSub := context.WithCancel(ctx)
	t.Cleanup(cancelSub)
	stream, err := client.Subscribe(subCtx)
	require.NoError(t, err, "subscribe")
	_, err = stream.Recv()
	require.NoError(t, err, "subscribe ready")

	// Idle for several ping intervals: the client pings, the hub must answer
	// rather than tear the connection down.
	time.Sleep(25 * time.Second)

	require.NoError(t, client.Heartbeat(ctx),
		"the connection must survive an idle period spanning several keepalive pings")
}
