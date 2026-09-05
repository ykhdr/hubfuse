package agent

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/ykhdr/hubfuse/proto"
)

// The daemon's FIRST hub session used to be the only one it could not survive.
// Every later session got reconnectSession's infinite backoff; the first got one
// attempt, and its error killed the process.
//
// That is what made the macOS local-network denial fatal. Measured on the test
// bed (macOS 26.4, under a LaunchAgent): the first connect() from an identity
// macOS has not yet registered is refused with EHOSTUNREACH — the kernel names
// `reason: NECP` — and attempts seconds later succeed. Two independent ad-hoc
// probes gave `fail, ok, ok` and `fail, fail, ok`. The same retry also covers a
// Mac waking up, a network that is not up yet, and a hub that is still booting.
//
// These tests pin the four outcomes that follow from that, and each fails if the
// change is reverted.

// startupDaemon builds a daemon wired for the first-session path: no real hub,
// a heartbeat that cannot log, and a backoff short enough that a few retries
// cost milliseconds rather than seconds.
func startupDaemon(t *testing.T) (*Daemon, *atomic.Int32) {
	t.Helper()

	d, _ := buildTestDaemon(t)
	d.minReconnectInterval = time.Millisecond
	d.heartbeatInterval = time.Hour
	d.heartbeatFn = func(context.Context) error { return nil }
	d.subscribeFn = func(context.Context) (pb.HubFuse_SubscribeClient, error) {
		return errStream(), nil
	}

	var ready atomic.Int32
	d.onReady = func() { ready.Add(1) }

	return d, &ready
}

// TestRegisterAndSubscribe_RetriesUntilTheHubAnswers is the fix itself: a
// startup failure that would have killed the daemon is now survived, and the
// session it eventually gets is a real one.
//
// The call counts are the objective part. Before the change registerFn is called
// exactly once and an error comes back; after it, the daemon keeps asking until
// the hub answers. onReady fires exactly once across the whole sequence —
// it is the PID-file hook and a process has one PID file — but it no longer
// waits for the accepted Register: since #102 it fires when the daemon COMMITS
// to running, which here is the moment it decides to retry.
func TestRegisterAndSubscribe_RetriesUntilTheHubAnswers(t *testing.T) {
	d, ready := startupDaemon(t)

	var calls atomic.Int32
	d.registerFn = func(context.Context, []*pb.Share, int) (*pb.RegisterResponse, error) {
		if calls.Add(1) <= 2 {
			return nil, errors.New("dial tcp 192.168.31.158:9090: connect: no route to host")
		}
		return &pb.RegisterResponse{}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := d.registerAndSubscribe(ctx)

	require.NoError(t, err, "a transient startup failure must not end the daemon")
	require.NotNil(t, stream, "the daemon must come up on the attempt the hub answers")
	assert.Equal(t, int32(3), calls.Load(),
		"two failures then a success: the daemon must have kept asking")
	assert.Equal(t, int32(1), ready.Load(),
		"one process, one PID file — however many attempts it took")
	assert.True(t, d.everRegistered.Load(),
		"an accepted Register must be recorded — the stop path branches on it")
}

// TestRegisterAndSubscribe_ReadyBeforeRegistered is the event #102 introduced,
// asserted at the only moment it can be asserted honestly.
//
// Readiness and registration used to be one event; they are two now. A daemon
// that fails its first attempt transiently and settles into the retry loop is a
// running daemon — it owns its SSH port, its mount targets are guarded, and it
// will keep asking — so it must have a PID file, or `hubfuse stop` and
// `hubfuse status` are blind to it and `hubfuse start -d` kills it outright.
//
// everRegistered is read INSIDE the onReady callback, not afterwards. Read after
// the fact it would be a race against whichever attempt eventually succeeds, and
// on this daemon it would simply be true by then — a vacuous pass. Read from
// inside, it inverts on revert: with readiness tied back to an accepted
// Register, onReady can only ever run with everRegistered already true.
func TestRegisterAndSubscribe_ReadyBeforeRegistered(t *testing.T) {
	d, ready := startupDaemon(t)

	var registeredWhenReady atomic.Bool
	var attemptsWhenReady atomic.Int32
	var calls atomic.Int32

	d.onReady = func() {
		ready.Add(1)
		registeredWhenReady.Store(d.everRegistered.Load())
		attemptsWhenReady.Store(calls.Load())
	}
	d.registerFn = func(context.Context, []*pb.Share, int) (*pb.RegisterResponse, error) {
		if calls.Add(1) <= 3 {
			return nil, errors.New("dial tcp 192.168.31.158:9090: connect: no route to host")
		}
		return &pb.RegisterResponse{}, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	stream, err := d.registerAndSubscribe(ctx)
	require.NoError(t, err)
	require.NotNil(t, stream)

	assert.Equal(t, int32(1), ready.Load(), "one process, one PID file")
	assert.False(t, registeredWhenReady.Load(),
		"readiness must be signalled BEFORE the hub has accepted anything — that is the whole "+
			"of #102, and tying it back to registration makes this true")
	assert.Equal(t, int32(1), attemptsWhenReady.Load(),
		"and specifically at the decision to retry, after the first attempt returned")
	assert.True(t, d.everRegistered.Load(),
		"while everRegistered still means what its two readers need: the hub accepted a Register")
}

// TestRegisterAndSubscribe_ExitsWhenTheHubRefusesTheFirstRegistration is the
// limit of the retry, and the reason it is a limit.
//
// "The hub cannot be reached" is what retrying is for. "The hub answered and
// said no" is not: a pruned device holds valid TLS material, connects, registers
// at the transport level and is refused, and no amount of retrying changes that
// — it needs `hubfuse join`. This is the first half of #69, and
// tests/scenarios/prune_test.go pins the same contract end to end.
//
// Exiting costs nothing here: there is no session, no PID file, no mount and no
// peer that has seen this device. Mid-life all four exist, which is why
// reconnectSession retries the identical error instead.
func TestRegisterAndSubscribe_ExitsWhenTheHubRefusesTheFirstRegistration(t *testing.T) {
	d, ready := startupDaemon(t)

	var calls atomic.Int32
	d.registerFn = func(context.Context, []*pb.Share, int) (*pb.RegisterResponse, error) {
		calls.Add(1)
		return nil, fmt.Errorf("%w: registration refused: this device is not registered on the hub", ErrHubRejected)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := d.registerAndSubscribe(ctx)

	require.Error(t, err, "a refusal must still end the daemon")
	assert.ErrorIs(t, err, ErrHubRejected, "and must stay recognisable as a refusal")
	assert.Nil(t, stream)
	assert.Equal(t, int32(1), calls.Load(),
		"a refusal is answered once, not retried — retrying cannot fix it")
	assert.Zero(t, ready.Load(),
		"no PID file for a daemon the hub does not know (#69) — markReady sits BELOW the "+
			"refusal branch in both of its call sites, which is the whole reason readiness is "+
			"signalled at the decision to retry rather than at the SSH bind")
	assert.False(t, d.everRegistered.Load())
}

// TestRegisterAndSubscribe_StopIsCleanWhileStillRetrying pins the exit code of
// the ordinary stop, which the retry made reachable in a new place, and — since
// #102 — that a daemon stopped mid-retry legitimately HAD a PID file.
//
// reconnectSession returns nil only on ctx.Done — SIGTERM or SIGINT, the normal
// way a daemon is stopped. If that came back as an error, Run would return it,
// main would exit 1, and the LaunchAgent's KeepAlive (SuccessfulExit false)
// would relaunch the daemon it was just asked to stop: a daemon that cannot be
// stopped during a hub outage, which is the #98 pathology reintroduced by the
// fix for #98.
func TestRegisterAndSubscribe_StopIsCleanWhileStillRetrying(t *testing.T) {
	d, ready := startupDaemon(t)

	d.registerFn = func(context.Context, []*pb.Share, int) (*pb.RegisterResponse, error) {
		return nil, errors.New("connection refused")
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	stream, err := d.registerAndSubscribe(ctx)

	require.NoError(t, err,
		"a stop during the retry is a clean stop, not a failure — a non-zero exit here is relaunched")
	assert.Nil(t, stream, "and there is no session to hand to the supervisor")
	assert.Equal(t, int32(1), ready.Load(),
		"a daemon that was retrying was a running daemon, and `hubfuse stop` has to be able "+
			"to see it — the PID file is removed by runAgent's defer on the way out")
	assert.False(t, d.everRegistered.Load(),
		"Run uses this to decide that no Deregister is owed — calling Shutdown against "+
			"an unreachable hub would fail and produce the very non-zero exit being avoided")
}

// TestRegisterAndSubscribe_SSHDeathEndsTheWait keeps #90 true across the window
// this change opened.
//
// d.sshDied is read in runServices, which is unreachable until the first session
// exists. Without this select, an accept loop that died during a long retry
// would sit unread in the buffer: registration would eventually succeed, the hub
// would be handed d.sshPort, peers would mount from a port this process does not
// hold, and only then would runServices drain the channel. That is #90's exact
// harm with a smaller window, so the SSH server's liveness stays a precondition
// of the daemon existing at all — in both directions.
func TestRegisterAndSubscribe_SSHDeathEndsTheWait(t *testing.T) {
	d, ready := startupDaemon(t)
	d.sshDied = make(chan error, 1)

	d.registerFn = func(context.Context, []*pb.Share, int) (*pb.RegisterResponse, error) {
		return nil, errors.New("connection refused")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() {
		time.Sleep(30 * time.Millisecond)
		d.sshDied <- errors.New("accept: too many open files")
	}()

	stream, err := d.registerAndSubscribe(ctx)

	require.Error(t, err, "a dead SSH server must end the daemon even before it has reached the hub")
	assert.Contains(t, err.Error(), "too many open files",
		"and the SSH failure must be the reported cause, not the hub failure it was waiting on")
	assert.Nil(t, stream)
	// This is the residual window #102 accepts rather than hides: readiness was
	// already signalled, so `hubfuse start -d` may have reported success for a
	// process that then exits here. It is accepted because the COMMON SSH
	// failure — a bind that cannot take the port, the whole of #90 — still
	// aborts Run before readiness is ever signalled. What is left is an accept
	// loop dying inside the retry window, which is rare and announces itself.
	assert.Equal(t, int32(1), ready.Load())
}
