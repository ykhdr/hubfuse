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
type Relay struct {
	Addr string

	listener net.Listener
	mu       sync.Mutex
	conns    []*atomic.Bool // one "dead" flag per accepted connection pair
	closed   atomic.Bool
	wg       sync.WaitGroup
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

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
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

			dead := &atomic.Bool{}
			r.mu.Lock()
			r.conns = append(r.conns, dead)
			r.mu.Unlock()

			r.wg.Add(2)
			go func() { defer r.wg.Done(); r.pipe(upstream, client, dead) }()
			go func() { defer r.wg.Done(); r.pipe(client, upstream, dead) }()
		}
	}()

	t.Cleanup(r.stop)
	return r
}

// Break silences every connection that exists right now. Later connections are
// forwarded as usual.
func (r *Relay) Break() {
	r.mu.Lock()
	for _, dead := range r.conns {
		dead.Store(true)
	}
	r.conns = nil
	r.mu.Unlock()
}

// pipe forwards src → dst until either side ends or the pair is broken, after
// which everything read is dropped on the floor. Both sockets stay open: a
// closed socket would surface as a clean error, which is exactly what the
// failure being reproduced never provides.
func (r *Relay) pipe(dst, src net.Conn, dead *atomic.Bool) {
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

func (r *Relay) stop() {
	r.closed.Store(true)
	_ = r.listener.Close()
}
