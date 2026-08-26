package agent

// Listen/Serve split and accept-error classification — issue #90.
//
// These live apart from sshserver_test.go, which is about what the server does
// with a CONNECTION (auth, ACLs, SFTP). This file is about whether the server
// is running at all, which is the question issue #90 turned out to hinge on.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeAcceptListener is a net.Listener whose Accept returns a scripted sequence
// of errors and then blocks until Close. A real EMFILE is not reproducible
// portably inside a test — it needs the process to actually run out of
// descriptors — and the branch that rides one out has to be pinned anyway; this
// is the seam that pins it. The errors are wrapped in *net.OpError the way a
// real listener wraps them, so the test also proves the classifier traverses
// the wrapping rather than only matching a bare errno. (#90)
type fakeAcceptListener struct {
	mu      sync.Mutex
	errs    []error
	calls   int
	release chan struct{}
	closed  bool
}

func newFakeAcceptListener(errs ...error) *fakeAcceptListener {
	return &fakeAcceptListener{errs: errs, release: make(chan struct{})}
}

func (f *fakeAcceptListener) Accept() (net.Conn, error) {
	f.mu.Lock()
	f.calls++
	var err error
	if len(f.errs) > 0 {
		err = f.errs[0]
		f.errs = f.errs[1:]
	}
	f.mu.Unlock()

	if err != nil {
		return nil, &net.OpError{Op: "accept", Net: "tcp", Err: err}
	}
	<-f.release
	return nil, net.ErrClosed
}

func (f *fakeAcceptListener) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.closed {
		f.closed = true
		close(f.release)
	}
	return nil
}

func (f *fakeAcceptListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4zero} }

func (f *fakeAcceptListener) acceptCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// newTestSSHServer builds an SSHServer on port with a fresh host key.
func newTestSSHServer(t *testing.T, port int) *SSHServer {
	t.Helper()
	dir := t.TempDir()
	_, err := GenerateSSHKeyPair(dir)
	require.NoError(t, err, "GenerateSSHKeyPair()")
	srv, err := NewSSHServer(port, filepath.Join(dir, "id_ed25519"), discardLogger())
	require.NoError(t, err, "NewSSHServer()")
	return srv
}

// injectListener replaces the bound listener. Only a test may do this; it is
// how the fake reaches the accept loop without a real socket.
func injectListener(srv *SSHServer, ln net.Listener) {
	srv.mu.Lock()
	srv.listener = ln
	srv.stopping = false
	srv.mu.Unlock()
}

// TestSSHServer_ListenReportsBindFailure is the startup half of issue #90: the
// bind error has to be a value the caller can act on, not a log line. Before
// the Listen/Serve split it was produced inside a goroutine Daemon.startSSH
// discarded, so the daemon registered anyway and the hub advertised a port this
// process does not own.
func TestSSHServer_ListenReportsBindFailure(t *testing.T) {
	squatter, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "squat a port")
	defer squatter.Close()
	port := squatter.Addr().(*net.TCPAddr).Port

	srv := newTestSSHServer(t, port)

	err = srv.Listen()
	require.Error(t, err, "Listen() on an occupied port must report the failure")
	assert.Contains(t, err.Error(), fmt.Sprintf("%d", port),
		"the bind error must name the port an operator has to free")
	assert.ErrorIs(t, err, syscall.EADDRINUSE,
		"the underlying errno must survive so callers can classify it")
}

// TestSSHServer_ServeWithoutListen pins that Serve refuses rather than panics
// on a nil listener. Splitting bind from serve makes "serve before bind" newly
// expressible, and a nil-deref there would be a worse failure than the one this
// issue fixes. (#90)
func TestSSHServer_ServeWithoutListen(t *testing.T) {
	srv := newTestSSHServer(t, 0)

	err := srv.Serve(context.Background())
	require.Error(t, err, "Serve() without Listen() must return an error")
	assert.Contains(t, err.Error(), "listener not bound")
}

// TestSSHServer_ServeRidesOutTransientAcceptErrors covers the second instance
// of issue #90's defect: a momentary EMFILE used to end the accept loop for
// good, leaving a registered, online daemon with nothing behind its advertised
// port. The listening socket is still ours in that state and its backlog still
// holds the pending connections, so the loop must retry rather than die.
func TestSSHServer_ServeRidesOutTransientAcceptErrors(t *testing.T) {
	srv := newTestSSHServer(t, 0)
	ln := newFakeAcceptListener(syscall.EMFILE, syscall.EMFILE, syscall.ENFILE)
	injectListener(srv, ln)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()

	// All three scripted errors consumed plus the blocking fourth call: the
	// loop kept going instead of returning on the first one.
	require.Eventually(t, func() bool { return ln.acceptCalls() >= 4 }, 3*time.Second, 10*time.Millisecond,
		"Serve must keep accepting after transient errors")

	select {
	case err := <-done:
		t.Fatalf("Serve() returned on a transient accept error: %v", err)
	default:
	}

	cancel()
	select {
	case err := <-done:
		assert.NoError(t, err, "a cancelled Serve is a clean stop")
	case <-time.After(3 * time.Second):
		t.Fatal("Serve() did not return after ctx cancel")
	}
}

// TestSSHServer_ServeReturnsFatalAcceptError is the negative control for the
// retry branch: if every accept error were ridden out, the daemon would never
// learn its SSH server had died and issue #90 would survive its own fix.
func TestSSHServer_ServeReturnsFatalAcceptError(t *testing.T) {
	boom := errors.New("listener died")
	srv := newTestSSHServer(t, 0)
	injectListener(srv, newFakeAcceptListener(boom))

	done := make(chan error, 1)
	go func() { done <- srv.Serve(context.Background()) }()

	select {
	case err := <-done:
		require.Error(t, err, "a dead listener must be reported")
		assert.ErrorIs(t, err, boom, "the accept error must reach the caller unchanged")
	case <-time.After(3 * time.Second):
		t.Fatal("Serve() did not return on a fatal accept error")
	}
}

// TestSSHServer_ServeReturnsNilOnStop pins the other half of that control: a
// deliberate Stop must NOT look like a dead listener, or every clean shutdown
// would take the daemon down through the fatal path. The verdict comes from the
// stopping flag rather than from the error, because a listener someone closed
// and a listener that died both surface as net.ErrClosed. (#90)
func TestSSHServer_ServeReturnsNilOnStop(t *testing.T) {
	srv := newTestSSHServer(t, 0)
	require.NoError(t, srv.Listen(), "Listen() on an ephemeral port")

	srv.mu.RLock()
	addr := srv.listener.Addr().String()
	srv.mu.RUnlock()

	done := make(chan error, 1)
	// Background context on purpose: Stop, not cancellation, is what ends this.
	go func() { done <- srv.Serve(context.Background()) }()

	// Let the loop reach Accept before closing the listener under it.
	require.Eventually(t, func() bool {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}, 3*time.Second, 20*time.Millisecond, "server should be accepting")

	require.NoError(t, srv.Stop(), "Stop()")

	select {
	case err := <-done:
		assert.NoError(t, err, "a deliberate Stop is a clean stop, not a dead listener")
	case <-time.After(3 * time.Second):
		t.Fatal("Serve() did not return after Stop()")
	}

	// A re-Listen clears the verdict, so a restarted server is not born stopped.
	require.NoError(t, srv.Listen(), "re-Listen()")
	assert.False(t, srv.isStopping(), "Listen() must clear the stopping flag")
	require.NoError(t, srv.Stop(), "final Stop()")
}
