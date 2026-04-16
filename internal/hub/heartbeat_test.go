package hub

import (
	"context"
	"testing"
	"time"

	"github.com/ykhdr/hubfuse/internal/hub/store"
)

func TestHeartbeatMonitor_MarksStaleDeviceOffline(t *testing.T) {
	r := newTestRegistry(t)
	bg := context.Background()

	joinDevice(t, r, "dev-stale2", "stale-device2")

	// Set the device online. Its last_heartbeat stays at zero (always stale).
	if err := r.store.UpdateDeviceStatus(bg, "dev-stale2", store.StatusOnline, "10.0.0.1", 22); err != nil {
		t.Fatalf("UpdateDeviceStatus: %v", err)
	}

	timeout := 50 * time.Millisecond
	monitor := NewHeartbeatMonitor(r, r.store, timeout, 0, r.logger)

	monCtx, cancel := context.WithCancel(bg)
	go monitor.Start(monCtx)
	defer cancel()

	// Poll until the device is marked offline or we time out.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d, err := r.store.GetDevice(bg, "dev-stale2")
		if err != nil {
			t.Fatalf("GetDevice: %v", err)
		}
		if d.Status == store.StatusOffline {
			return // success
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("device was not marked offline within 2 seconds")
}

func TestHeartbeatMonitor_DoesNotMarkFreshDeviceOffline(t *testing.T) {
	r := newTestRegistry(t)
	monCtx, cancel := context.WithCancel(context.Background())
	bg := context.Background()

	joinDevice(t, r, "dev-fresh", "fresh-device")
	registerDevice(t, r, "dev-fresh", "10.0.0.1", 22)

	// Update heartbeat to now so the device is fresh.
	if err := r.Heartbeat(bg, "dev-fresh"); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}

	timeout := 10 * time.Second // long timeout — device won't go stale
	monitor := NewHeartbeatMonitor(r, r.store, timeout, 0, r.logger)

	go monitor.Start(monCtx)
	time.Sleep(100 * time.Millisecond)
	cancel()

	d, err := r.store.GetDevice(bg, "dev-fresh")
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	if d.Status != store.StatusOnline {
		t.Errorf("Status = %q, want online", d.Status)
	}
}

func TestHeartbeatMonitor_BroadcastsOfflineEvent(t *testing.T) {
	r := newTestRegistry(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	joinDevice(t, r, "dev-stale", "stale-device")
	joinDevice(t, r, "dev-watcher", "watcher")

	// Put dev-stale online with stale heartbeat.
	if err := r.store.UpdateDeviceStatus(ctx, "dev-stale", store.StatusOnline, "10.0.0.1", 22); err != nil {
		t.Fatalf("UpdateDeviceStatus: %v", err)
	}

	ch, unsub := r.Subscribe("dev-watcher")
	defer unsub()

	timeout := 50 * time.Millisecond
	monitor := NewHeartbeatMonitor(r, r.store, timeout, 0, r.logger)
	go monitor.Start(ctx)

	select {
	case event := <-ch:
		cancel()
		if event.GetDeviceOffline() == nil {
			t.Errorf("expected DeviceOffline event, got %T", event.GetPayload())
		}
		if event.GetDeviceOffline().DeviceId != "dev-stale" {
			t.Errorf("DeviceId = %q, want dev-stale", event.GetDeviceOffline().DeviceId)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for DeviceOffline event from heartbeat monitor")
	}
}

func TestHeartbeatMonitor_StopsOnContextCancel(t *testing.T) {
	r := newTestRegistry(t)
	ctx, cancel := context.WithCancel(context.Background())

	timeout := time.Minute // won't fire
	monitor := NewHeartbeatMonitor(r, r.store, timeout, 0, r.logger)

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

func TestHeartbeatMonitor_PrunesInactiveDevices(t *testing.T) {
	r := newTestRegistry(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	joinDevice(t, r, "dev-prune", "prune-me")
	joinDevice(t, r, "dev-active", "active-sub")
	joinDevice(t, r, "dev-recent", "recent-offline")

	// Make dev-recent fresh so it should not be pruned.
	if err := r.Heartbeat(ctx, "dev-recent"); err != nil {
		t.Fatalf("Heartbeat dev-recent: %v", err)
	}

	// Simulate an active subscriber for dev-active to ensure it is skipped.
	activeCh, activeUnsub := r.Subscribe("dev-active")
	defer activeUnsub()
	// Drain to avoid blocking (channel may never get events).
	go func() {
		for range activeCh {
		}
	}()

	watchCh, watchUnsub := r.Subscribe("watcher")
	defer watchUnsub()

	stopHeartbeats := make(chan struct{})
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopHeartbeats:
				return
			case <-ticker.C:
				_ = r.Heartbeat(ctx, "dev-recent")
			}
		}
	}()
	defer close(stopHeartbeats)

	retention := 50 * time.Millisecond
	monitor := NewHeartbeatMonitor(r, r.store, 0, retention, r.logger)
	go monitor.Start(ctx)

	var gotRemoved bool
	select {
	case event := <-watchCh:
		if removed := event.GetDeviceRemoved(); removed != nil {
			if removed.DeviceId != "dev-prune" {
				t.Fatalf("DeviceRemoved device_id = %q, want dev-prune", removed.DeviceId)
			}
			gotRemoved = true
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for DeviceRemoved event")
	}

	if !gotRemoved {
		t.Fatal("expected DeviceRemoved event for dev-prune")
	}

	if _, err := r.store.GetDevice(ctx, "dev-prune"); err == nil {
		t.Fatal("dev-prune still present after pruning")
	}
	if _, err := r.store.GetDevice(ctx, "dev-active"); err != nil {
		t.Fatalf("dev-active should not be pruned: %v", err)
	}
	if _, err := r.store.GetDevice(ctx, "dev-recent"); err != nil {
		t.Fatalf("dev-recent should not be pruned: %v", err)
	}
}

func TestNewHeartbeatMonitor_DefaultCheckInterval(t *testing.T) {
	r := newTestRegistry(t)
	timeout := 30 * time.Second
	monitor := NewHeartbeatMonitor(r, r.store, timeout, 0, r.logger)

	expectedCheck := timeout / 3
	if monitor.check != expectedCheck {
		t.Errorf("check interval = %v, want %v", monitor.check, expectedCheck)
	}
}
