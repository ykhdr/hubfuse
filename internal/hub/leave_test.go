package hub

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRegistry_Leave_RemovesDeviceAndBroadcasts verifies that calling Leave
// deletes the device from the store, closes the caller's subscriber channel,
// and delivers a DeviceRemoved event to other subscribers.
func TestRegistry_Leave_RemovesDeviceAndBroadcasts(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	// Seed two devices.
	joinDevice(t, r, "alice-id", "alice", "10.0.0.1")
	joinDevice(t, r, "bob-id", "bob", "10.0.0.2")

	// Subscribe both devices before calling Leave.
	aliceCh, aliceUnsub := r.Subscribe("alice-id")
	defer aliceUnsub()
	bobCh, bobUnsub := r.Subscribe("bob-id")
	defer bobUnsub()

	// Alice leaves.
	err := r.Leave(ctx, "alice-id")
	require.NoError(t, err, "Leave")

	// Store must no longer contain alice.
	_, err = r.store.GetDevice(ctx, "alice-id")
	assert.Error(t, err, "GetDevice should return an error for the removed device")

	// Bob's channel must receive a DeviceRemoved event for alice.
	select {
	case event := <-bobCh:
		removed := event.GetDeviceRemoved()
		require.NotNil(t, removed, "expected DeviceRemoved event, got %T", event.GetPayload())
		assert.Equal(t, "alice-id", removed.DeviceId)
		assert.Equal(t, "alice", removed.Nickname)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for DeviceRemoved event on bob's channel")
	}

	// Alice's subscriber channel must be closed.
	select {
	case _, ok := <-aliceCh:
		assert.False(t, ok, "alice's channel should be closed after Leave")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for alice's channel to be closed")
	}

	// The subscriber map must not contain alice.
	r.mu.RLock()
	_, exists := r.subscribers["alice-id"]
	r.mu.RUnlock()
	assert.False(t, exists, "alice's subscriber still present after Leave")
}
