package integration

import (
	"context"
	"io"
	"log/slog"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"

	"github.com/ykhdr/hubfuse/internal/agent"
	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/hub"
	"github.com/ykhdr/hubfuse/internal/hub/hubtest"
	pb "github.com/ykhdr/hubfuse/proto"
)

// Issue #78 claimed a hub built before #72 traps every agent in a permanent
// ~30s disconnect loop. Measured against a real pre-#72-policy hub, that is
// false: the loop only happens to a connection that never calls the hub at
// all. Two independent mechanisms — grpc-go's client only pings when the
// transport has had NO read for keepalive.Time, and the hub server zeroes its
// ping-strike counter on every response WRITE (setResetPingStrikes) — mean a
// heartbeat every 10s (the production cadence, agent.hubKeepaliveTime /
// internal/agent/heartbeat.go's default) keeps the punishment from ever
// accumulating. That compatibility is real but was, until this file, entirely
// unpinned: nothing in the suite exercised a pre-#72 hub at all.
const (
	// oldHubEnforcementMinTime is exactly grpc-go's own built-in default
	// (internal/transport/defaults.go:40 in grpc@v1.79.3), i.e. precisely what
	// a hub predating #72 ran, since it never called
	// grpc.KeepaliveEnforcementPolicy at all. Spelling it out here — rather
	// than just omitting the option and relying on grpc-go's internal default
	// — keeps startOldHub legible without requiring the reader to know that
	// default.
	oldHubEnforcementMinTime = 5 * time.Minute

	// productionCadence mirrors the actual production heartbeat cadence
	// (internal/agent/heartbeat.go's default, HUBFUSE_HEARTBEAT_INTERVAL
	// unset). It is the one interval this test is not free to shrink to speed
	// itself up: grpc.WithKeepaliveParams clamps the client's keepalive Time
	// to a 10s floor (internal.KeepaliveMinPingTime, grpc@v1.79.3
	// dialoptions.go:562-564), so a heartbeat any faster than that floor would
	// silently stop testing the mechanism issue #78 is about — a response
	// beating the client's OWN fastest possible ping.
	productionCadence = 10 * time.Second

	// boundaryCadence is the third control (2.5x productionCadence): it exists
	// because A (silence) and B (10s) alone only bracket the two extremes —
	// "never responds" and "responds faster than the client can even ping" —
	// and say nothing about where the actual boundary sits in between.
	//
	// The punishment needs THREE consecutive keepalive pings with no server
	// response WRITE in between (the hub's ping-strike counter resets on every
	// write, and GOAWAY fires once pingStrikes > 2 — maxPingStrikes). The
	// client's ping interval is pinned at the 10s floor above and cannot go
	// lower, so three strikes need on the order of 30s of silence. A heartbeat
	// every 25s writes a response before a third ping can accumulate against
	// it, so the connection must survive — this is what actually pins the
	// boundary above ~20s (two ping intervals) rather than merely asserting
	// two disconnected extremes are safe/unsafe.
	boundaryCadence = 25 * time.Second

	// aDeathDeadline bounds how long the test waits for connection A's stream
	// to die. Measured twice independently against a real old-policy hub, the
	// GOAWAY lands at ~30.07s after the last RPC (three ping-strike cycles at
	// the clamped 10s floor); this test observes it at 31.00s ±2ms across five
	// consecutive runs, the extra second being B's and C's setup, which happens
	// after A's last RPC but before the shared clock starts (see `start`).
	// 50s leaves roughly 19s of margin above that measurement for a slow CI
	// box, while still bounding the test — raising it further would turn "the
	// fixture stopped punishing" into a silent hang instead of a loud failure.
	// In the common case the test returns as soon as A's death is observed
	// (around 31s), not after the full 50s.
	aDeathDeadline = 50 * time.Second
)

// startOldHub brings up a second gRPC front end for h's registry that behaves
// like a hub built before issue #72: the PRODUCTION server options
// (hub.ServerOptions) — same credentials, same drain/auth interceptor chain,
// same grpc.KeepaliveParams — with ONLY the keepalive ENFORCEMENT POLICY
// replaced. grpc.KeepaliveEnforcementPolicy is a plain assignment into
// serverOptions (grpc@v1.79.3 server.go:335-339), so appending a second one
// here wins over the one hub.ServerOptions already installed. Building the
// option list any other way (credentials/interceptors/keepalive hand-rolled
// separately, as a throwaway measurement harness for this issue once did)
// would test a transport production never runs.
//
// MinTime: oldHubEnforcementMinTime (see above) plus PermitWithoutStream left
// at its zero value (false, also grpc-go's default) together reproduce
// exactly what a pre-#72 hub had, since it never touched this option at all.
func startOldHub(t *testing.T, h *hubtest.Harness) string {
	t.Helper()

	tlsCfg, err := common.LoadTLSServerConfig(
		filepath.Join(h.TLSDir, common.CACertFile),
		filepath.Join(h.TLSDir, common.ServerCertFile),
		filepath.Join(h.TLSDir, common.ServerKeyFile),
	)
	require.NoError(t, err, "startOldHub: LoadTLSServerConfig")

	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))

	opts := append(hub.ServerOptions(credentials.NewTLS(tlsCfg), h.Registry),
		grpc.KeepaliveEnforcementPolicy(keepalive.EnforcementPolicy{
			MinTime: oldHubEnforcementMinTime,
			// PermitWithoutStream is left at its zero value (false): that too
			// is grpc-go's own default, and the combination of both zero-config
			// defaults is exactly what a hub predating #72 ran.
		}),
	)

	srv := grpc.NewServer(opts...)
	pb.RegisterHubFuseServer(srv, hub.NewServer(h.Registry, logger))

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "startOldHub: listen")

	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	return lis.Addr().String()
}

// setupOldHubConnection joins deviceID to h, registers it and subscribes,
// all through the old-hub front end at oldHubAddr, returning the client and
// its ready event stream. Shared by all three connections in
// TestIntegration_OldHubToleratesHeartbeatingAgent so what differs between
// them — how each behaves AFTER this point — stays the only difference on
// the page.
func setupOldHubConnection(t *testing.T, h *hubtest.Harness, oldHubAddr, deviceID, nickname string, sshPort int) (*agent.HubClient, pb.HubFuse_SubscribeClient) {
	t.Helper()
	ctx := context.Background()

	// Join goes to the real hub (h.Addr) to mint the certificate and create
	// the device row, while the returned client talks to oldHubAddr for
	// everything after that — joinAgentClientAt exists precisely for putting
	// something other than the hub itself between the agent and its dial
	// target (#72).
	client := joinAgentClientAt(t, h, oldHubAddr, deviceID, nickname)

	_, err := client.Register(ctx, nil, sshPort)
	require.NoError(t, err, "register %s", deviceID)

	subCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	stream, err := client.Subscribe(subCtx)
	require.NoError(t, err, "subscribe %s", deviceID)
	ready, err := stream.Recv()
	require.NoError(t, err, "%s subscribe ready", deviceID)
	require.NotNil(t, ready.GetSubscribeReady(), "expected SubscribeReady for %s, got %T", deviceID, ready.GetPayload())

	return client, stream
}

// streamDeath is what watchStreamDeath reports: not just WHEN a stream died
// but WHY, since "died at 27s" alone cannot tell a GOAWAY apart from a
// canceled context or a plain EOF once this test is failing in CI and there
// is nobody around to rerun it with more logging.
type streamDeath struct {
	err error
	at  time.Duration
}

// watchStreamDeath reads stream in its own goroutine so its death is observed
// the instant it happens — the streamDeath sent on the returned channel IS
// the measurement, not an artefact of when some later probe happened to run.
func watchStreamDeath(stream pb.HubFuse_SubscribeClient, start time.Time) <-chan streamDeath {
	dead := make(chan streamDeath, 1)
	go func() {
		for {
			if _, err := stream.Recv(); err != nil {
				dead <- streamDeath{err: err, at: time.Since(start)}
				return
			}
		}
	}()
	return dead
}

// heartbeatLoop calls client.Heartbeat every interval until ctx is done. It
// is deliberately best-effort: a real failure surfaces through the caller's
// watchStreamDeath channel, not through this loop's return value, since the
// stream (not the heartbeat call itself) is the thing under test.
func heartbeatLoop(ctx context.Context, client *agent.HubClient, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			hCtx, hCancel := context.WithTimeout(ctx, interval)
			_ = client.Heartbeat(hCtx)
			hCancel()
		}
	}
}

// TestIntegration_OldHubToleratesHeartbeatingAgent pins issue #78's actual,
// previously-unpinned finding by carrying THREE controls against ONE old-hub
// fixture in a single window:
//
//   - connection A ("silent") registers, subscribes, then sends nothing —
//     this is the POSITIVE CONTROL. Its stream dying proves the fixture
//     genuinely reproduces a pre-#72 hub's punishment; without this, a green
//     test would also be green if startOldHub's enforcement-policy override
//     silently failed to apply, which would make the assertions below
//     worthless.
//   - connection B ("production cadence") calls Heartbeat every
//     productionCadence (10s) — this is the REGRESSION assertion: a
//     heartbeating agent must not be punished by a pre-#72 hub.
//   - connection C ("boundary cadence") calls Heartbeat every boundaryCadence
//     (25s) — see boundaryCadence's doc comment for why this third point
//     matters: A and B alone only bracket the two extremes (never responds /
//     responds far faster than needed), and say nothing about where the
//     boundary actually sits. C's stream surviving pins it above ~20s — within
//     this ~31s shared window C gets exactly one heartbeat write (at ~25s),
//     which is what resets the strikes the pings before it accumulated; this
//     shows the boundary sits above ~20s within the window that kills A, not
//     that a 25s cadence survives indefinitely.
//
// Runtime cost: this test normally finishes in ~30-35s (however long A
// actually takes to die) and is bounded at aDeathDeadline plus a few seconds
// of setup/teardown. That cost is why the Makefile raised the integration
// package's shared budget from 180s to 300s in this same branch — see the
// comment on `test-integration`: this window is timer-dominated and cannot be
// shortened, because grpc-go clamps the client's keepalive interval to a 10s
// floor.
//
// All three streams are read from their own goroutine, and every assertion
// below is made on the OBSERVED STREAM OUTCOME per connection — never on
// grpc-go's global log — because grpclog.SetLoggerV2 is process-global and
// could not attribute a captured GOAWAY to one of three connections anyway.
func TestIntegration_OldHubToleratesHeartbeatingAgent(t *testing.T) {
	h := hubtest.StartTestHub(t)
	oldHubAddr := startOldHub(t, h)

	// Each connection needs its own device identity, so each gets its own
	// Join/Register/Subscribe via setupOldHubConnection.
	_, streamA := setupOldHubConnection(t, h, oldHubAddr, "oldhub-silent-dev", "oldhub-silent", 2222)
	connB, streamB := setupOldHubConnection(t, h, oldHubAddr, "oldhub-cadence10-dev", "oldhub-cadence10", 2223)
	connC, streamC := setupOldHubConnection(t, h, oldHubAddr, "oldhub-cadence25-dev", "oldhub-cadence25", 2224)

	// All three connections are registered and subscribed — the shared clock
	// starts now, which is the closest single reference point to the measured
	// one (elapsed from a connection's last RPC to its GOAWAY, not from
	// process start). It is not exact for A: A's last RPC was its Subscribe,
	// which precedes B's and C's setup, so every elapsed figure reported for A
	// is that setup cost too high. That is why it reads ~31s here against the
	// ~30.07s reference, and why aDeathDeadline is a bound with a full 19s of
	// margin rather than an equality check.
	start := time.Now()

	deadA := watchStreamDeath(streamA, start)
	deadB := watchStreamDeath(streamB, start)
	deadC := watchStreamDeath(streamC, start)

	hbCtxB, cancelHBB := context.WithCancel(context.Background())
	t.Cleanup(cancelHBB)
	go heartbeatLoop(hbCtxB, connB, productionCadence)

	hbCtxC, cancelHBC := context.WithCancel(context.Background())
	t.Cleanup(cancelHBC)
	go heartbeatLoop(hbCtxC, connC, boundaryCadence)

	// POSITIVE CONTROL: A must actually die. See aDeathDeadline's doc comment
	// for why the deadline is what it is — this is a wait-for-signal with a
	// bound, not a fixed sleep-then-check-once, so it neither races a slow CI
	// box nor pads out the common case.
	select {
	case dA := <-deadA:
		t.Logf("connection A (silent) stream died at %s elapsed since the shared clock started: %v", dA.at.Round(time.Millisecond), dA.err)

		// Pin the reason, not just the death. Two things ride on this string.
		//
		// First, it proves A died of the punishment rather than of anything
		// else that can kill a stream (a cleanup racing the assertion, the
		// fixture falling over) — without it the positive control proves only
		// "something went wrong", which is not the same claim.
		//
		// Second, it is the one externally observable trace of the CAUSE.
		// grpc-go's typed reason (GetGoAwayReason) never leaves
		// internal/transport, but the GOAWAY's debug data is formatted into
		// goAwayDebugMessage (http2_client.go:1417-1419) and embedded in the
		// status handed to every stream still active when the transport closes
		// (:1052-1053). "too_many_pings" itself is SERVER-sent — it is the
		// debug data of the GOAWAY frame (http2_server.go:913) — so it is
		// frozen in every old hub already deployed; only grpc-go's wrapper
		// wording around it could ever change. Asserting it here means a
		// grpc-go upgrade that stops surfacing the debug data breaks this
		// build instead of silently disabling the agent's own detection of it.
		require.ErrorContains(t, dA.err, "too_many_pings",
			"connection A must die of the keepalive punishment specifically, and the reason must "+
				"remain visible in the error an application can see")
	case <-time.After(aDeathDeadline):
		t.Fatalf("connection A (silent) stream did not die within %s of the shared clock starting — "+
			"the old-hub fixture stopped punishing an idle connection (observed here at ~31.0s, "+
			"measured reference ~30.07s from the last RPC); "+
			"this positive control failing means the regression assertions below would be meaningless even if they passed",
			aDeathDeadline)
	}

	// REGRESSION ASSERTIONS: B (10s) and C (25s) must both still be alive.
	// Stop their heartbeat loops first so nothing after this point can extend
	// their lives by luck.
	cancelHBB()
	cancelHBC()

	// Settle before reading the death channels. A regressed B or C would be
	// punished on the SAME ~30s schedule as A, so its death could land a few
	// milliseconds after A's — and a non-blocking read taken that instant would
	// see an empty channel and report "alive", passing the test for the one
	// failure it exists to catch. The grace is small next to the ~30s window it
	// guards, and it only ever runs once A has already died.
	const settle = 2 * time.Second
	time.Sleep(settle)

	select {
	case dB := <-deadB:
		t.Fatalf("connection B (production cadence, every %s) stream died at %s elapsed: %v — "+
			"the old hub punished a heartbeating agent too; issue #78's accidental compatibility does not hold",
			productionCadence, dB.at.Round(time.Millisecond), dB.err)
	default:
		t.Logf("connection B (production cadence, every %s) stream is still alive at %s elapsed", productionCadence, time.Since(start).Round(time.Millisecond))
	}

	select {
	case dC := <-deadC:
		t.Fatalf("connection C (boundary cadence, every %s) stream died at %s elapsed: %v — "+
			"the safe/punished boundary sits at or below %s, narrower than the production cadence alone would suggest",
			boundaryCadence, dC.at.Round(time.Millisecond), dC.err, boundaryCadence)
	default:
		t.Logf("connection C (boundary cadence, every %s) stream is still alive at %s elapsed", boundaryCadence, time.Since(start).Round(time.Millisecond))
	}
}
