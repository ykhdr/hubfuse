package agent

import (
	"context"
	"log/slog"
	"time"
)

// defaultHeartbeatInterval is how often the daemon pings the hub. It is a third
// of the hub's default 30s liveness timeout (see hub.DefaultHeartbeatTimeout),
// so two beats can be lost before the hub demotes the device.
const defaultHeartbeatInterval = 10 * time.Second

// heartbeatDangerCadence is the cadence at, or past, which
// heartbeatIntervalFromEnv still ACCEPTS the operator's value (this knob has
// no clamp and no reject path beyond malformed/non-positive input — scenario
// tests deliberately drive it, and the value may be a deliberate operator
// choice) but logs a warning, because two independent safety margins are gone
// at once:
//
//   - It is past the hub's own liveness window (hub.DefaultHeartbeatTimeout,
//     30s): a cadence this slow will get the device demoted and every peer's
//     mount torn down by a perfectly ordinary, fully up-to-date hub. This half
//     is true regardless of hub version.
//   - It is also past 3*hubKeepaliveTime (client.go), the bound
//     bounds_test.go pins: against a hub older than #72, a response no longer
//     lands before a third keepalive ping-strike accumulates, so the hub
//     answers with GOAWAY too_many_pings and drops the connection roughly
//     every 30s (measured in tests/integration/oldhub_test.go).
//
// Numerically these two bounds are the same 30s, but that is a coincidence,
// not a shared constant: internal/agent must not import internal/hub (the
// hub's timeout is independently configurable via --heartbeat-timeout /
// heartbeat-timeout), so nothing in this package can read
// hub.DefaultHeartbeatTimeout directly. The relationship lives only here, in
// this comment and in the warning text below — which is exactly why the
// warning leads with the modern-hub consequence: an operator who is told only
// about the old-hub GOAWAY path and fixes it by upgrading their hub is left
// with a cadence that is still broken against the hub they just upgraded to.
const heartbeatDangerCadence = 3 * hubKeepaliveTime

// heartbeatIntervalFromEnv resolves the heartbeat interval from the raw
// HUBFUSE_HEARTBEAT_INTERVAL value — a test handle for scenario tests that run
// the hub with a shortened liveness timeout, mirroring
// HUBFUSE_MOUNT_MONITOR_INTERVAL. An empty value keeps def.
//
// Unlike the mount monitor's knob, a non-positive value does NOT disable
// anything: a daemon that stops heartbeating is guaranteed to be demoted and
// have its shares unmounted by every peer, so "off" is not a state this handle
// is allowed to express. Both a malformed and a non-positive value log a WARN
// and fall back to def. It is a pure helper so the rules are unit-testable
// without driving NewDaemon. (#69)
//
// A value at or past heartbeatDangerCadence is a third case: it is neither
// malformed nor non-positive, so it is still returned unchanged — but it warns,
// because at that cadence the value is very likely a mistake rather than a
// deliberate choice. See heartbeatDangerCadence's comment for why the warning
// text orders its two reasons the way it does. (#78)
func heartbeatIntervalFromEnv(raw string, def time.Duration, logger *slog.Logger) time.Duration {
	if raw == "" {
		return def
	}
	interval, err := time.ParseDuration(raw)
	if err != nil {
		logger.Warn("invalid HUBFUSE_HEARTBEAT_INTERVAL; using default",
			"value", raw,
			"default", def,
			"error", err,
		)
		return def
	}
	if interval <= 0 {
		logger.Warn("HUBFUSE_HEARTBEAT_INTERVAL must be positive; using default",
			"value", raw,
			"default", def,
		)
		return def
	}
	if interval >= heartbeatDangerCadence {
		// PRIMARY reason first: true against every hub, old or new. SECONDARY
		// reason (the pre-#72 GOAWAY loop) is additional, not alternative — an
		// operator who reads only the secondary reason and "fixes" this by
		// upgrading the hub is left with the identical broken cadence.
		logger.Warn(
			"HUBFUSE_HEARTBEAT_INTERVAL is so slow the hub will mark this device offline and every peer will unmount its shares; accepting it, but this is almost certainly a mistake",
			"value", raw,
			"interval", interval,
			"hub_liveness_timeout_hint", "hub.DefaultHeartbeatTimeout defaults to 30s",
			"also_true_against_older_hubs", "at this cadence three keepalive pings can also accumulate with no response between them, so the hub additionally answers with GOAWAY too_many_pings and drops the connection roughly every 30s",
		)
	}
	return interval
}

// minHeartbeatRPCTimeout floors the per-call deadline of a heartbeat RPC, so a
// shortened test cadence does not become a hair-trigger against a hub that
// needs a moment to answer. (#69)
const minHeartbeatRPCTimeout = 2 * time.Second

// startHeartbeat launches the heartbeat loop exactly once for the daemon's
// lifetime, whichever caller gets there first.
//
// It is called from sessionOnce the moment a Register succeeds — BEFORE the
// initial mount reconciliation — because that reconciliation can block for a
// long time: every mount that cannot be established burns the full
// mountVerifyTimeout under the mounter lock, and a stale entry in config is
// enough to do it. Liveness used to start only after all of that (runServices,
// after registerAndSubscribe returned), so a single unreachable mount could
// keep the daemon from ever proving it was alive within the hub's 30s window;
// the hub demoted it and its peers unmounted the shares it was serving. (#69)
//
// runServices still calls this as a safety net: the Once makes the second call
// free, and the daemon is then never left without a heartbeat if the register
// path changes shape.
//
// ctx must be the daemon-lifetime context (both Run and supervise pass exactly
// that), so the single goroutine lives until shutdown regardless of how many
// hub sessions come and go.
func (d *Daemon) startHeartbeat(ctx context.Context) {
	d.heartbeatOnce.Do(func() {
		go d.runHeartbeat(ctx)
	})
}

// runHeartbeat sends a Heartbeat RPC to the hub on every tick until ctx is
// cancelled. Transient errors are logged as warnings but never stop the loop:
// a hub that is briefly unreachable must not cost the daemon its liveness once
// the hub returns.
func (d *Daemon) runHeartbeat(ctx context.Context) {
	if d.heartbeatFn == nil {
		// Only reachable for a Daemon assembled outside NewDaemon (unit-test
		// fixtures). Log and return rather than panic on a nil call.
		d.logger.Error("heartbeat loop not started: no heartbeat transport configured")
		return
	}

	interval := d.heartbeatInterval
	if interval <= 0 {
		// NewDaemon always sets a positive value; this guards fixtures that
		// build a Daemon literally, where time.NewTicker(0) would panic.
		interval = defaultHeartbeatInterval
	}
	rpcTimeout := interval
	if rpcTimeout < minHeartbeatRPCTimeout {
		// A very short cadence (scenario tests run at sub-second intervals)
		// must not turn every beat into a deadline error on a hub that needs a
		// moment to answer.
		rpcTimeout = minHeartbeatRPCTimeout
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Bound every call. A heartbeat RPC on a half-open connection can
			// hang indefinitely, and the loop would then stop beating entirely
			// (ticks are dropped while it is blocked) — the hub demotes the
			// device and its peers unmount, which is the very outcome this loop
			// exists to prevent. One interval is the natural budget: a call
			// that has not answered by the time the next beat is due is already
			// too late to be useful. (#69)
			beatCtx, cancel := context.WithTimeout(ctx, rpcTimeout)
			err := d.heartbeatFn(beatCtx)
			cancel()
			if err == nil {
				d.clearHeartbeatFailures()
				continue
			}

			d.logger.Warn("heartbeat failed", "error", err)

			// Consecutive failures are the application-level evidence that this
			// session is over. gRPC keepalive covers a transport nobody
			// answers, but a hub that answers PINGs at the HTTP/2 layer while
			// its RPCs go nowhere looks perfectly healthy to it — only a real
			// call can tell, and the heartbeat is the one that runs on a fixed
			// cadence. Ending the session hands the recovery to the supervisor,
			// which already knows how to re-register and remount. (#72)
			d.noteHeartbeatFailure()
		}
	}
}
