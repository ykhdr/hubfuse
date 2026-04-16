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

// heartbeatInterval is how often the heartbeat monitor checks for stale devices.
const heartbeatInterval = 10 * time.Second

// DefaultDeviceRetention is how long offline devices are kept before pruning.
const DefaultDeviceRetention = 7 * 24 * time.Hour

// HeartbeatMonitor periodically checks for stale devices and marks them offline.
// Every invitePruneEvery ticks it also prunes expired invite codes from the store.
type HeartbeatMonitor struct {
	registry   *Registry
	store      store.Store
	timeout    time.Duration // how long without a heartbeat before a device is stale
	check      time.Duration // how often to run the stale check
	retention  time.Duration // how long to keep offline devices before pruning
	pruneEvery time.Duration // cadence for pruning offline devices
	logger     *slog.Logger
}

// invitePruneEvery is how many stale-check ticks elapse between invite-prune
// runs. With the default 10s check interval this yields a ~60s prune cadence.
const invitePruneEvery = 6

// devicePruneInterval is the default cadence for pruning offline devices.
const devicePruneInterval = time.Hour

// NewHeartbeatMonitor creates a HeartbeatMonitor. The timeout is how long a
// device may go without a heartbeat before being marked offline. When timeout
// is zero, DefaultHeartbeatTimeout and heartbeatInterval are used.
// retention controls how long offline devices are retained before pruning;
// zero disables pruning.
func NewHeartbeatMonitor(registry *Registry, s store.Store, timeout, retention time.Duration, logger *slog.Logger) *HeartbeatMonitor {
	check := heartbeatInterval
	if timeout == 0 {
		timeout = DefaultHeartbeatTimeout
	} else {
		check = timeout / 3
		if check < time.Second {
			check = time.Second
		}
	}
	pruneEvery := devicePruneInterval
	if retention > 0 && retention < pruneEvery {
		pruneEvery = retention
	}
	return &HeartbeatMonitor{
		registry:   registry,
		store:      s,
		timeout:    timeout,
		check:      check,
		retention:  retention,
		pruneEvery: pruneEvery,
		logger:     logger,
	}
}

// Start runs the heartbeat monitor until ctx is cancelled. On each tick it
// fetches all devices whose last heartbeat is older than the configured timeout
// and marks them offline via the registry. Once every invitePruneEvery ticks
// it also prunes expired invite codes.
func (m *HeartbeatMonitor) Start(ctx context.Context) {
	ticker := time.NewTicker(m.check)
	defer ticker.Stop()

	var pruneTicker *time.Ticker
	var pruneCh <-chan time.Time
	if m.retention > 0 {
		pruneTicker = time.NewTicker(m.pruneEvery)
		pruneCh = pruneTicker.C
		defer pruneTicker.Stop()
	}

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
		case <-pruneCh:
			m.pruneInactive(ctx)
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

// pruneInactive deletes offline devices whose last heartbeat is older than the
// configured retention window. Devices with an active subscriber stream are
// skipped.
func (m *HeartbeatMonitor) pruneInactive(ctx context.Context) {
	threshold := time.Now().Add(-m.retention)
	activeSubs := m.registry.ActiveSubscribers()

	pruned, err := m.store.DeletePrunedDevices(ctx, threshold, activeSubs)
	if err != nil {
		m.logger.Error("heartbeat monitor: prune inactive devices", slog.Any("error", err))
		return
	}

	for _, d := range pruned {
		m.registry.removeSubscriber(d.DeviceID)
		m.registry.BroadcastDeviceRemoved(d)
		m.logger.Info("heartbeat monitor: pruned inactive device",
			slog.String("device_id", d.DeviceID),
			slog.String("nickname", d.Nickname))
	}
}
