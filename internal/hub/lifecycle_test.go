package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// TestStopServer_ExhaustedBudgetStillGivesTheForcedStopAChance covers the case
// where everything before StopServer has already spent the shutdown budget, so
// the caller passes a zero hard limit (splitRemaining caps grace at whatever
// remains and hands the rest — nothing — to the forced window).
//
// Without a floor this is a guaranteed FALSE StopHung: time.After(0) is ready
// immediately, while the just-spawned goroutine still has to be scheduled and
// call Stop() before the waiter can wake. The caller turns StopHung into
// os.Exit(1), so an ordinary SIGTERM would exit non-zero — systemd records a
// failed unit — on a hub whose forced stop would have returned in
// microseconds. Reachable in production whenever the sweep waits out the
// store's busy_timeout behind a concurrent `hubfuse-hub issue-join`.
func TestStopServer_ExhaustedBudgetStillGivesTheForcedStopAChance(t *testing.T) {
	srv := newFakeStoppableServer(true)
	t.Cleanup(srv.unblock)

	outcome := StopServer(srv, 0, 0, testLifecycleLogger())

	assert.Equal(t, StopForced, outcome,
		"a spent budget must not be reported as hung when Stop() works fine")
	assert.True(t, srv.stopCalled.Load(), "Stop must still be attempted")
}

// TestStopServer_ExhaustedBudgetNeverReportsHung is the assertion that
// actually matters when the budget is gone, and it is deliberately weaker than
// "reports graceful".
//
// With a zero grace, whether GracefulStop is observed at all is a question of
// goroutine SCHEDULING, not of how fast the server is: the goroutine that calls
// it may not have run yet when time.After(0) fires, so a clean stop can still
// be classified as forced. That misclassification is cheap — it only means the
// store is left open, which costs nothing on a process about to exit with its
// WAL committed. StopHung is the expensive one: it costs an os.Exit(1). So the
// invariant worth pinning is that an exhausted budget never manufactures a hang.
func TestStopServer_ExhaustedBudgetNeverReportsHung(t *testing.T) {
	srv := newFakeStoppableServer(false)
	srv.unblock() // GracefulStop returns as soon as it is scheduled.
	t.Cleanup(srv.unblock)

	outcome := StopServer(srv, 0, 0, testLifecycleLogger())

	assert.NotEqual(t, StopHung, outcome,
		"a spent budget must not turn a server that stops fine into an exit-1 hang")
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

// jsonLogEntry captures the fields logCancelable's callers care about from a
// single slog.NewJSONHandler line.
func jsonLogEntry(t *testing.T, line []byte) map[string]any {
	t.Helper()
	var entry map[string]any
	require.NoError(t, json.Unmarshal(line, &entry), "unmarshal log line: %s", line)
	return entry
}

// TestLogCancelable_ContextCanceled_LogsDebugNotConfiguredLevel covers the
// #75 quiet-shutdown fix: Stop cancels the background goroutines' context, so
// every store call still in flight at that instant returns context.Canceled.
// If this regressed to logging at the caller's level again, every normal
// shutdown would print an Error/Warn line indistinguishable from a real fault.
func TestLogCancelable_ContextCanceled_LogsDebugNotConfiguredLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	logCancelable(logger, slog.LevelError, "get stale devices", context.Canceled)

	entry := jsonLogEntry(t, buf.Bytes())
	assert.Equal(t, "DEBUG", entry["level"], "a cancelled context must log at Debug, not the caller-requested level")
}

// TestLogCancelable_DeadlineExceeded_LogsDebugNotConfiguredLevel covers the
// other half of the pair the spec calls out: a context that expired on its
// own budget is just as ordinary during shutdown as one that was cancelled
// outright, and must be quieted the same way.
func TestLogCancelable_DeadlineExceeded_LogsDebugNotConfiguredLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	logCancelable(logger, slog.LevelWarn, "prune expired invites", context.DeadlineExceeded)

	entry := jsonLogEntry(t, buf.Bytes())
	assert.Equal(t, "DEBUG", entry["level"], "an expired deadline must log at Debug, not the caller-requested level")
}

// TestLogCancelable_WrappedCancellation_StillDetected covers the store layer
// wrapping context.Canceled inside a %w chain (as every store method that
// takes ctx would); errors.Is, not ==, is what has to see through that.
func TestLogCancelable_WrappedCancellation_StillDetected(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	wrapped := fmt.Errorf("get stale devices: %w", context.Canceled)
	logCancelable(logger, slog.LevelError, "get stale devices", wrapped)

	entry := jsonLogEntry(t, buf.Bytes())
	assert.Equal(t, "DEBUG", entry["level"], "a wrapped context.Canceled must still be recognised via errors.Is")
}

// TestLogCancelable_OrdinaryError_KeepsConfiguredLevel is the other side of
// the fix: an error that is not a cancellation must be exactly as loud as it
// was before — logCancelable must not blanket-quiet every failure, only the
// shutdown-induced ones.
func TestLogCancelable_OrdinaryError_KeepsConfiguredLevel(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	logCancelable(logger, slog.LevelError, "get stale devices", errors.New("disk I/O error"))

	entry := jsonLogEntry(t, buf.Bytes())
	assert.Equal(t, "ERROR", entry["level"], "a non-cancellation error must keep the level its caller asked for")
}

// TestLogCancelable_PreservesExtraAttrs covers checkStale's mark-offline call
// site, the one place that logs an extra device_id attribute alongside the
// error — the helper must not silently drop it.
func TestLogCancelable_PreservesExtraAttrs(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	logCancelable(logger, slog.LevelError, "mark offline", errors.New("boom"), slog.String("device_id", "dev-1"))

	entry := jsonLogEntry(t, buf.Bytes())
	assert.Equal(t, "dev-1", entry["device_id"], "attrs passed to logCancelable must reach the log line")
}
