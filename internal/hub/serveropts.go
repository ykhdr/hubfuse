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
// GOAWAY too_many_pings, and the agent pings every 10s
// (agent.hubKeepaliveTime). MinTime is set to half that interval so ordinary
// jitter cannot trip it, and PermitWithoutStream must match the agent: between
// hub sessions there is no open stream, and that is exactly when the connection
// still needs checking.
//
// What removing this option would actually cost is narrower than it looks, and
// was measured rather than reasoned (issue #78). A hub without it does NOT trap
// a running agent in a disconnect loop: 5 minutes of a real daemon against a
// real default-policy hub produced one session, zero reconnects and zero pings.
// The punishment needs THREE pings with no server write in between, and the
// agent's 10s heartbeat prevents that twice over — grpc-go's client skips a
// ping whenever the transport has been read from within the interval, and this
// server zeroes its own strike counter on every response it writes
// (setResetPingStrikes, http2_server.go). Both suppressors need the same thing:
// a live heartbeat.
//
// That is a reason to keep this option, not to drop it. It is what protects the
// connections nothing is heartbeating on — an idle one is punished at ~30s,
// measured — and every punishment monotonically doubles that client's keepalive
// interval (clientconn.go adjustParams), silently eroding the dead-transport
// detection #72 exists to provide. tests/integration/oldhub_test.go pins the
// whole shape against a fixture that keeps grpc-go's default here.
const (
	serverKeepaliveTime    = 15 * time.Second
	serverKeepaliveTimeout = 5 * time.Second
	keepaliveMinTime       = 5 * time.Second
)

// ServerOptions returns the gRPC server options every HubFuse hub is built
// with: the transport credentials, the drain and identity interceptors, and the
// keepalive settings above.
//
// It exists because the hub is constructed in two places — the real one in
// Hub.Start and the in-process one in hubtest — and options that drift apart
// mean the tests exercise a different transport from production. Adding an
// option here reaches both. (#72)
//
// The interceptors are registered with the Chain* variants, and that is not a
// stylistic choice: grpc.UnaryInterceptor and grpc.StreamInterceptor panic when
// set a second time ("the unary server interceptor was already set and may not
// be reset"), so a second option cannot be appended to this slice. Chain order
// is execution order, and the drain guard deliberately runs first — a hub on its
// way out has no interest in who is knocking. (#75)
func ServerOptions(creds credentials.TransportCredentials, registry *Registry) []grpc.ServerOption {
	return []grpc.ServerOption{
		grpc.Creds(creds),
		grpc.ChainUnaryInterceptor(DrainUnaryInterceptor(registry), AuthUnaryInterceptor),
		grpc.ChainStreamInterceptor(DrainStreamInterceptor(registry), AuthStreamInterceptor),
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
