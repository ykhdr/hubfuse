package agent

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	pb "github.com/ykhdr/hubfuse/proto"
)

// ─── isKeepalivePunished ────────────────────────────────────────────────────────

// TestIsKeepalivePunished pins the classifier to a plain substring match on
// err.Error(). The first case is the verbatim text measured from a live
// Subscribe stream's Recv against a hub running grpc-go's default
// EnforcementPolicy (MinTime: 5m) — see isKeepalivePunished's comment for why
// a substring match, rather than a typed check, is the correct one here.
func TestIsKeepalivePunished(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "verbatim GOAWAY too_many_pings error from a punished transport",
			err: errors.New(`rpc error: code = Unavailable desc = closing transport due to: ` +
				`connection error: desc = "error reading from server: EOF", received prior goaway: ` +
				`code: ENHANCE_YOUR_CALM, debug data: "too_many_pings"`),
			want: true,
		},
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
		{
			name: "ordinary Unavailable error unrelated to keepalive enforcement",
			err:  status.Error(codes.Unavailable, "transport is closing"),
			want: false,
		},
		{
			name: "context cancellation",
			err:  context.Canceled,
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isKeepalivePunished(tc.err))
		})
	}
}

// ─── A hub that answers, over a real connection ───────────────────────────────

// stubHub is the smallest hub a HubClient can genuinely talk to. The tests below
// are about which *connection* an operation runs on, so the connection has to be
// real: a HubClient built around a nil grpc.ClientConn would panic in Close, and
// a fake pb.HubFuseClient would never show that a conn was closed underneath a
// call in flight.
type stubHub struct {
	pb.UnimplementedHubFuseServer
	deregisters atomic.Int32
}

func (s *stubHub) Deregister(context.Context, *pb.DeregisterRequest) (*pb.DeregisterResponse, error) {
	s.deregisters.Add(1)
	return &pb.DeregisterResponse{Success: true}, nil
}

// startStubHub serves stubHub on a local port for the duration of the test.
func startStubHub(t *testing.T) (*stubHub, string) {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "listen")

	hub := &stubHub{}
	srv := grpc.NewServer()
	pb.RegisterHubFuseServer(srv, hub)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	return hub, lis.Addr().String()
}

// dialStubHub builds a HubClient over a plain (no-TLS) connection to addr. The
// TLS handshake is not what these tests are about, and grpc.NewClient is lazy
// either way, so nothing connects until the first RPC.
func dialStubHub(t *testing.T, addr string) *HubClient {
	t.Helper()

	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err, "NewClient")

	return &HubClient{conn: conn, client: pb.NewHubFuseClient(conn), logger: discardLogger()}
}

// ─── The swap mechanics ───────────────────────────────────────────────────────

// The connection moved under sessionMu in #77 precisely so it could be replaced
// while the daemon runs. Every read goes through hub(), so a reader concurrent
// with a swap must be safe — with -race this is the test that says so.
func TestHubAccessorAndSwapDoNotRace(t *testing.T) {
	t.Parallel()

	_, addr := startStubHub(t)
	first := dialStubHub(t, addr)
	second := dialStubHub(t, addr)
	t.Cleanup(func() { _ = first.Close(); _ = second.Close() })

	d := &Daemon{logger: discardLogger()}
	d.setHubClient(first)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 200 {
				// Every reader must see one of the two clients and never a
				// torn or nil pointer.
				got := d.hub()
				if got != first && got != second {
					t.Errorf("hub() returned an unknown client %p", got)
					return
				}
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		condemned, fresh := first, second
		for range 200 {
			if _, ok := d.swapHubClient(condemned, fresh); ok {
				condemned, fresh = fresh, condemned
			}
		}
	}()

	wg.Wait()
}

// A swap names the client it condemns, and the CAS is the whole point: a daemon
// that condemned connection A must never take down the B that replaced it in
// between — B may be perfectly healthy and is provably not the one that failed.
func TestSwapHubClientOnlyReplacesTheCondemnedClient(t *testing.T) {
	t.Parallel()

	_, addr := startStubHub(t)
	installed := dialStubHub(t, addr)
	other := dialStubHub(t, addr)
	fresh := dialStubHub(t, addr)
	t.Cleanup(func() { _ = installed.Close(); _ = other.Close(); _ = fresh.Close() })

	d := &Daemon{logger: discardLogger()}
	d.setHubClient(installed)

	old, swapped := d.swapHubClient(other, fresh)
	assert.False(t, swapped, "a swap condemning a client that is not installed must not happen")
	assert.Nil(t, old, "nothing to close when the swap did not happen")
	assert.Same(t, installed, d.hub(), "the installed client must survive a stale condemnation")

	old, swapped = d.swapHubClient(installed, fresh)
	require.True(t, swapped, "condemning the installed client must swap")
	assert.Same(t, installed, old, "the caller must be handed the old client to close")
	assert.Same(t, fresh, d.hub(), "the fresh client must be installed")
}

// Once Shutdown has claimed the connection it owns it outright: a swap landing
// afterwards must decline, so the client Shutdown is about to Deregister on is
// never closed underneath it. The swapper closes the connection it dialled.
func TestSwapHubClientDeclinesOnceShutdownClaimed(t *testing.T) {
	t.Parallel()

	_, addr := startStubHub(t)
	installed := dialStubHub(t, addr)
	fresh := dialStubHub(t, addr)
	t.Cleanup(func() { _ = installed.Close(); _ = fresh.Close() })

	d := &Daemon{logger: discardLogger()}
	d.setHubClient(installed)

	claimed := d.takeHubClientForShutdown()
	assert.Same(t, installed, claimed, "shutdown takes the installed client")

	old, swapped := d.swapHubClient(installed, fresh)
	assert.False(t, swapped, "no swap may land after shutdown claimed the connection")
	assert.Nil(t, old)
	assert.Same(t, installed, d.hub(), "the claimed client stays put")
}

// ─── Shutdown ─────────────────────────────────────────────────────────────────

// Shutdown must Deregister and Close the SAME client, and it must be the one
// installed at the time — including after a mid-life replacement. This is the
// #69 contract restated for a mutable connection: a Deregister that comes back
// with ErrClientConnClosing aggregates into a non-zero exit for `hubfuse stop`
// on a perfectly healthy stop.
func TestShutdownUsesTheCurrentClientForDeregisterAndClose(t *testing.T) {
	hub, addr := startStubHub(t)

	d, _ := buildTestDaemon(t)
	replaced := dialStubHub(t, addr)
	current := dialStubHub(t, addr)
	d.setHubClient(replaced)

	old, swapped := d.swapHubClient(replaced, current)
	require.True(t, swapped, "swap")
	require.NoError(t, old.Close(), "closing the replaced client")

	require.NoError(t, d.Shutdown(), "a healthy stop must not report errors")
	assert.Equal(t, int32(1), hub.deregisters.Load(), "exactly one deregistration, on the live client")

	// Close is idempotent-ish in gRPC (a second Close returns ErrClientConnClosing),
	// which is how the test tells that Shutdown really closed the client it used.
	assert.Error(t, current.Close(), "Shutdown must have closed the client it deregistered on")
}

// A swap racing Shutdown must not cost the daemon its clean exit: the swap
// declines, Shutdown keeps deregistering on the client it claimed, and the
// aggregate stays empty.
func TestShutdownSurvivesASwapRacingIt(t *testing.T) {
	hub, addr := startStubHub(t)

	d, _ := buildTestDaemon(t)
	installed := dialStubHub(t, addr)
	fresh := dialStubHub(t, addr)
	d.setHubClient(installed)

	// The swapper does what replaceHubClient does when it loses the race:
	// it closes the connection it dialled and leaves the installed one alone.
	start := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-start
		if old, swapped := d.swapHubClient(installed, fresh); swapped {
			_ = old.Close()
		} else {
			_ = fresh.Close()
		}
	}()

	close(start)
	err := d.Shutdown()
	<-done

	require.NoError(t, err, "a swap racing shutdown must not make the stop fail")
	assert.Equal(t, int32(1), hub.deregisters.Load(), "the deregistration still happened")
}
