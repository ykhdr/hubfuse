package hub

import (
	"context"

	"google.golang.org/grpc"

	"github.com/ykhdr/hubfuse/internal/common"
	pb "github.com/ykhdr/hubfuse/proto"
)

// DrainUnaryInterceptor refuses new unary RPCs once the hub has started
// shutting down.
//
// It is what keeps the shutdown sequence from racing its own clients. Closing
// every subscriber channel (CloseAllSubscribers) ends each Subscribe stream, and
// the agents on the other side reconnect immediately — their backoff starts at
// one second, well inside the shutdown budget. Without this guard those
// reconnects would re-enter Register and Subscribe on a hub that has already
// swept its devices offline and already closed its subscriptions, so the new
// stream would have nothing left to close it and GracefulStop would wait for it
// all over again.
//
// Join is guarded too, and for a different reason: its first act is to consume a
// single-use join token. A dying hub that burns the token still issues a
// certificate whose Register the next hub cannot honour, leaving the operator to
// issue a fresh token for a device that appeared to join successfully.
//
// ErrHubShuttingDown is already a codes.Unavailable status, so the caller sees a
// real gRPC status rather than an application-level Success=false. (#75)
func DrainUnaryInterceptor(registry *Registry) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req interface{}, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
		if registry.Draining() {
			return nil, common.ErrHubShuttingDown
		}
		return handler(ctx, req)
	}
}

// DrainStreamInterceptor refuses new streaming RPCs once the hub has started
// shutting down. Subscribe is the only streaming method, and it is the one that
// matters most here — see DrainUnaryInterceptor. (#75)
func DrainStreamInterceptor(registry *Registry) grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if registry.Draining() {
			return common.ErrHubShuttingDown
		}
		return handler(srv, ss)
	}
}

// AuthUnaryInterceptor is a gRPC unary server interceptor that enforces
// authentication for all methods except Join. Authentication is satisfied by
// the presence of a valid mTLS client certificate whose CN is the device_id.
func AuthUnaryInterceptor(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	if info.FullMethod == pb.HubFuse_Join_FullMethodName {
		return handler(ctx, req)
	}

	if _, err := common.ExtractDeviceID(ctx); err != nil {
		return nil, common.ErrNotAuthenticated
	}

	return handler(ctx, req)
}

// AuthStreamInterceptor is a gRPC stream server interceptor that enforces
// authentication for all streaming methods. (Join is unary, so it is never
// seen here, but the guard is kept consistent.)
func AuthStreamInterceptor(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if info.FullMethod == pb.HubFuse_Join_FullMethodName {
		return handler(srv, ss)
	}

	if _, err := common.ExtractDeviceID(ss.Context()); err != nil {
		return common.ErrNotAuthenticated
	}

	return handler(srv, ss)
}
