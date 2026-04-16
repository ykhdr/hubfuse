package hub

import (
	"context"
	"log/slog"
	"time"

	"github.com/ykhdr/hubfuse/internal/hub/store"
)

// DefaultHeartbeatTimeout is the default duration a device may go without a
// heartbeat before being marked offline.
const DefaultHeartbeatTimeout = 30 * time.Second

// HeartbeatMonitor periodically checks for stale devices and marks them offline.
// Every invitePruneEvery ticks it also prunes expired invite codes from the store.
type HeartbeatMonitor struct {
	registry *Registry
	store    store.Store
	timeout  time.Duration // how long without a heartbeat before a device is stale
	check    time.Duration // how often to run the stale check
	logger   *slog.Logger
}

// invitePruneEvery is how many stale-check ticks elapse between invite-prune
// runs. With the default 10s check interval this yields a ~60s prune cadence.
const invitePruneEvery = 6

// NewHeartbeatMonitor creates a HeartbeatMonitor. The timeout is how long a
// device may go without a heartbeat before being marked offline; the check
// interval defaults to timeout/3 (minimum 1 second).
func NewHeartbeatMonitor(registry *Registry, s store.Store, timeout time.Duration, logger *slog.Logger) *HeartbeatMonitor {
	if timeout == 0 {
		timeout = DefaultHeartbeatTimeout
	}
	check := timeout / 3
	if check < time.Second {
		check = time.Second
	}
	return &HeartbeatMonitor{
		registry: registry,
		store:    s,
		timeout:  timeout,
		check:    check,
		logger:   logger,
	}
}

// Start runs the heartbeat monitor until ctx is cancelled. On each tick it
// fetches all devices whose last heartbeat is older than the configured timeout
// and marks them offline via the registry. Once every invitePruneEvery ticks
// it also prunes expired invite codes.
func (m *HeartbeatMonitor) Start(ctx context.Context) {
	ticker := time.NewTicker(m.check)
	defer ticker.Stop()

	tickCount := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.checkStale(ctx)
			tickCount++
			if tickCount%invitePruneEvery == 0 {
				if err := m.store.DeleteExpiredInvites(ctx); err != nil {
					m.logger.Warn("heartbeat monitor: prune expired invites", slog.Any("error", err))
				}
			}
		}
	}
}

// checkStale fetches stale devices and marks each offline.
func (m *HeartbeatMonitor) checkStale(ctx context.Context) {
	threshold := time.Now().Add(-m.timeout)
	stale, err := m.store.GetStaleDevices(ctx, threshold)
	if err != nil {
		m.logger.Error("heartbeat monitor: get stale devices", slog.Any("error", err))
		return
	}

	for _, d := range stale {
		if err := m.registry.MarkOffline(ctx, d); err != nil {
			m.logger.Error("heartbeat monitor: mark offline",
				slog.String("device_id", d.DeviceID),
				slog.Any("error", err))
		} else {
			m.logger.Info("heartbeat monitor: marked device offline",
				slog.String("device_id", d.DeviceID))
		}
	}
}
