// Package testrelay provides a TCP forwarder that test suites put between an
// agent and the hub to reproduce a connection that stops carrying traffic
// without either side being told (issue #72).
package testrelay

import (
	"net"
	"sync"
	"sync/atomic"
	"testing"
)

// relay is a TCP forwarder that can be made to swallow traffic without telling
// either side — the half-open connection from issue #72, reproducible in a test
// without firewalls or privileges.
//
// Break() marks every connection open at that moment as dead: bytes read from
// one side are dropped instead of forwarded, and no socket is closed, so the
// client keeps an ESTABLISHED socket that will never carry an answer again.
// Connections accepted AFTER Break() are forwarded normally, which is what lets
// a test observe recovery as well as the failure.
//
// BreakAll() extends that silence to connections accepted later. The pair mirrors
// testwedge's Wedge()/WedgeAll(), and for the same reason: one fixture bounds a
// failure the daemon can dial its way out of, the other one it cannot.
type Relay struct {
	Addr string

	listener net.Listener
	mu       sync.Mutex
	pairs    []*connPair
	// all silences every connection the relay accepts from now on (BreakAll).
	// It is guarded by mu rather than being an atomic, so that the accept loop
	// can read it in the SAME critical section that appends the new pair: with
	// two separate synchronisations a connection accepted between BreakAll's
	// store and the append would be dead by neither route — BreakAll's loop
	// would not have seen it yet, and the accept loop would have read the flag
	// before it was set. That connection would then carry traffic normally,
	// which is precisely the state BreakAll exists to make unreachable.
	all bool
}

// connPair is one accepted connection and its upstream counterpart. dead marks
// the pair as silenced; both sockets are kept so stop can actually close them —
// leaving them open would strand two goroutines per connection blocked in Read
// for the rest of the test binary's life.
type connPair struct {
	client   net.Conn
	upstream net.Conn
	dead     atomic.Bool
}

// Start listens on a free local port and forwards every connection to target.
// It is stopped automatically when the test ends.
func Start(t *testing.T, target string) *Relay {
	t.Helper()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("relay: listen: %v", err)
	}

	r := &Relay{Addr: lis.Addr().String(), listener: lis}

	go func() {
		for {
			client, acceptErr := lis.Accept()
			if acceptErr != nil {
				return // listener closed
			}
			upstream, dialErr := net.Dial("tcp", target)
			if dialErr != nil {
				_ = client.Close()
				continue
			}

			pair := &connPair{client: client, upstream: upstream}
			r.mu.Lock()
			// Decide this pair's fate under the same lock that records it — see
			// Relay.all for why the two cannot be split.
			pair.dead.Store(r.all)
			r.pairs = append(r.pairs, pair)
			r.mu.Unlock()

			go pipe(upstream, client, &pair.dead)
			go pipe(client, upstream, &pair.dead)
		}
	}()

	t.Cleanup(r.stop)
	return r
}

// Break silences every connection that exists right now. Later connections are
// forwarded as usual, which is what lets a test observe recovery too.
func (r *Relay) Break() {
	r.mu.Lock()
	for _, pair := range r.pairs {
		pair.dead.Store(true)
	}
	r.mu.Unlock()
}

// BreakAll silences every connection, present and future: nothing the relay
// accepts from here on carries a byte either way, and no socket is ever closed.
//
// The difference from Break is NOT the state of the socket. Both leave the
// sockets open and both keep reading and discarding, so the client's writes
// never block under either — filling a TCP send buffer takes on the order of
// 10^5-10^6 unread bytes (tcp_wmem autotuning), and a HubFuse agent sends a
// few-hundred-byte heartbeat every 10s plus 9-byte keepalive PINGs. From the
// client's side the two fixtures are indistinguishable.
//
// The difference is the DURATION of the failure. Break deliberately serves
// connections opened after it, because its own tests (#72) need to observe the
// daemon RECOVERING: keepalive tears the silent transport down after ~15s and
// the next dial has to succeed. A test that instead needs the daemon held in the
// failure — the idle-under-outage regression of #73, which measures CPU over a
// 30s window — would see that recovery end its measurement halfway through.
// BreakAll is that test's fixture, and is otherwise identical.
func (r *Relay) BreakAll() {
	r.mu.Lock()
	r.all = true
	for _, pair := range r.pairs {
		pair.dead.Store(true)
	}
	r.mu.Unlock()
}

// pipe forwards src → dst until either side ends or the pair is broken, after
// which everything read is dropped on the floor. Both sockets stay open: a
// closed socket would surface as a clean error, which is exactly what the
// failure being reproduced never provides.
func pipe(dst, src net.Conn, dead *atomic.Bool) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 && !dead.Load() {
			if _, writeErr := dst.Write(buf[:n]); writeErr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

// stop closes the listener and every connection it is still forwarding, so no
// pipe goroutine outlives the test that created the relay.
func (r *Relay) stop() {
	_ = r.listener.Close()

	r.mu.Lock()
	pairs := r.pairs
	r.pairs = nil
	r.mu.Unlock()

	for _, pair := range pairs {
		_ = pair.client.Close()
		_ = pair.upstream.Close()
	}
}
