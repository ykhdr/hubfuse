package hub

import (
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// fakeStoppableServer is a stoppableServer whose GracefulStop/Stop timing the
// test controls directly, so StopServer's grace and hardLimit windows can be
// exercised in milliseconds instead of driving a real listener through the
// same clock as the limits meant to bound it.
//
// GracefulStop blocks on gracefulUnblock exactly like grpc-go's real one
// blocks on its connection set going empty; Stop(), when unblockOnStop is set,
// closes gracefulUnblock itself — mirroring the real Stop() forcing every
// connection closed out from under a hanging GracefulStop, which is what
// actually lets it return on the forced path. stopCalled is read from the
// test goroutine while it is written from a goroutine StopServer starts, so a
// bare bool would race; atomic.Bool keeps -race clean. (#75)
type fakeStoppableServer struct {
	gracefulUnblock chan struct{}
	unblockOnStop   bool
	stopCalled      atomic.Bool
}

func newFakeStoppableServer(unblockOnStop bool) *fakeStoppableServer {
	return &fakeStoppableServer{
		gracefulUnblock: make(chan struct{}),
		unblockOnStop:   unblockOnStop,
	}
}

func (f *fakeStoppableServer) GracefulStop() {
	<-f.gracefulUnblock
}

func (f *fakeStoppableServer) Stop() {
	f.stopCalled.Store(true)
	if f.unblockOnStop {
		f.unblock()
	}
}

// unblock releases a blocked GracefulStop. Safe to call more than once.
func (f *fakeStoppableServer) unblock() {
	select {
	case <-f.gracefulUnblock:
	default:
		close(f.gracefulUnblock)
	}
}

func testLifecycleLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// TestStopServer_GracefulReturnsWithinGrace_NeverForces is the common case: a
// server whose connections close on their own inside grace. If this
// regresses, StopServer starts forcing every ordinary shutdown down the path
// reserved for a dead peer, breaking the "Stop() never runs on the happy
// path" contract StopServer promises its caller.
func TestStopServer_GracefulReturnsWithinGrace_NeverForces(t *testing.T) {
	srv := newFakeStoppableServer(false)
	srv.unblock() // GracefulStop returns immediately.

	start := time.Now()
	outcome := StopServer(srv, 200*time.Millisecond, 200*time.Millisecond, testLifecycleLogger())
	elapsed := time.Since(start)

	assert.Equal(t, StopGraceful, outcome)
	assert.False(t, srv.stopCalled.Load(), "Stop must not be called when GracefulStop returns on its own")
	assert.Less(t, elapsed, 100*time.Millisecond, "must not wait out the grace timer when graceful already returned")
}

// TestStopServer_GraceExpires_ForcedStopUnblocksIt covers the ordinary
// unhealthy case: a live handler holds GracefulStop open past grace, and the
// forced Stop() is what actually ends it — grpc-go's Stop() empties the
// connection set a hanging GracefulStop is blocked on. If this regresses,
// either Stop() never gets called (the 29.99s hang from the issue comes back)
// or StopServer fails to notice the forced stop succeeded and reports the
// wrong outcome.
func TestStopServer_GraceExpires_ForcedStopUnblocksIt(t *testing.T) {
	srv := newFakeStoppableServer(true)
	t.Cleanup(srv.unblock) // in case an assertion fails before StopServer forces it

	grace := 30 * time.Millisecond
	start := time.Now()
	outcome := StopServer(srv, grace, 200*time.Millisecond, testLifecycleLogger())
	elapsed := time.Since(start)

	assert.Equal(t, StopForced, outcome)
	assert.True(t, srv.stopCalled.Load(), "Stop must be called once grace expires")
	assert.GreaterOrEqual(t, elapsed, grace, "must not force before grace has actually expired")
	assert.Less(t, elapsed, 500*time.Millisecond, "must return promptly once the forced Stop unblocks it")
}

// TestStopServer_ForcedStopDoesNotHelp_ReturnsHung covers a peer stuck deep
// enough (e.g. blocked in stream.Send on an exhausted flow-control window)
// that even a forced Stop() does not free the handler within hardLimit. If
// this regresses, StopServer blocks past grace+hardLimit and the whole point
// of a hard upper bound — giving the caller a chance to os.Exit regardless —
// is defeated.
func TestStopServer_ForcedStopDoesNotHelp_ReturnsHung(t *testing.T) {
	srv := newFakeStoppableServer(false)
	t.Cleanup(srv.unblock) // release the goroutine StopServer leaves running behind it

	grace := 20 * time.Millisecond
	hardLimit := 20 * time.Millisecond
	start := time.Now()
	outcome := StopServer(srv, grace, hardLimit, testLifecycleLogger())
	elapsed := time.Since(start)

	assert.Equal(t, StopHung, outcome)
	assert.True(t, srv.stopCalled.Load(), "Stop must still be attempted even though it will not help")
	assert.Less(t, elapsed, grace+hardLimit+500*time.Millisecond, "must return once hardLimit expires, not block indefinitely")
}
