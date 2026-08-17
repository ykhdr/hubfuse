// Package testwedge provides an in-process hub façade whose RPCs can be made to
// stop returning while its transport stays perfectly healthy — the failure
// issue #77 is about.
//
// It is the application-level counterpart to tests/testrelay. The relay breaks
// the wire: bytes stop flowing, so gRPC keepalive eventually notices and kills
// the transport. A wedge breaks nothing on the wire — HTTP/2 PINGs are answered
// by the transport layer as usual, the event stream keeps its socket, and only
// the calls themselves go nowhere. That combination is what gRPC will not repair
// on its own and what a TCP forwarder physically cannot express: inside TLS a
// PING frame and a request frame are indistinguishable.
//
// The fixture is deliberately connection-scoped, and honest about what that
// proves: Wedge() declares the failure connection-scoped by construction (calls
// from connections seen so far hang, calls from new ones are served), so a test
// built on it shows that IF a wedge is connection-scoped the daemon must be able
// to escape it by dialling again — not that a production wedge is of that kind.
// WedgeAll() is the opposite bound: a wedge no new connection escapes, which is
// what keeps the daemon's swap budget honest.
package testwedge

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/hub"
	"github.com/ykhdr/hubfuse/internal/hub/hubtest"
	pb "github.com/ykhdr/hubfuse/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// Wedge is a second gRPC front end for an existing hubtest.Harness: same
// registry, same store, same production server options (hub.ServerOptions), so
// a test that points an agent at Wedge.Addr exercises the real transport and the
// real service — only the answering is under the test's control.
//
// The zero value is not usable; call Start.
type Wedge struct {
	// Addr is the wedge's listen address; point the agent's hub address here.
	Addr string

	mu sync.Mutex
	// seen holds the peer address of every connection that has carried at
	// least one RPC. A connection only becomes wedgeable once it has been
	// seen, which is what makes "the connection the daemon already had" a
	// set the test can name at the moment it calls Wedge.
	seen map[string]struct{}
	// wedged holds the peer addresses whose calls must never return.
	wedged map[string]struct{}
	// all wedges every connection, present and future (WedgeAll).
	all bool
	// fast answers instantly with codes.DeadlineExceeded (WedgeFast). It is
	// NOT a model of a wedge: a hub that answers is a hub whose connection
	// demonstrably works. It exists so a test can prove the daemon does not
	// condemn a connection just because a DeadlineExceeded came back.
	fast bool

	// release lets Start's cleanup unblock every parked handler goroutine
	// directly, instead of resting on the assumption that srv.Stop cancels each
	// handler's own context. It is the fixture's whole purpose to have handlers
	// that never return on their own, so a second, explicit way out is worth
	// having even though Stop is expected to cancel them too.
	release     chan struct{}
	releaseOnce sync.Once
}

// Start brings up a wedgeable front end for h on a fresh local port, serving the
// harness's own registry over the harness's TLS material. It is stopped when the
// test ends.
func Start(t *testing.T, h *hubtest.Harness) *Wedge {
	t.Helper()

	tlsCfg, err := common.LoadTLSServerConfig(
		filepath.Join(h.TLSDir, common.CACertFile),
		filepath.Join(h.TLSDir, common.ServerCertFile),
		filepath.Join(h.TLSDir, common.ServerKeyFile),
	)
	if err != nil {
		t.Fatalf("testwedge: LoadTLSServerConfig: %v", err)
	}

	w := &Wedge{
		seen:    map[string]struct{}{},
		wedged:  map[string]struct{}{},
		release: make(chan struct{}),
	}

	// The production options already install the drain and identity
	// interceptors, so the wedge chains onto them rather than replacing them.
	// It runs after both, which is what a real hub stall would look like too:
	// a hub that is neither draining nor rejecting the caller, just not
	// answering. ServerOptions takes the registry because the drain guard reads
	// it (#75); passing the harness's own means a wedged hub still refuses new
	// RPCs once it starts shutting down, exactly like production.
	opts := append(hub.ServerOptions(credentials.NewTLS(tlsCfg), h.Registry),
		grpc.ChainUnaryInterceptor(w.unaryInterceptor),
		grpc.ChainStreamInterceptor(w.streamInterceptor),
	)

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	srv := grpc.NewServer(opts...)
	pb.RegisterHubFuseServer(srv, hub.NewServer(h.Registry, logger))

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("testwedge: listen: %v", err)
	}
	w.Addr = lis.Addr().String()

	// Registered before the t.Cleanup(srv.Stop) below — cleanups run LIFO, so
	// this fires AFTER Stop. Stop is expected to cancel every handler's context
	// on its own (that is why Stop and not GracefulStop is used at all), and
	// this is the independent backstop for a parked goroutine that somehow does
	// not unblock from that alone; it must not run BEFORE Stop, or it would stop
	// being a backstop and become the only mechanism actually being tested.
	t.Cleanup(func() { w.releaseOnce.Do(func() { close(w.release) }) })

	go func() { _ = srv.Serve(lis) }()
	// Stop, not GracefulStop: wedged handlers are parked on their caller's
	// context and a graceful stop would wait for them forever.
	t.Cleanup(srv.Stop)

	return w
}

// Wedge silences every connection that has already been used, leaving later ones
// served. This is the connection-scoped failure: the transport stays READY, the
// event stream keeps its socket, and only the calls stop coming back.
func (w *Wedge) Wedge() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for addr := range w.seen {
		w.wedged[addr] = struct{}{}
	}
}

// WedgeAll silences every connection, including ones dialled afterwards — a hub
// that is stuck as a process rather than on one connection. No client action can
// recover from it, which is exactly why it is the fixture that proves the daemon
// stops at one replacement instead of dialling once per retry forever.
func (w *Wedge) WedgeAll() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.all = true
}

// WedgeFast makes every call fail immediately with codes.DeadlineExceeded. The
// hub is answering, so the connection is provably fine; a daemon that replaced
// its connection here would be reading the status code instead of asking whose
// deadline expired.
func (w *Wedge) WedgeFast() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.fast = true
}

// verdict records the caller's connection and reports how this call must be
// answered.
func (w *Wedge) verdict(ctx context.Context) (block, refuse bool) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return false, false
	}
	addr := p.Addr.String()

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.fast {
		return false, true
	}
	if w.all {
		return true, false
	}
	if _, wedged := w.wedged[addr]; wedged {
		return true, false
	}
	w.seen[addr] = struct{}{}
	return false, false
}

// errWedgedFast is the answer WedgeFast gives. It is a real, completed
// round-trip that happens to carry a deadline code.
var errWedgedFast = status.Error(codes.DeadlineExceeded, "testwedge: refused fast")

func (w *Wedge) unaryInterceptor(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	block, refuse := w.verdict(ctx)
	switch {
	case refuse:
		return nil, errWedgedFast
	case block:
		// Park until the caller gives up, OR until the test releases every
		// wedged handler on its own terms rather than trusting ctx.Done() alone
		// to fire. Returning ctx.Err() on that branch keeps the handler honest
		// for the server's own bookkeeping; the caller has long since stopped
		// listening either way.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-w.release:
			return nil, status.Error(codes.Unavailable, "testwedge: released")
		}
	}
	return handler(ctx, req)
}

func (w *Wedge) streamInterceptor(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	block, refuse := w.verdict(ss.Context())
	switch {
	case refuse:
		return errWedgedFast
	case block:
		select {
		case <-ss.Context().Done():
			return ss.Context().Err()
		case <-w.release:
			return status.Error(codes.Unavailable, "testwedge: released")
		}
	}
	return handler(srv, ss)
}
