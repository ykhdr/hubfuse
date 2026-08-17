package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/ykhdr/hubfuse/proto"
)

// ─── The classifier ───────────────────────────────────────────────────────────

// The predicate is the whole design decision in three conjuncts, so it gets a
// test per failure class rather than one happy path. Note especially the cases
// that must NOT qualify: each is a hub that demonstrably answered, or a daemon
// that is shutting down, and condemning a connection on either would replace a
// healthy one for no reason.
func TestOurBudgetExpiredIdentifiesOnlyOurOwnDeadline(t *testing.T) {
	t.Parallel()

	live := context.Background()
	dead, cancelDead := context.WithCancel(context.Background())
	cancelDead()

	unavailable := status.Error(codes.Unavailable, "connection refused")
	hubDeadline := status.Error(codes.DeadlineExceeded, "hub says: too slow")
	refused := fmt.Errorf("%w: registration refused", ErrHubRejected)

	tests := []struct {
		name    string
		parent  context.Context
		budget  error
		callErr error
		want    bool
	}{
		{
			name:    "our budget expired on a call that went nowhere",
			parent:  live,
			budget:  context.DeadlineExceeded,
			callErr: status.Error(codes.DeadlineExceeded, "context deadline exceeded"),
			want:    true,
		},
		{
			name: "our budget expired while the code said Unavailable",
			// This is the draining-after-GOAWAY shape read out of grpc-go's
			// source: every attempt reports Unavailable while the call dies on
			// the caller's deadline. A code-based classifier would miss exactly
			// the case the replacement exists for.
			parent:  live,
			budget:  context.DeadlineExceeded,
			callErr: unavailable,
			want:    true,
		},
		{
			name:    "hub is down: instant Unavailable, our budget never fired",
			parent:  live,
			budget:  context.Canceled,
			callErr: unavailable,
			want:    false,
		},
		{
			name:    "hub answered DeadlineExceeded itself: a completed round-trip",
			parent:  live,
			budget:  context.Canceled,
			callErr: hubDeadline,
			want:    false,
		},
		{
			name:    "hub refused: a completed round-trip, even as our timer fires",
			parent:  live,
			budget:  context.DeadlineExceeded,
			callErr: refused,
			want:    false,
		},
		{
			name:    "daemon is shutting down: everything fails and none of it is evidence",
			parent:  dead,
			budget:  context.DeadlineExceeded,
			callErr: context.Canceled,
			want:    false,
		},
		{
			name:    "budget merely cancelled, not expired",
			parent:  live,
			budget:  context.Canceled,
			callErr: errors.New("some transport hiccup"),
			want:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ourBudgetExpired(tc.parent, tc.budget, tc.callErr))
		})
	}
}

// The predicate must be read from the budget context BEFORE it is cancelled.
// cancelReg runs before the error is inspected, so a check made afterwards sees
// a non-nil error every single time and would mark every Register failure as
// ours — including an instant Unavailable from a hub that is simply down.
func TestSessionOnceMarksOnlyItsOwnDeadline(t *testing.T) {
	// No t.Parallel(): this test rewrites a package-level budget var.
	t.Run("expired budget carries the sentinel", func(t *testing.T) {
		d, _ := buildTestDaemon(t)
		shortenBudget(t, &registerTimeout, 50*time.Millisecond)
		d.registerFn = func(ctx context.Context, _ []*pb.Share, _ int) (*pb.RegisterResponse, error) {
			<-ctx.Done()
			return nil, status.Error(codes.DeadlineExceeded, "context deadline exceeded")
		}

		_, err := d.sessionOnce(context.Background())
		require.Error(t, err)
		assert.ErrorIs(t, err, errSessionTimedOut)
	})

	t.Run("an instant refusal does not", func(t *testing.T) {
		d, _ := buildTestDaemon(t)
		d.registerFn = func(context.Context, []*pb.Share, int) (*pb.RegisterResponse, error) {
			return nil, status.Error(codes.Unavailable, "connection refused")
		}

		_, err := d.sessionOnce(context.Background())
		require.Error(t, err)
		assert.NotErrorIs(t, err, errSessionTimedOut,
			"a hub that is down must not look like our own deadline expiring")
	})
}

// ─── replaceHubClient ─────────────────────────────────────────────────────────

// A Daemon built as a literal has neither a connection nor a connector, which is
// how every unit fixture in this package looks. Replacing must decline there,
// not panic: swapHubClient would otherwise CAS nil against nil, report success,
// and the caller would dereference a nil *HubClient in Close.
func TestReplaceHubClientDeclinesWithNothingToWorkWith(t *testing.T) {
	t.Parallel()

	d := &Daemon{logger: discardLogger()}
	assert.NotPanics(t, func() {
		assert.False(t, d.replaceHubClient(nil), "no connection to condemn")
	})

	_, addr := startStubHub(t)
	installed := dialStubHub(t, addr)
	t.Cleanup(func() { _ = installed.Close() })
	d.setHubClient(installed)

	assert.NotPanics(t, func() {
		assert.False(t, d.replaceHubClient(installed), "no way to dial a replacement")
	})
	assert.Same(t, installed, d.hub(), "a declined replacement must leave the connection alone")
}

// A dial that fails must leave the daemon with the connection it had. However
// sick it is, it is strictly better than none.
func TestReplaceHubClientKeepsTheOldConnectionWhenDialFails(t *testing.T) {
	t.Parallel()

	_, addr := startStubHub(t)
	installed := dialStubHub(t, addr)
	t.Cleanup(func() { _ = installed.Close() })

	d := &Daemon{logger: discardLogger()}
	d.setHubClient(installed)
	d.dialFn = func() (*HubClient, error) { return nil, errors.New("certificates are gone") }

	assert.False(t, d.replaceHubClient(installed))
	assert.Same(t, installed, d.hub())
}

// ─── The swap, driven through the real reconnect loop ─────────────────────────

// swapFixture drives reconnectSession with controllable seams and counts what
// the loop did.
type swapFixture struct {
	daemon *Daemon

	dials      atomic.Int32
	registers  atomic.Int32
	registerOn chan *HubClient // the client each Register attempt ran on

	mu     sync.Mutex
	behave func(ctx context.Context) error // nil means the Register succeeds
}

func newSwapFixture(t *testing.T) *swapFixture {
	t.Helper()

	_, addr := startStubHub(t)
	initial := dialStubHub(t, addr)

	d, _ := buildTestDaemon(t)
	d.setHubClient(initial)

	f := &swapFixture{daemon: d, registerOn: make(chan *HubClient, 64)}

	d.dialFn = func() (*HubClient, error) {
		f.dials.Add(1)
		client := dialStubHub(t, addr)
		return client, nil
	}
	d.registerFn = func(ctx context.Context, _ []*pb.Share, _ int) (*pb.RegisterResponse, error) {
		f.registers.Add(1)
		select {
		case f.registerOn <- d.hub():
		default:
		}

		f.mu.Lock()
		behave := f.behave
		f.mu.Unlock()
		if behave != nil {
			if err := behave(ctx); err != nil {
				return nil, err
			}
		}
		return &pb.RegisterResponse{Success: true}, nil
	}
	d.subscribeFn = func(context.Context) (pb.HubFuse_SubscribeClient, error) {
		return errStream(), nil
	}

	return f
}

func (f *swapFixture) setBehaviour(behave func(ctx context.Context) error) {
	f.mu.Lock()
	f.behave = behave
	f.mu.Unlock()
}

// wedged models the connection-scoped wedge: the call never comes back, so it
// dies on the daemon's own deadline.
func wedged(ctx context.Context) error {
	<-ctx.Done()
	return status.Error(codes.DeadlineExceeded, "context deadline exceeded")
}

// This is the behaviour issue #77 asks for: the daemon decided the session was
// dead, and recovery must not be attempted forever on the very connection it
// condemned.
func TestReconnectReplacesTheConnectionItsAttemptExpiredOn(t *testing.T) {
	// No t.Parallel(): this test rewrites a package-level budget var.
	shortenBudget(t, &registerTimeout, 50*time.Millisecond)
	f := newSwapFixture(t)
	before := f.daemon.hub()

	// Wedged at first; healthy once the daemon has dialled again.
	f.setBehaviour(func(ctx context.Context) error {
		if f.dials.Load() > 0 {
			return nil
		}
		return wedged(ctx)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream := f.daemon.reconnectSession(ctx)
	require.NotNil(t, stream, "the session must be re-established")

	assert.Equal(t, int32(1), f.dials.Load(), "exactly one replacement connection")
	assert.NotSame(t, before, f.daemon.hub(), "the session must come back on a DIFFERENT connection")
}

// A hub that is simply unreachable answers at once; our budget never expires, so
// the connection must be kept. Replacing here is the measured regression: a kept
// channel self-corrects at 0ms per attempt while a replaced one returns to IDLE
// and pays the full connect budget again on every retry.
func TestReconnectKeepsTheConnectionWhenTheHubMerelyAnswersBadly(t *testing.T) {
	// No t.Parallel(): this test rewrites a package-level budget var.
	shortenBudget(t, &registerTimeout, 50*time.Millisecond)

	tests := []struct {
		name string
		err  error
	}{
		{"hub is down", status.Error(codes.Unavailable, "connection refused")},
		{"hub refused the registration", fmt.Errorf("%w: registration refused", ErrHubRejected)},
		{"hub answered DeadlineExceeded itself", status.Error(codes.DeadlineExceeded, "hub says: too slow")},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newSwapFixture(t)
			before := f.daemon.hub()
			f.daemon.minReconnectInterval = time.Millisecond

			var rounds atomic.Int32
			f.setBehaviour(func(context.Context) error {
				if rounds.Add(1) < 4 {
					return tc.err
				}
				return nil
			})

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			require.NotNil(t, f.daemon.reconnectSession(ctx))
			assert.Zero(t, f.dials.Load(), "a hub that answered proves the connection works")
			assert.Same(t, before, f.daemon.hub(), "the connection must be kept")
		})
	}
}

// A daemon being shut down fails every call, and none of those failures say
// anything about the connection.
func TestReconnectDoesNotReplaceWhileShuttingDown(t *testing.T) {
	// No t.Parallel(): this test rewrites a package-level budget var.
	shortenBudget(t, &registerTimeout, time.Second)
	f := newSwapFixture(t)
	f.setBehaviour(wedged)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	assert.Nil(t, f.daemon.reconnectSession(ctx), "a cancelled daemon must stop reconnecting")
	assert.Zero(t, f.dials.Load(), "shutdown must never be read as evidence against the connection")
}

// Damping. A network black hole produces the same sentinel as a wedge, so an
// undamped daemon would dial a new connection every round — each one starting
// from IDLE and paying the full connect budget — for as long as the outage
// lasts. One replacement per episode takes the useful case and stops there.
func TestReconnectReplacesTheConnectionOnlyOncePerEpisode(t *testing.T) {
	// No t.Parallel(): this test rewrites a package-level budget var.
	shortenBudget(t, &registerTimeout, 20*time.Millisecond)
	f := newSwapFixture(t)
	f.daemon.minReconnectInterval = time.Millisecond
	f.setBehaviour(wedged) // never recovers

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	assert.Nil(t, f.daemon.reconnectSession(ctx))
	assert.Greater(t, f.registers.Load(), int32(3), "the loop must have retried several times")
	assert.Equal(t, int32(1), f.dials.Load(),
		"a wedge no new connection escapes must cost exactly one replacement, not one per round")
}

// The right to replace is spent per outage, not per daemon lifetime: a session
// that comes up ends the episode, so the next outage gets its own attempt.
func TestSwapRightReturnsAfterASuccessfulSession(t *testing.T) {
	// No t.Parallel(): this test rewrites a package-level budget var.
	shortenBudget(t, &registerTimeout, 20*time.Millisecond)
	f := newSwapFixture(t)
	f.daemon.minReconnectInterval = time.Millisecond

	// Each episode: wedged until a replacement is dialled, then healthy.
	var episodeDials int32
	f.setBehaviour(func(ctx context.Context) error {
		if f.dials.Load() > episodeDials {
			return nil
		}
		return wedged(ctx)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	require.NotNil(t, f.daemon.reconnectSession(ctx), "first episode recovers")
	require.Equal(t, int32(1), f.dials.Load())

	episodeDials = 1
	require.NotNil(t, f.daemon.reconnectSession(ctx), "second episode recovers")
	assert.Equal(t, int32(2), f.dials.Load(),
		"a successful session in between must restore the right to replace")
}

// After a replacement the session must actually run on the new connection —
// otherwise the swap is bookkeeping with no effect.
func TestRegisterAfterAReplacementRunsOnTheNewConnection(t *testing.T) {
	// No t.Parallel(): this test rewrites a package-level budget var.
	shortenBudget(t, &registerTimeout, 50*time.Millisecond)
	f := newSwapFixture(t)
	original := f.daemon.hub()

	f.setBehaviour(func(ctx context.Context) error {
		if f.dials.Load() > 0 {
			return nil
		}
		return wedged(ctx)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	require.NotNil(t, f.daemon.reconnectSession(ctx))

	close(f.registerOn)
	var seen []*HubClient
	for c := range f.registerOn {
		seen = append(seen, c)
	}
	require.GreaterOrEqual(t, len(seen), 2, "at least a failed and a successful attempt")
	assert.Same(t, original, seen[0], "the first attempt ran on the original connection")
	assert.NotSame(t, original, seen[len(seen)-1], "the last one ran on the replacement")
}
