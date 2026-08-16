package hub

import (
	"context"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/hub/store"
	pb "github.com/ykhdr/hubfuse/proto"
)

// newTestRegistry creates an in-memory store, generates a test CA, and returns
// a ready-to-use Registry.
func newTestRegistry(t *testing.T) *Registry {
	t.Helper()
	s, err := store.NewSQLiteStore(":memory:")
	require.NoError(t, err, "NewSQLiteStore")
	t.Cleanup(func() { s.Close() })

	caCert, caKey, err := common.GenerateCA()
	require.NoError(t, err, "GenerateCA")

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return NewRegistry(s, caCert, caKey, logger, 0)
}

// joinDevice is a helper that issues a join token and calls Join, fataling on error.
func joinDevice(t *testing.T, r *Registry, deviceID, nickname, ip string) {
	t.Helper()
	ctx := context.Background()
	token, _, err := r.IssueJoinToken(ctx)
	require.NoErrorf(t, err, "IssueJoinToken for Join(%q, %q)", deviceID, nickname)
	_, _, _, err = r.Join(ctx, deviceID, nickname, ip, token)
	require.NoErrorf(t, err, "Join(%q, %q)", deviceID, nickname)
}

// registerDevice is a helper that calls Register (online) and fatals on error.
func registerDevice(t *testing.T, r *Registry, deviceID, ip string, port int) []*store.Device {
	t.Helper()
	online, err := r.Register(context.Background(), deviceID, ip, port, nil, common.ProtocolVersion)
	require.NoErrorf(t, err, "Register(%q)", deviceID)
	return online
}

// --- Join ---

func TestJoin_Success(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	token, _, err := r.IssueJoinToken(ctx)
	require.NoError(t, err, "IssueJoinToken")

	certPEM, keyPEM, caCertPEM, err := r.Join(ctx, "dev-1", "alice", "1.2.3.4", token)
	require.NoError(t, err, "Join")
	assert.NotEmpty(t, certPEM, "certPEM is empty")
	assert.NotEmpty(t, keyPEM, "keyPEM is empty")
	assert.NotEmpty(t, caCertPEM, "caCertPEM is empty")

	// Verify the device was created in the store.
	d, err := r.store.GetDevice(ctx, "dev-1")
	require.NoError(t, err, "GetDevice")
	assert.Equal(t, "alice", d.Nickname)
	assert.Equal(t, store.StatusRegistered, d.Status)
	assert.Equal(t, "1.2.3.4", d.LastIP)
}

func TestJoin_DuplicateNickname(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	token1, _, err := r.IssueJoinToken(ctx)
	require.NoError(t, err, "IssueJoinToken 1")
	_, _, _, err = r.Join(ctx, "dev-1", "alice", "", token1)
	require.NoError(t, err, "first Join")

	token2, _, err := r.IssueJoinToken(ctx)
	require.NoError(t, err, "IssueJoinToken 2")
	_, _, _, err = r.Join(ctx, "dev-2", "alice", "", token2)
	require.Error(t, err, "expected error for duplicate nickname")
	assert.Equal(t, common.ErrNicknameTaken, err)
}

func TestJoin_DuplicateDeviceID(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	token1, _, err := r.IssueJoinToken(ctx)
	require.NoError(t, err, "IssueJoinToken 1")
	_, _, _, err = r.Join(ctx, "dev-1", "alice", "", token1)
	require.NoError(t, err, "first Join")

	// Same device_id, different nickname — store should reject the duplicate.
	token2, _, err := r.IssueJoinToken(ctx)
	require.NoError(t, err, "IssueJoinToken 2")
	_, _, _, err = r.Join(ctx, "dev-1", "bob", "", token2)
	require.Error(t, err, "expected error for duplicate device_id")
}

// --- Register ---

func TestRegister_ReturnsOnlineDevices(t *testing.T) {
	r := newTestRegistry(t)

	joinDevice(t, r, "dev-1", "alice", "")
	joinDevice(t, r, "dev-2", "bob", "")

	online := registerDevice(t, r, "dev-1", "10.0.0.1", 22)
	require.Len(t, online, 1, "online count after first register")
	assert.Equal(t, "dev-1", online[0].DeviceID)

	// Register second device; both should now be in the online list.
	online = registerDevice(t, r, "dev-2", "10.0.0.2", 22)
	assert.Len(t, online, 2, "online count after second register")
}

func TestRegister_SetsHeartbeat(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	joinDevice(t, r, "dev-1", "alice", "")

	before := time.Now()
	_, err := r.Register(ctx, "dev-1", "10.0.0.1", 22, nil, common.ProtocolVersion)
	require.NoError(t, err, "Register")

	d, err := r.store.GetDevice(ctx, "dev-1")
	require.NoError(t, err, "GetDevice")
	assert.False(t, d.LastHeartbeat.IsZero() || d.LastHeartbeat.Before(before),
		"LastHeartbeat not set on register, got %v", d.LastHeartbeat)
	assert.Equal(t, store.StatusOnline, d.Status)
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
		assert.NotNil(t, event.GetDeviceOnline(), "expected DeviceOnline event, got %T", event.GetPayload())
		if event.GetDeviceOnline() != nil {
			assert.Equal(t, "dev-1", event.GetDeviceOnline().DeviceId)
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
	_, err := r.Register(ctx, "dev-1", "10.0.0.1", 22, shares, common.ProtocolVersion)
	require.NoError(t, err, "Register with shares")

	stored, err := r.store.GetShares(ctx, "dev-1")
	require.NoError(t, err, "GetShares")
	require.Len(t, stored, 1, "stored shares count")
	assert.Equal(t, "docs", stored[0].Alias)
}

// --- Heartbeat ---

func TestHeartbeat_UpdatesTimestamp(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	joinDevice(t, r, "dev-1", "alice", "")

	before := time.Now()
	err := r.Heartbeat(ctx, "dev-1", "10.0.0.1")
	require.NoError(t, err, "Heartbeat")
	after := time.Now()

	d, err := r.store.GetDevice(ctx, "dev-1")
	require.NoError(t, err, "GetDevice")
	assert.False(t, d.LastHeartbeat.Before(before) || d.LastHeartbeat.After(after),
		"LastHeartbeat %v not in expected range [%v, %v]", d.LastHeartbeat, before, after)
}

// TestHeartbeat_OnlineDeviceBroadcastsNothing — the ordinary case must stay
// silent: a heartbeat from a device the hub already considers online is a
// timestamp update and nothing else.
func TestHeartbeat_OnlineDeviceBroadcastsNothing(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	joinDevice(t, r, "dev-1", "alice", "")
	joinDevice(t, r, "dev-2", "bob", "")
	registerDevice(t, r, "dev-1", "10.0.0.1", 2222)

	ch, unsub := r.Subscribe("dev-2")
	defer unsub()

	require.NoError(t, r.Heartbeat(ctx, "dev-1", "10.0.0.1"), "Heartbeat")

	select {
	case event := <-ch:
		t.Fatalf("an online device's heartbeat must not broadcast anything, got %T", event.GetPayload())
	case <-time.After(200 * time.Millisecond):
	}
}

// TestHeartbeat_RecoversOfflineDevice is the core of issue #69: the hub demoted
// a device that was in fact alive (its heartbeats were merely late), every peer
// unmounted its shares, and nothing but a full session re-establishment could
// undo that. A heartbeat is proof of life, so it must restore the device and
// tell the peers.
func TestHeartbeat_RecoversOfflineDevice(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	joinDevice(t, r, "dev-1", "alice", "")
	joinDevice(t, r, "dev-2", "bob", "")
	registerDevice(t, r, "dev-1", "10.0.0.1", 2222)
	require.NoError(t, r.UpdateShares(ctx, "dev-1", []*pb.Share{{Alias: "docs", Permissions: "ro"}}), "UpdateShares")

	// The monitor gave up on dev-1 while the device kept running.
	d1, err := r.store.GetDevice(ctx, "dev-1")
	require.NoError(t, err, "GetDevice")
	demoted, err := r.MarkOffline(ctx, d1, time.Now().Add(time.Minute))
	require.NoError(t, err, "MarkOffline")
	require.True(t, demoted, "MarkOffline should demote a device past the threshold")

	ch, unsub := r.Subscribe("dev-2")
	defer unsub()
	// dev-1's own subscription stands in for its live daemon: an open stream is
	// how the hub tells "demoted while connected" from "deregistered on
	// purpose" (Registry.canRecover). It doubles as the assertion below that
	// the recovered device is not sent its own event.
	selfCh, selfUnsub := r.Subscribe("dev-1")
	defer selfUnsub()

	require.NoError(t, r.Heartbeat(ctx, "dev-1", "10.0.0.7"), "Heartbeat")

	d, err := r.store.GetDevice(ctx, "dev-1")
	require.NoError(t, err, "GetDevice")
	assert.Equal(t, store.StatusOnline, d.Status, "a heartbeat must bring an offline device back")
	assert.Equal(t, "10.0.0.7", d.LastIP, "the caller's current address must be recorded")

	select {
	case event := <-ch:
		online := event.GetDeviceOnline()
		require.NotNil(t, online, "expected DeviceOnline, got %T", event.GetPayload())
		assert.Equal(t, "dev-1", online.DeviceId)
		assert.Equal(t, "alice", online.Nickname)
		assert.Equal(t, "10.0.0.7", online.Ip, "peers must be told the address the device is reachable at now")
		assert.Equal(t, int32(2222), online.SshPort, "the stored SSH port must be re-announced")
		require.Len(t, online.Shares, 1, "the device's shares must be re-announced so peers can remount")
		assert.Equal(t, "docs", online.Shares[0].Alias)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the recovery DeviceOnline event")
	}

	select {
	case event := <-selfCh:
		t.Fatalf("the recovered device must not receive its own DeviceOnline, got %T", event.GetPayload())
	case <-time.After(200 * time.Millisecond):
	}

	// A second heartbeat changes nothing — the device is online already.
	require.NoError(t, r.Heartbeat(ctx, "dev-1", "10.0.0.7"), "second Heartbeat")
	select {
	case event := <-ch:
		t.Fatalf("recovery must be announced once, got a second %T", event.GetPayload())
	case <-time.After(200 * time.Millisecond):
	}
}

// TestHeartbeat_DoesNotResurrectDeregisteredDevice — being offline is not by
// itself proof that the hub gave up on a live device. A device that shut down
// cleanly is offline too, and a heartbeat still in flight when its Deregister
// lands must not bring it back: peers would be told to mount a daemon that has
// already exited.
func TestHeartbeat_DoesNotResurrectDeregisteredDevice(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	joinDevice(t, r, "dev-1", "alice", "")
	joinDevice(t, r, "dev-2", "bob", "")
	registerDevice(t, r, "dev-1", "10.0.0.1", 2222)

	_, unsub := r.Subscribe("dev-1") // a connected daemon, as after a real Register
	defer unsub()

	watchCh, watchUnsub := r.Subscribe("dev-2")
	defer watchUnsub()

	require.NoError(t, r.Deregister(ctx, "dev-1"), "Deregister")
	select {
	case event := <-watchCh:
		require.NotNil(t, event.GetDeviceOffline(), "expected DeviceOffline, got %T", event.GetPayload())
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for DeviceOffline")
	}

	// The late heartbeat the departing daemon had already sent.
	require.NoError(t, r.Heartbeat(ctx, "dev-1", "10.0.0.1"), "late Heartbeat")

	d, err := r.store.GetDevice(ctx, "dev-1")
	require.NoError(t, err, "GetDevice")
	assert.Equal(t, store.StatusOffline, d.Status, "a deregistered device must stay offline")

	select {
	case event := <-watchCh:
		t.Fatalf("a deregistered device must not be announced online again, got %T", event.GetPayload())
	case <-time.After(200 * time.Millisecond):
	}
}

// TestHeartbeat_DrainingHubDoesNotRecover — hub shutdown marks every device
// offline while the gRPC server still answers. A heartbeat inside that window
// would otherwise leave a phantom-online row behind for the next hub start.
func TestHeartbeat_DrainingHubDoesNotRecover(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	joinDevice(t, r, "dev-1", "alice", "")
	registerDevice(t, r, "dev-1", "10.0.0.1", 2222)
	_, unsub := r.Subscribe("dev-1")
	defer unsub()

	d1, err := r.store.GetDevice(ctx, "dev-1")
	require.NoError(t, err, "GetDevice")
	demoted, err := r.MarkOffline(ctx, d1, time.Now().Add(time.Minute))
	require.NoError(t, err, "MarkOffline")
	require.True(t, demoted)

	r.Drain()

	require.NoError(t, r.Heartbeat(ctx, "dev-1", "10.0.0.1"), "Heartbeat while draining")

	d, err := r.store.GetDevice(ctx, "dev-1")
	require.NoError(t, err, "GetDevice")
	assert.Equal(t, store.StatusOffline, d.Status, "a draining hub must not bring devices back online")
}

// TestHeartbeat_OfflineDeviceWithoutSSHPortIsNotRecovered — a device that never
// completed a Register has no endpoint the hub could announce; recovering it
// would broadcast a mount target of port 0.
func TestHeartbeat_OfflineDeviceWithoutSSHPortIsNotRecovered(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	joinDevice(t, r, "dev-1", "alice", "")
	joinDevice(t, r, "dev-2", "bob", "")
	require.NoError(t, r.store.UpdateDeviceStatus(ctx, "dev-1", store.StatusOffline, "10.0.0.1", 0), "UpdateDeviceStatus")

	_, selfUnsub := r.Subscribe("dev-1") // connected, so only the missing port can block recovery
	defer selfUnsub()

	ch, unsub := r.Subscribe("dev-2")
	defer unsub()

	require.NoError(t, r.Heartbeat(ctx, "dev-1", "10.0.0.1"), "Heartbeat")

	d, err := r.store.GetDevice(ctx, "dev-1")
	require.NoError(t, err, "GetDevice")
	assert.Equal(t, store.StatusOffline, d.Status, "a device with no known SSH port must stay offline")

	select {
	case event := <-ch:
		t.Fatalf("nothing should be broadcast for a device with no endpoint, got %T", event.GetPayload())
	case <-time.After(200 * time.Millisecond):
	}
}

// TestHeartbeat_UnknownDevice — the pruned identity from issue #69. The hub must
// say so instead of silently accepting the beat.
func TestHeartbeat_UnknownDevice(t *testing.T) {
	r := newTestRegistry(t)

	err := r.Heartbeat(context.Background(), "never-joined", "10.0.0.1")
	require.Error(t, err, "heartbeat from an unknown device must fail")
	assert.ErrorIs(t, err, common.ErrDeviceNotFound)
}

// TestRegister_UnknownDevice — the same for registration, which is what a
// restarted pruned daemon hits first.
func TestRegister_UnknownDevice(t *testing.T) {
	r := newTestRegistry(t)

	_, err := r.Register(context.Background(), "never-joined", "10.0.0.1", 2222, nil, common.ProtocolVersion)
	require.Error(t, err, "registration of an unknown device must fail")
	assert.ErrorIs(t, err, common.ErrDeviceNotFound)
}

// TestMarkOffline_FreshHeartbeatKeepsDeviceOnline pins the registry half of the
// sweep race: the monitor selected this device as stale, it heartbeated before
// the demotion was written, and peers must NOT be told it went offline.
func TestMarkOffline_FreshHeartbeatKeepsDeviceOnline(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	joinDevice(t, r, "dev-1", "alice", "")
	joinDevice(t, r, "dev-2", "bob", "")
	registerDevice(t, r, "dev-1", "10.0.0.1", 2222)

	d1, err := r.store.GetDevice(ctx, "dev-1")
	require.NoError(t, err, "GetDevice")

	// The sweep computed its threshold, then the device heartbeated.
	threshold := time.Now().Add(-time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	require.NoError(t, r.Heartbeat(ctx, "dev-1", "10.0.0.1"), "Heartbeat")

	ch, unsub := r.Subscribe("dev-2")
	defer unsub()

	demoted, err := r.MarkOffline(ctx, d1, threshold)
	require.NoError(t, err, "MarkOffline")
	assert.False(t, demoted, "a device that heartbeated during the sweep must stay online")

	d, err := r.store.GetDevice(ctx, "dev-1")
	require.NoError(t, err, "GetDevice")
	assert.Equal(t, store.StatusOnline, d.Status)

	select {
	case event := <-ch:
		t.Fatalf("no DeviceOffline may be broadcast for a device that stayed online, got %T", event.GetPayload())
	case <-time.After(200 * time.Millisecond):
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
		assert.Equal(t, "dev-2", got.GetDeviceOnline().DeviceId)
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

	err := r.Deregister(ctx, "dev-1")
	require.NoError(t, err, "Deregister")

	// dev-1 must be offline in the store.
	d, err := r.store.GetDevice(ctx, "dev-1")
	require.NoError(t, err, "GetDevice")
	assert.Equal(t, store.StatusOffline, d.Status)

	// dev-2 should receive a DeviceOffline event.
	select {
	case event := <-ch:
		assert.NotNil(t, event.GetDeviceOffline(), "expected DeviceOffline, got %T", event.GetPayload())
		if event.GetDeviceOffline() != nil {
			assert.Equal(t, "dev-1", event.GetDeviceOffline().DeviceId)
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

	err := r.Deregister(ctx, "dev-1")
	require.NoError(t, err, "Deregister")

	r.mu.RLock()
	_, exists := r.subscribers["dev-1"]
	r.mu.RUnlock()

	assert.False(t, exists, "subscriber still present after Deregister")

	// Reading from a closed channel must return immediately with the zero value.
	select {
	case _, ok := <-ch:
		assert.False(t, ok, "channel is not closed after Deregister")
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
	err := r.UpdateShares(ctx, "dev-1", shares)
	require.NoError(t, err, "UpdateShares")

	// Verify stored.
	stored, err := r.store.GetShares(ctx, "dev-1")
	require.NoError(t, err, "GetShares")
	require.Len(t, stored, 1, "stored shares count")
	assert.Equal(t, "music", stored[0].Alias)

	// Verify broadcast.
	select {
	case event := <-ch:
		assert.NotNil(t, event.GetSharesUpdated(), "expected SharesUpdated, got %T", event.GetPayload())
		if event.GetSharesUpdated() != nil {
			assert.Equal(t, "dev-1", event.GetSharesUpdated().DeviceId)
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

	err := r.Rename(ctx, "dev-1", "alice2")
	require.NoError(t, err, "Rename")

	d, err := r.store.GetDevice(ctx, "dev-1")
	require.NoError(t, err, "GetDevice")
	assert.Equal(t, "alice2", d.Nickname)
}

func TestRename_DuplicateNickname(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	joinDevice(t, r, "dev-1", "alice", "")
	joinDevice(t, r, "dev-2", "bob", "")

	err := r.Rename(ctx, "dev-1", "bob")
	require.Error(t, err, "expected error for duplicate nickname")
	assert.Equal(t, common.ErrNicknameTaken, err)
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
	require.NoError(t, err, "GetDevice before MarkOffline")
	demoted, err := r.MarkOffline(ctx, d1, time.Now().Add(time.Minute))
	require.NoError(t, err, "MarkOffline")
	require.True(t, demoted, "an online device past the threshold must be demoted")

	d, err := r.store.GetDevice(ctx, "dev-1")
	require.NoError(t, err, "GetDevice")
	assert.Equal(t, store.StatusOffline, d.Status)

	select {
	case event := <-ch:
		assert.NotNil(t, event.GetDeviceOffline(), "expected DeviceOffline, got %T", event.GetPayload())
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for DeviceOffline event")
	}
}
