package hub

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/hub/store"
)

// setupPairingDevices creates two online devices ready for pairing tests.
func setupPairingDevices(t *testing.T, r *Registry) {
	t.Helper()

	joinDevice(t, r, "dev-a", "alice")
	joinDevice(t, r, "dev-b", "bob")
	registerDevice(t, r, "dev-a", "10.0.0.1", 22)
	registerDevice(t, r, "dev-b", "10.0.0.2", 22)
}

// --- GenerateInviteCode ---

func TestGenerateInviteCode_Format(t *testing.T) {
	code := GenerateInviteCode()
	assert.True(t, strings.HasPrefix(code, "HUB-"), "code %q does not start with HUB-", code)
	parts := strings.Split(code, "-")
	require.Len(t, parts, 3, "code %q has wrong number of parts", code)
	assert.Len(t, parts[1], 3, "segment 1 length wrong")
	assert.Len(t, parts[2], 3, "segment 2 length wrong")
	// Each character must be in the allowed alphabet.
	for _, part := range parts[1:] {
		for _, ch := range part {
			assert.True(t, strings.ContainsRune(inviteAlphabet, ch), "character %q not in alphabet", ch)
		}
	}
}

func TestGenerateInviteCode_Unique(t *testing.T) {
	seen := make(map[string]struct{}, 100)
	for i := 0; i < 100; i++ {
		code := GenerateInviteCode()
		_, dup := seen[code]
		assert.False(t, dup, "duplicate invite code generated: %q", code)
		seen[code] = struct{}{}
	}
}

// --- RequestPairing ---

func TestRequestPairing_Success(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	setupPairingDevices(t, r)

	code, err := r.RequestPairing(ctx, "dev-a", "bob", "pk-alice")
	require.NoError(t, err, "RequestPairing")
	assert.True(t, strings.HasPrefix(code, "HUB-"), "code %q does not start with HUB-", code)
}

func TestRequestPairing_SendsEventToTarget(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	setupPairingDevices(t, r)

	ch, unsub := r.Subscribe("dev-b")
	defer unsub()

	_, err := r.RequestPairing(ctx, "dev-a", "bob", "pk-alice")
	require.NoError(t, err, "RequestPairing")

	select {
	case event := <-ch:
		assert.NotNil(t, event.GetPairingRequested(), "expected PairingRequested, got %T", event.GetPayload())
		if event.GetPairingRequested() != nil {
			assert.Equal(t, "dev-a", event.GetPairingRequested().FromDeviceId)
			assert.Equal(t, "alice", event.GetPairingRequested().FromNickname)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for PairingRequested event")
	}
}

func TestRequestPairing_UnknownFromDevice(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	setupPairingDevices(t, r)

	_, err := r.RequestPairing(ctx, "no-such-device", "dev-b", "pk")
	assert.Error(t, err, "expected error for unknown from device")
}

func TestRequestPairing_UnknownToDevice(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	setupPairingDevices(t, r)

	_, err := r.RequestPairing(ctx, "dev-a", "no-such-nickname", "pk")
	assert.Error(t, err, "expected error for unknown to device")
}

func TestRequestPairing_AlreadyPaired(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	setupPairingDevices(t, r)

	err := r.store.CreatePairing(ctx, "dev-a", "dev-b")
	require.NoError(t, err, "CreatePairing")

	_, err = r.RequestPairing(ctx, "dev-a", "bob", "pk-alice")
	require.Error(t, err, "expected ErrPairingAlreadyExists")
	assert.Equal(t, common.ErrPairingAlreadyExists, err)
}

func TestRequestPairing_OfflineDevice(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	// Create two devices but only register (online) dev-a.
	joinDevice(t, r, "dev-a", "alice")
	joinDevice(t, r, "dev-b", "bob")
	registerDevice(t, r, "dev-a", "10.0.0.1", 22)
	// dev-b remains offline.

	_, err := r.RequestPairing(ctx, "dev-a", "bob", "pk-alice")
	assert.Error(t, err, "expected error for offline target device")
}

// --- ConfirmPairing ---

func TestConfirmPairing_Success(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	setupPairingDevices(t, r)

	code, err := r.RequestPairing(ctx, "dev-a", "bob", "pk-alice")
	require.NoError(t, err, "RequestPairing")

	peerPK, err := r.ConfirmPairing(ctx, "dev-b", code, "pk-bob")
	require.NoError(t, err, "ConfirmPairing")
	assert.Equal(t, "pk-alice", peerPK)
}

func TestConfirmPairing_SendsCompletedEventToInitiator(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	setupPairingDevices(t, r)

	ch, unsub := r.Subscribe("dev-a")
	defer unsub()

	code, err := r.RequestPairing(ctx, "dev-a", "bob", "pk-alice")
	require.NoError(t, err, "RequestPairing")

	_, err = r.ConfirmPairing(ctx, "dev-b", code, "pk-bob")
	require.NoError(t, err, "ConfirmPairing")

	select {
	case event := <-ch:
		assert.NotNil(t, event.GetPairingCompleted(), "expected PairingCompleted, got %T", event.GetPayload())
		if event.GetPairingCompleted() != nil {
			assert.Equal(t, "dev-b", event.GetPairingCompleted().PeerDeviceId)
			assert.Equal(t, "pk-bob", event.GetPairingCompleted().PeerPublicKey)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for PairingCompleted event")
	}
}

func TestConfirmPairing_WrongCode(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	setupPairingDevices(t, r)

	_, err := r.RequestPairing(ctx, "dev-a", "bob", "pk-alice")
	require.NoError(t, err, "RequestPairing")

	_, err = r.ConfirmPairing(ctx, "dev-b", "HUB-WRONG-CODE", "pk-bob")
	require.Error(t, err, "expected error for wrong code")
	assert.Equal(t, common.ErrInvalidInviteCode, err)
}

func TestConfirmPairing_MaxAttempts(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	setupPairingDevices(t, r)

	code, err := r.RequestPairing(ctx, "dev-a", "bob", "pk-alice")
	require.NoError(t, err, "RequestPairing")

	// Exhaust attempts by bumping the counter directly to maxPairingAttempts.
	for i := 0; i < maxPairingAttempts; i++ {
		err := r.store.IncrementInviteAttempts(ctx, code)
		require.NoError(t, err, "IncrementInviteAttempts")
	}

	_, err = r.ConfirmPairing(ctx, "dev-b", code, "pk-bob")
	require.Error(t, err, "expected ErrMaxAttemptsExceeded")
	assert.Equal(t, common.ErrMaxAttemptsExceeded, err)
}

func TestConfirmPairing_Expired(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	setupPairingDevices(t, r)

	// Create an already-expired invite directly in the store.
	inv := &store.PendingInvite{
		InviteCode:    "HUB-EXP-IRD",
		FromDevice:    "dev-a",
		ToDevice:      "dev-b",
		FromPublicKey: "pk-alice",
		ExpiresAt:     time.Now().Add(-time.Minute), // already expired
		Attempts:      0,
	}
	err := r.store.CreateInvite(ctx, inv)
	require.NoError(t, err, "CreateInvite")

	_, err = r.ConfirmPairing(ctx, "dev-b", "HUB-EXP-IRD", "pk-bob")
	require.Error(t, err, "expected ErrInviteExpired")
	assert.Equal(t, common.ErrInviteExpired, err)
}

func TestConfirmPairing_WrongTargetDevice(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	setupPairingDevices(t, r)

	code, err := r.RequestPairing(ctx, "dev-a", "bob", "pk-alice")
	require.NoError(t, err, "RequestPairing")

	// dev-a tries to confirm, but the invite targets dev-b.
	_, err = r.ConfirmPairing(ctx, "dev-a", code, "pk-alice")
	require.Error(t, err, "expected error for wrong target device")
	assert.Equal(t, common.ErrInvalidInviteCode, err)
}

func TestConfirmPairing_InviteDeletedAfterSuccess(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	setupPairingDevices(t, r)

	code, err := r.RequestPairing(ctx, "dev-a", "bob", "pk-alice")
	require.NoError(t, err, "RequestPairing")

	_, err = r.ConfirmPairing(ctx, "dev-b", code, "pk-bob")
	require.NoError(t, err, "ConfirmPairing")

	// The invite must no longer exist in the store.
	_, err = r.store.GetInvite(ctx, code)
	assert.Error(t, err, "expected invite to be deleted after ConfirmPairing")
}

func TestConfirmPairing_PairingRecordCreated(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	setupPairingDevices(t, r)

	code, err := r.RequestPairing(ctx, "dev-a", "bob", "pk-alice")
	require.NoError(t, err, "RequestPairing")

	_, err = r.ConfirmPairing(ctx, "dev-b", code, "pk-bob")
	require.NoError(t, err, "ConfirmPairing")

	paired, err := r.store.IsPaired(ctx, "dev-a", "dev-b")
	require.NoError(t, err, "IsPaired")
	assert.True(t, paired, "IsPaired = false after ConfirmPairing, want true")
}
