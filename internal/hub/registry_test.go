package hub

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/hub/store"
	pb "github.com/ykhdr/hubfuse/proto"
)

// newTestRegistry creates an in-memory store, generates a test CA, and returns
// a ready-to-use Registry.
func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	s, err := store.NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	caCert, caKey, err := common.GenerateCA()
	if err != nil {
		t.Fatalf("GenerateCA: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return NewRegistry(s, caCert, caKey, logger)
}

// joinDevice is a helper that calls Join and fatals on error.
func joinDevice(t *testing.T, r *Registry, deviceID, nickname, ip string) {
	t.Helper()
	if _, _, _, err := r.Join(context.Background(), deviceID, nickname, ip); err != nil {
		t.Fatalf("Join(%q, %q): %v", deviceID, nickname, err)
	}
}

// registerDevice is a helper that calls Register (online) and fatals on error.
func registerDevice(t *testing.T, r *Registry, deviceID, ip string, port int) []*store.Device {
	t.Helper()
	online, err := r.Register(context.Background(), deviceID, ip, port, nil, 1)
	if err != nil {
		t.Fatalf("Register(%q): %v", deviceID, err)
	}
	return online
}

// --- Join ---

func TestJoin_Success(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	certPEM, keyPEM, caCertPEM, err := r.Join(ctx, "dev-1", "alice", "1.2.3.4")
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	if len(certPEM) == 0 {
		t.Error("certPEM is empty")
	}
	if len(keyPEM) == 0 {
		t.Error("keyPEM is empty")
	}
	if len(caCertPEM) == 0 {
		t.Error("caCertPEM is empty")
	}

	// Verify the device was created in the store.
	d, err := r.store.GetDevice(ctx, "dev-1")
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	if d.Nickname != "alice" {
		t.Errorf("Nickname = %q, want %q", d.Nickname, "alice")
	}
	if d.Status != store.StatusRegistered {
		t.Errorf("Status = %q, want %q", d.Status, store.StatusRegistered)
	}
	if d.LastIP != "1.2.3.4" {
		t.Errorf("LastIP = %q, want 1.2.3.4", d.LastIP)
	}
}

func TestJoin_DuplicateNickname(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	if _, _, _, err := r.Join(ctx, "dev-1", "alice", ""); err != nil {
		t.Fatalf("first Join: %v", err)
	}

	_, _, _, err := r.Join(ctx, "dev-2", "alice", "")
	if err == nil {
		t.Fatal("expected error for duplicate nickname, got nil")
	}
	if err != common.ErrNicknameTaken {
		t.Errorf("error = %v, want ErrNicknameTaken", err)
	}
}

func TestJoin_DuplicateDeviceID(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	if _, _, _, err := r.Join(ctx, "dev-1", "alice", ""); err != nil {
		t.Fatalf("first Join: %v", err)
	}

	// Same device_id, different nickname — store should reject the duplicate.
	_, _, _, err := r.Join(ctx, "dev-1", "bob", "")
	if err == nil {
		t.Fatal("expected error for duplicate device_id, got nil")
	}
}

// --- Register ---

func TestRegister_ReturnsOnlineDevices(t *testing.T) {
	r := newTestRegistry(t)

	joinDevice(t, r, "dev-1", "alice", "")
	joinDevice(t, r, "dev-2", "bob", "")

	online := registerDevice(t, r, "dev-1", "10.0.0.1", 22)
	if len(online) != 1 {
		t.Fatalf("online count = %d, want 1", len(online))
	}
	if online[0].DeviceID != "dev-1" {
		t.Errorf("DeviceID = %q, want dev-1", online[0].DeviceID)
	}

	// Register second device; both should now be in the online list.
	online = registerDevice(t, r, "dev-2", "10.0.0.2", 22)
	if len(online) != 2 {
		t.Fatalf("online count = %d, want 2", len(online))
	}
}

func TestRegister_SetsHeartbeat(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	joinDevice(t, r, "dev-1", "alice", "")

	before := time.Now()
	if _, err := r.Register(ctx, "dev-1", "10.0.0.1", 22, nil, 1); err != nil {
		t.Fatalf("Register: %v", err)
	}

	d, err := r.store.GetDevice(ctx, "dev-1")
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	if d.LastHeartbeat.IsZero() || d.LastHeartbeat.Before(before) {
		t.Errorf("LastHeartbeat not set on register, got %v", d.LastHeartbeat)
	}
	if d.Status != store.StatusOnline {
		t.Errorf("Status = %q, want online", d.Status)
	}
}

func TestRegister_BroadcastsDeviceOnline(t *testing.T) {
	r := newTestRegistry(t)

	joinDevice(t, r, "dev-1", "alice", "")
	joinDevice(t, r, "dev-2", "bob", "")

	// Subscribe dev-2 before dev-1 registers.
	ch, unsub := r.Subscribe("dev-2")
	defer unsub()

	registerDevice(t, r, "dev-1", "10.0.0.1", 22)

	select {
	case event := <-ch:
		if event.GetDeviceOnline() == nil {
			t.Errorf("expected DeviceOnline event, got %T", event.GetPayload())
		}
		if event.GetDeviceOnline().DeviceId != "dev-1" {
			t.Errorf("DeviceId = %q, want dev-1", event.GetDeviceOnline().DeviceId)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for DeviceOnline event")
	}
}

func TestRegister_WithShares(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	joinDevice(t, r, "dev-1", "alice", "")

	shares := []*pb.Share{
		{Alias: "docs", Permissions: "ro", AllowedDevices: []string{"all"}},
	}
	_, err := r.Register(ctx, "dev-1", "10.0.0.1", 22, shares, 1)
	if err != nil {
		t.Fatalf("Register with shares: %v", err)
	}

	stored, err := r.store.GetShares(ctx, "dev-1")
	if err != nil {
		t.Fatalf("GetShares: %v", err)
	}
	if len(stored) != 1 {
		t.Fatalf("stored shares = %d, want 1", len(stored))
	}
	if stored[0].Alias != "docs" {
		t.Errorf("Alias = %q, want docs", stored[0].Alias)
	}
}

// --- Heartbeat ---

func TestHeartbeat_UpdatesTimestamp(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	joinDevice(t, r, "dev-1", "alice", "")

	before := time.Now()
	if err := r.Heartbeat(ctx, "dev-1"); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	after := time.Now()

	d, err := r.store.GetDevice(ctx, "dev-1")
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	if d.LastHeartbeat.Before(before) || d.LastHeartbeat.After(after) {
		t.Errorf("LastHeartbeat %v not in expected range [%v, %v]",
			d.LastHeartbeat, before, after)
	}
}

// --- Subscribe + Broadcast ---

func TestSubscribe_ReceivesEvents(t *testing.T) {
	r := newTestRegistry(t)

	ch, unsub := r.Subscribe("dev-1")
	defer unsub()

	event := &pb.Event{
		Payload: &pb.Event_DeviceOnline{
			DeviceOnline: &pb.DeviceOnlineEvent{DeviceId: "dev-2"},
		},
	}
	r.Broadcast(event, "dev-2")

	select {
	case got := <-ch:
		if got.GetDeviceOnline().DeviceId != "dev-2" {
			t.Errorf("DeviceId = %q, want dev-2", got.GetDeviceOnline().DeviceId)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for event")
	}
}

func TestBroadcast_ExcludesSender(t *testing.T) {
	r := newTestRegistry(t)

	ch1, unsub1 := r.Subscribe("dev-1")
	defer unsub1()
	ch2, unsub2 := r.Subscribe("dev-2")
	defer unsub2()

	event := &pb.Event{
		Payload: &pb.Event_DeviceOnline{
			DeviceOnline: &pb.DeviceOnlineEvent{DeviceId: "dev-1"},
		},
	}
	// Exclude dev-1 — only dev-2 should receive.
	r.Broadcast(event, "dev-1")

	// ch2 should receive the event.
	select {
	case <-ch2:
		// good
	case <-time.After(time.Second):
		t.Fatal("dev-2 did not receive event")
	}

	// ch1 should NOT receive the event.
	select {
	case <-ch1:
		t.Fatal("dev-1 received its own event (should be excluded)")
	case <-time.After(10 * time.Millisecond):
		// good — nothing received
	}
}

func TestSubscribe_UnsubscribeStopsEvents(t *testing.T) {
	r := newTestRegistry(t)

	ch, unsub := r.Subscribe("dev-1")
	unsub()

	event := &pb.Event{
		Payload: &pb.Event_DeviceOnline{
			DeviceOnline: &pb.DeviceOnlineEvent{DeviceId: "dev-2"},
		},
	}
	// After unsub, Broadcast should not send to dev-1.
	r.Broadcast(event, "dev-2")

	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("received event after unsubscribe")
		}
		// Channel was closed — ok
	case <-time.After(10 * time.Millisecond):
		// No event delivered, as expected.
	}
}

// --- Deregister ---

func TestDeregister_MarksOfflineAndBroadcasts(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	joinDevice(t, r, "dev-1", "alice", "")
	joinDevice(t, r, "dev-2", "bob", "")
	registerDevice(t, r, "dev-1", "10.0.0.1", 22)

	ch, unsub := r.Subscribe("dev-2")
	defer unsub()

	if err := r.Deregister(ctx, "dev-1"); err != nil {
		t.Fatalf("Deregister: %v", err)
	}

	// dev-1 must be offline in the store.
	d, err := r.store.GetDevice(ctx, "dev-1")
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	if d.Status != store.StatusOffline {
		t.Errorf("Status = %q, want offline", d.Status)
	}

	// dev-2 should receive a DeviceOffline event.
	select {
	case event := <-ch:
		if event.GetDeviceOffline() == nil {
			t.Errorf("expected DeviceOffline, got %T", event.GetPayload())
		}
		if event.GetDeviceOffline().DeviceId != "dev-1" {
			t.Errorf("DeviceId = %q, want dev-1", event.GetDeviceOffline().DeviceId)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for DeviceOffline event")
	}
}

func TestDeregister_RemovesSubscriber(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	joinDevice(t, r, "dev-1", "alice", "")
	registerDevice(t, r, "dev-1", "10.0.0.1", 22)

	ch, _ := r.Subscribe("dev-1")

	if err := r.Deregister(ctx, "dev-1"); err != nil {
		t.Fatalf("Deregister: %v", err)
	}

	r.mu.RLock()
	_, exists := r.subscribers["dev-1"]
	r.mu.RUnlock()

	if exists {
		t.Error("subscriber still present after Deregister")
	}

	// Reading from a closed channel must return immediately with the zero value.
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("channel is not closed after Deregister")
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for channel to be closed by Deregister")
	}
}

// --- UpdateShares ---

func TestUpdateShares_StoresAndBroadcasts(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	joinDevice(t, r, "dev-1", "alice", "")
	joinDevice(t, r, "dev-2", "bob", "")

	ch, unsub := r.Subscribe("dev-2")
	defer unsub()

	shares := []*pb.Share{
		{Alias: "music", Permissions: "rw", AllowedDevices: []string{"dev-2"}},
	}
	if err := r.UpdateShares(ctx, "dev-1", shares); err != nil {
		t.Fatalf("UpdateShares: %v", err)
	}

	// Verify stored.
	stored, err := r.store.GetShares(ctx, "dev-1")
	if err != nil {
		t.Fatalf("GetShares: %v", err)
	}
	if len(stored) != 1 || stored[0].Alias != "music" {
		t.Errorf("unexpected stored shares: %v", stored)
	}

	// Verify broadcast.
	select {
	case event := <-ch:
		if event.GetSharesUpdated() == nil {
			t.Errorf("expected SharesUpdated, got %T", event.GetPayload())
		}
		if event.GetSharesUpdated().DeviceId != "dev-1" {
			t.Errorf("DeviceId = %q, want dev-1", event.GetSharesUpdated().DeviceId)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for SharesUpdated event")
	}
}

// --- Rename ---

func TestRename_Success(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	joinDevice(t, r, "dev-1", "alice", "")

	if err := r.Rename(ctx, "dev-1", "alice2"); err != nil {
		t.Fatalf("Rename: %v", err)
	}

	d, err := r.store.GetDevice(ctx, "dev-1")
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	if d.Nickname != "alice2" {
		t.Errorf("Nickname = %q, want alice2", d.Nickname)
	}
}

func TestRename_DuplicateNickname(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	joinDevice(t, r, "dev-1", "alice", "")
	joinDevice(t, r, "dev-2", "bob", "")

	err := r.Rename(ctx, "dev-1", "bob")
	if err == nil {
		t.Fatal("expected error for duplicate nickname, got nil")
	}
	if err != common.ErrNicknameTaken {
		t.Errorf("error = %v, want ErrNicknameTaken", err)
	}
}

// --- MarkOffline ---

func TestMarkOffline_MarksAndBroadcasts(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	joinDevice(t, r, "dev-1", "alice", "")
	joinDevice(t, r, "dev-2", "bob", "")
	registerDevice(t, r, "dev-1", "10.0.0.1", 22)

	ch, unsub := r.Subscribe("dev-2")
	defer unsub()

	d1, err := r.store.GetDevice(ctx, "dev-1")
	if err != nil {
		t.Fatalf("GetDevice before MarkOffline: %v", err)
	}
	if err := r.MarkOffline(ctx, d1); err != nil {
		t.Fatalf("MarkOffline: %v", err)
	}

	d, err := r.store.GetDevice(ctx, "dev-1")
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	if d.Status != store.StatusOffline {
		t.Errorf("Status = %q, want offline", d.Status)
	}

	select {
	case event := <-ch:
		if event.GetDeviceOffline() == nil {
			t.Errorf("expected DeviceOffline, got %T", event.GetPayload())
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for DeviceOffline event")
	}
}
