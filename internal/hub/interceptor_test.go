package hub

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ykhdr/hubfuse/internal/common"
	pb "github.com/ykhdr/hubfuse/proto"
)

// TestDrainUnaryInterceptor_PassesThroughBeforeDrain — the guard must be
// invisible during normal operation. Every unary RPC the hub serves goes through
// it, so a guard that is even slightly too eager takes the whole hub down with
// it.
func TestDrainUnaryInterceptor_PassesThroughBeforeDrain(t *testing.T) {
	r := newTestRegistry(t)
	interceptor := DrainUnaryInterceptor(r)

	called := false
	resp, err := interceptor(context.Background(), "req",
		&grpc.UnaryServerInfo{FullMethod: pb.HubFuse_Register_FullMethodName},
		func(context.Context, interface{}) (interface{}, error) {
			called = true
			return "resp", nil
		})

	require.NoError(t, err)
	assert.Equal(t, "resp", resp)
	assert.True(t, called, "the handler must run while the hub is serving")
}

// TestDrainUnaryInterceptor_RefusesAfterDrain pins the contract the reconnect
// storm depends on. Closing every subscription (Hub.Stop) makes each agent
// reconnect within a second, and those reconnects must not be allowed to
// re-register on a hub that has already swept its devices offline.
//
// The refusal must arrive as a gRPC status, not as an application-level
// Success=false: the agent distinguishes "hub said no" from "hub is gone" by the
// status code, and codes.Unavailable is what makes it back off and retry rather
// than treat the answer as final.
func TestDrainUnaryInterceptor_RefusesAfterDrain(t *testing.T) {
	r := newTestRegistry(t)
	interceptor := DrainUnaryInterceptor(r)

	r.Drain()

	called := false
	_, err := interceptor(context.Background(), "req",
		&grpc.UnaryServerInfo{FullMethod: pb.HubFuse_Register_FullMethodName},
		func(context.Context, interface{}) (interface{}, error) {
			called = true
			return "resp", nil
		})

	require.ErrorIs(t, err, common.ErrHubShuttingDown)
	assert.Equal(t, codes.Unavailable, status.Code(err),
		"a draining hub must refuse with Unavailable, not an application-level error")
	assert.False(t, called, "the handler must not run on a draining hub")
}

// TestDrainUnaryInterceptor_RefusesJoinAfterDrain — Join is unauthenticated and
// therefore skips the auth interceptor, which is exactly why it needs its own
// guard. Its first act is to consume a single-use join token: a hub that burns
// the token on its way out issues a certificate whose Register the next hub will
// reject, and the operator has to issue a fresh token for a device that appeared
// to join successfully.
func TestDrainUnaryInterceptor_RefusesJoinAfterDrain(t *testing.T) {
	r := newTestRegistry(t)
	interceptor := DrainUnaryInterceptor(r)

	r.Drain()

	called := false
	_, err := interceptor(context.Background(), "req",
		&grpc.UnaryServerInfo{FullMethod: pb.HubFuse_Join_FullMethodName},
		func(context.Context, interface{}) (interface{}, error) {
			called = true
			return "resp", nil
		})

	require.ErrorIs(t, err, common.ErrHubShuttingDown)
	assert.False(t, called, "a draining hub must not consume a join token")
}

// TestDrainStreamInterceptor_PassesThroughBeforeDrain — same invisibility
// requirement as the unary guard, for the one stream the hub serves.
func TestDrainStreamInterceptor_PassesThroughBeforeDrain(t *testing.T) {
	r := newTestRegistry(t)
	interceptor := DrainStreamInterceptor(r)

	called := false
	err := interceptor(nil, nil,
		&grpc.StreamServerInfo{FullMethod: pb.HubFuse_Subscribe_FullMethodName},
		func(interface{}, grpc.ServerStream) error {
			called = true
			return nil
		})

	require.NoError(t, err)
	assert.True(t, called, "the handler must run while the hub is serving")
}

// TestDrainStreamInterceptor_RefusesAfterDrain is the guard that makes
// CloseAllSubscribers a one-shot operation rather than a race. A Subscribe
// accepted after the subscriptions were closed would register a channel nobody
// will ever close, and GracefulStop would wait on that stream for as long as its
// client stayed alive — reintroducing the very hang this issue is about.
func TestDrainStreamInterceptor_RefusesAfterDrain(t *testing.T) {
	r := newTestRegistry(t)
	interceptor := DrainStreamInterceptor(r)

	r.Drain()

	called := false
	err := interceptor(nil, nil,
		&grpc.StreamServerInfo{FullMethod: pb.HubFuse_Subscribe_FullMethodName},
		func(interface{}, grpc.ServerStream) error {
			called = true
			return nil
		})

	require.ErrorIs(t, err, common.ErrHubShuttingDown)
	assert.Equal(t, codes.Unavailable, status.Code(err))
	assert.False(t, called, "no new subscription may open on a draining hub")
}
