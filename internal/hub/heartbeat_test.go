package hub

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ykhdr/hubfuse/internal/hub/store"
)

func TestHeartbeatMonitor_MarksStaleDeviceOffline(t *testing.T) {
	r := newTestRegistry(t)
	bg := context.Background()

	joinDevice(t, r, "dev-stale2", "stale-device2")

	// Set the device online. Its last_heartbeat stays at zero (always stale).
	err := r.store.UpdateDeviceStatus(bg, "dev-stale2", store.StatusOnline, "10.0.0.1", 22)
	require.NoError(t, err, "UpdateDeviceStatus")

	timeout := 50 * time.Millisecond
	monitor := NewHeartbeatMonitor(r, r.store, timeout, r.logger)

	monCtx, cancel := context.WithCancel(bg)
	go monitor.Start(monCtx)
	defer cancel()

	// Poll until the device is marked offline or we time out.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d, err := r.store.GetDevice(bg, "dev-stale2")
		require.NoError(t, err, "GetDevice")
		if d.Status == store.StatusOffline {
			return // success
		}
		time.Sleep(10 * time.Millisecond)
	}
	assert.Fail(t, "device was not marked offline within 2 seconds")
}

func TestHeartbeatMonitor_DoesNotMarkFreshDeviceOffline(t *testing.T) {
	r := newTestRegistry(t)
	monCtx, cancel := context.WithCancel(context.Background())
	bg := context.Background()

	joinDevice(t, r, "dev-fresh", "fresh-device")
	registerDevice(t, r, "dev-fresh", "10.0.0.1", 22)

	// Update heartbeat to now so the device is fresh.
	err := r.Heartbeat(bg, "dev-fresh")
	require.NoError(t, err, "Heartbeat")

	timeout := 10 * time.Second // long timeout — device won't go stale
	monitor := NewHeartbeatMonitor(r, r.store, timeout, r.logger)

	go monitor.Start(monCtx)
	time.Sleep(100 * time.Millisecond)
	cancel()

	d, err := r.store.GetDevice(bg, "dev-fresh")
	require.NoError(t, err, "GetDevice")
	assert.Equal(t, store.StatusOnline, d.Status)
}

func TestHeartbeatMonitor_BroadcastsOfflineEvent(t *testing.T) {
	r := newTestRegistry(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	joinDevice(t, r, "dev-stale", "stale-device")
	joinDevice(t, r, "dev-watcher", "watcher")

	// Put dev-stale online with stale heartbeat.
	err := r.store.UpdateDeviceStatus(ctx, "dev-stale", store.StatusOnline, "10.0.0.1", 22)
	require.NoError(t, err, "UpdateDeviceStatus")

	ch, unsub := r.Subscribe("dev-watcher")
	defer unsub()

	timeout := 50 * time.Millisecond
	monitor := NewHeartbeatMonitor(r, r.store, timeout, r.logger)
	go monitor.Start(ctx)

	select {
	case event := <-ch:
		cancel()
		assert.NotNil(t, event.GetDeviceOffline(), "expected DeviceOffline event, got %T", event.GetPayload())
		if event.GetDeviceOffline() != nil {
			assert.Equal(t, "dev-stale", event.GetDeviceOffline().DeviceId)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for DeviceOffline event from heartbeat monitor")
	}
}

func TestHeartbeatMonitor_StopsOnContextCancel(t *testing.T) {
	r := newTestRegistry(t)
	ctx, cancel := context.WithCancel(context.Background())

	timeout := time.Minute // won't fire
	monitor := NewHeartbeatMonitor(r, r.store, timeout, r.logger)

	done := make(chan struct{})
	go func() {
		monitor.Start(ctx)
		close(done)
	}()

	cancel()

	select {
	case <-done:
		// monitor exited cleanly
	case <-time.After(time.Second):
		t.Fatal("heartbeat monitor did not stop after context cancellation")
	}
}

func TestNewHeartbeatMonitor_DefaultCheckInterval(t *testing.T) {
	r := newTestRegistry(t)
	timeout := 30 * time.Second
	monitor := NewHeartbeatMonitor(r, r.store, timeout, r.logger)

	expectedCheck := timeout / 3
	assert.Equal(t, expectedCheck, monitor.check)
}
