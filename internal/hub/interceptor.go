package hub

import (
	"context"

	"google.golang.org/grpc"

	"github.com/ykhdr/hubfuse/internal/common"
	pb "github.com/ykhdr/hubfuse/proto"
)

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
