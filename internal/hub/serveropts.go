package hub

import (
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
)

// Hub-side keepalive. The hub pings idle client connections so it notices a
// client that has gone away instead of holding its subscriber slot and writing
// events into a socket nobody will ever read. (#72)
//
// The enforcement policy is the load-bearing half. grpc-go's default punishes a
// client that pings more often than once every 5 minutes with
// GOAWAY too_many_pings, which would turn the agent's 10s keepalive
// (agent.hubKeepaliveTime) into a disconnect loop. MinTime is set to half that
// interval so ordinary jitter cannot trip it, and PermitWithoutStream must
// match the agent: between hub sessions there is no open stream, and that is
// exactly when the connection still needs checking.
const (
	serverKeepaliveTime    = 15 * time.Second
	serverKeepaliveTimeout = 5 * time.Second
	keepaliveMinTime       = 5 * time.Second
)

// ServerOptions returns the gRPC server options every HubFuse hub is built
// with: the transport credentials, the identity interceptors, and the
// keepalive settings above.
//
// It exists because the hub is constructed in two places — the real one in
// Hub.Start and the in-process one in hubtest — and options that drift apart
// mean the tests exercise a different transport from production. Adding an
// option here reaches both. (#72)
func ServerOptions(creds credentials.TransportCredentials) []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.Creds(creds),
		grpc.UnaryInterceptor(AuthUnaryInterceptor),
		grpc.StreamInterceptor(AuthStreamInterceptor),
		grpc.KeepaliveParams(keepalive.ServerParameters{
			Time:    serverKeepaliveTime,
			Timeout: serverKeepaliveTimeout,
		}),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime:             keepaliveMinTime,
			PermitWithoutStream: true,
		}),
	}
}
