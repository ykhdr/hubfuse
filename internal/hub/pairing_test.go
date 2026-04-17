package hub

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/hub/store"
)

// setupPairingDevices creates two online devices ready for pairing tests.
func setupPairingDevices(t *testing.T, r *Registry) {
	t.Helper()

	joinDevice(t, r, "dev-a", "alice", "")
	joinDevice(t, r, "dev-b", "bob", "")
	registerDevice(t, r, "dev-a", "10.0.0.1", 22)
	registerDevice(t, r, "dev-b", "10.0.0.2", 22)
}

// --- GenerateInviteCode ---

func TestGenerateInviteCode_Format(t *testing.T) {
	code := GenerateInviteCode()
	if !strings.HasPrefix(code, "HUB-") {
		t.Errorf("code %q does not start with HUB-", code)
	}
	parts := strings.Split(code, "-")
	if len(parts) != 3 {
		t.Fatalf("code %q has %d parts, want 3", code, len(parts))
	}
	if len(parts[1]) != 3 {
		t.Errorf("segment 1 length = %d, want 3", len(parts[1]))
	}
	if len(parts[2]) != 3 {
		t.Errorf("segment 2 length = %d, want 3", len(parts[2]))
	}
	// Each character must be in the allowed alphabet.
	for _, part := range parts[1:] {
		for _, ch := range part {
			if !strings.ContainsRune(inviteAlphabet, ch) {
				t.Errorf("character %q not in alphabet", ch)
			}
		}
	}
}

func TestGenerateInviteCode_Unique(t *testing.T) {
	seen := make(map[string]struct{}, 100)
	for i := 0; i < 100; i++ {
		code := GenerateInviteCode()
		if _, dup := seen[code]; dup {
			t.Errorf("duplicate invite code generated: %q", code)
		}
		seen[code] = struct{}{}
	}
}

// --- RequestPairing ---

func TestRequestPairing_Success(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	setupPairingDevices(t, r)

	code, err := r.RequestPairing(ctx, "dev-a", "bob", "pk-alice")
	if err != nil {
		t.Fatalf("RequestPairing: %v", err)
	}
	if !strings.HasPrefix(code, "HUB-") {
		t.Errorf("code %q does not start with HUB-", code)
	}
}

func TestRequestPairing_SendsEventToTarget(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	setupPairingDevices(t, r)

	ch, unsub := r.Subscribe("dev-b")
	defer unsub()

	if _, err := r.RequestPairing(ctx, "dev-a", "bob", "pk-alice"); err != nil {
		t.Fatalf("RequestPairing: %v", err)
	}

	select {
	case event := <-ch:
		if event.GetPairingRequested() == nil {
			t.Errorf("expected PairingRequested, got %T", event.GetPayload())
		}
		if event.GetPairingRequested().FromDeviceId != "dev-a" {
			t.Errorf("FromDeviceId = %q, want dev-a", event.GetPairingRequested().FromDeviceId)
		}
		if event.GetPairingRequested().FromNickname != "alice" {
			t.Errorf("FromNickname = %q, want alice", event.GetPairingRequested().FromNickname)
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
	if err == nil {
		t.Fatal("expected error for unknown from device, got nil")
	}
}

func TestRequestPairing_UnknownToDevice(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	setupPairingDevices(t, r)

	_, err := r.RequestPairing(ctx, "dev-a", "no-such-nickname", "pk")
	if err == nil {
		t.Fatal("expected error for unknown to device, got nil")
	}
}

func TestRequestPairing_AlreadyPaired(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	setupPairingDevices(t, r)

	if err := r.store.CreatePairing(ctx, "dev-a", "dev-b"); err != nil {
		t.Fatalf("CreatePairing: %v", err)
	}

	_, err := r.RequestPairing(ctx, "dev-a", "bob", "pk-alice")
	if err == nil {
		t.Fatal("expected ErrPairingAlreadyExists, got nil")
	}
	if err != common.ErrPairingAlreadyExists {
		t.Errorf("error = %v, want ErrPairingAlreadyExists", err)
	}
}

func TestRequestPairing_OfflineDevice(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	// Create two devices but only register (online) dev-a.
	joinDevice(t, r, "dev-a", "alice", "")
	joinDevice(t, r, "dev-b", "bob", "")
	registerDevice(t, r, "dev-a", "10.0.0.1", 22)
	// dev-b remains offline.

	_, err := r.RequestPairing(ctx, "dev-a", "bob", "pk-alice")
	if err == nil {
		t.Fatal("expected error for offline target device, got nil")
	}
	if err != common.ErrDeviceOffline {
		t.Fatalf("error = %v, want ErrDeviceOffline", err)
	}
}

func TestRequestPairing_RegisteredDevice(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()

	joinDevice(t, r, "dev-a", "alice", "")
	joinDevice(t, r, "dev-b", "bob", "")
	registerDevice(t, r, "dev-a", "10.0.0.1", 22)

	_, err := r.RequestPairing(ctx, "dev-a", "bob", "pk-alice")
	if err == nil {
		t.Fatal("expected error for registered target device, got nil")
	}
	if err != common.ErrDeviceOffline {
		t.Fatalf("error = %v, want ErrDeviceOffline", err)
	}
}

// --- ConfirmPairing ---

func TestConfirmPairing_Success(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	setupPairingDevices(t, r)

	code, err := r.RequestPairing(ctx, "dev-a", "bob", "pk-alice")
	if err != nil {
		t.Fatalf("RequestPairing: %v", err)
	}

	peerPK, err := r.ConfirmPairing(ctx, "dev-b", code, "pk-bob")
	if err != nil {
		t.Fatalf("ConfirmPairing: %v", err)
	}
	if peerPK != "pk-alice" {
		t.Errorf("peerPublicKey = %q, want pk-alice", peerPK)
	}
}

func TestConfirmPairing_SendsCompletedEventToInitiator(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	setupPairingDevices(t, r)

	ch, unsub := r.Subscribe("dev-a")
	defer unsub()

	code, err := r.RequestPairing(ctx, "dev-a", "bob", "pk-alice")
	if err != nil {
		t.Fatalf("RequestPairing: %v", err)
	}

	if _, err := r.ConfirmPairing(ctx, "dev-b", code, "pk-bob"); err != nil {
		t.Fatalf("ConfirmPairing: %v", err)
	}

	select {
	case event := <-ch:
		if event.GetPairingCompleted() == nil {
			t.Errorf("expected PairingCompleted, got %T", event.GetPayload())
		}
		if event.GetPairingCompleted().PeerDeviceId != "dev-b" {
			t.Errorf("PeerDeviceId = %q, want dev-b", event.GetPairingCompleted().PeerDeviceId)
		}
		if event.GetPairingCompleted().PeerPublicKey != "pk-bob" {
			t.Errorf("PeerPublicKey = %q, want pk-bob", event.GetPairingCompleted().PeerPublicKey)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for PairingCompleted event")
	}
}

func TestConfirmPairing_WrongCode(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	setupPairingDevices(t, r)

	if _, err := r.RequestPairing(ctx, "dev-a", "bob", "pk-alice"); err != nil {
		t.Fatalf("RequestPairing: %v", err)
	}

	_, err := r.ConfirmPairing(ctx, "dev-b", "HUB-WRONG-CODE", "pk-bob")
	if err == nil {
		t.Fatal("expected error for wrong code, got nil")
	}
	if err != common.ErrInvalidInviteCode {
		t.Errorf("error = %v, want ErrInvalidInviteCode", err)
	}
}

func TestConfirmPairing_MaxAttempts(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	setupPairingDevices(t, r)

	code, err := r.RequestPairing(ctx, "dev-a", "bob", "pk-alice")
	if err != nil {
		t.Fatalf("RequestPairing: %v", err)
	}

	// Exhaust attempts by bumping the counter directly to maxPairingAttempts.
	for i := 0; i < maxPairingAttempts; i++ {
		if err := r.store.IncrementInviteAttempts(ctx, code); err != nil {
			t.Fatalf("IncrementInviteAttempts: %v", err)
		}
	}

	_, err = r.ConfirmPairing(ctx, "dev-b", code, "pk-bob")
	if err == nil {
		t.Fatal("expected ErrMaxAttemptsExceeded, got nil")
	}
	if err != common.ErrMaxAttemptsExceeded {
		t.Errorf("error = %v, want ErrMaxAttemptsExceeded", err)
	}
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
	if err := r.store.CreateInvite(ctx, inv); err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}

	_, err := r.ConfirmPairing(ctx, "dev-b", "HUB-EXP-IRD", "pk-bob")
	if err == nil {
		t.Fatal("expected ErrInviteExpired, got nil")
	}
	if err != common.ErrInviteExpired {
		t.Errorf("error = %v, want ErrInviteExpired", err)
	}
}

func TestConfirmPairing_WrongTargetDevice(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	setupPairingDevices(t, r)

	code, err := r.RequestPairing(ctx, "dev-a", "bob", "pk-alice")
	if err != nil {
		t.Fatalf("RequestPairing: %v", err)
	}

	// dev-a tries to confirm, but the invite targets dev-b.
	_, err = r.ConfirmPairing(ctx, "dev-a", code, "pk-alice")
	if err == nil {
		t.Fatal("expected error for wrong target device, got nil")
	}
	if err != common.ErrInvalidInviteCode {
		t.Errorf("error = %v, want ErrInvalidInviteCode", err)
	}
}

func TestConfirmPairing_InviteDeletedAfterSuccess(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	setupPairingDevices(t, r)

	code, err := r.RequestPairing(ctx, "dev-a", "bob", "pk-alice")
	if err != nil {
		t.Fatalf("RequestPairing: %v", err)
	}

	if _, err := r.ConfirmPairing(ctx, "dev-b", code, "pk-bob"); err != nil {
		t.Fatalf("ConfirmPairing: %v", err)
	}

	// The invite must no longer exist in the store.
	_, err = r.store.GetInvite(ctx, code)
	if err == nil {
		t.Fatal("expected invite to be deleted after ConfirmPairing, got nil error")
	}
}

func TestConfirmPairing_PairingRecordCreated(t *testing.T) {
	r := newTestRegistry(t)
	ctx := context.Background()
	setupPairingDevices(t, r)

	code, err := r.RequestPairing(ctx, "dev-a", "bob", "pk-alice")
	if err != nil {
		t.Fatalf("RequestPairing: %v", err)
	}

	if _, err := r.ConfirmPairing(ctx, "dev-b", code, "pk-bob"); err != nil {
		t.Fatalf("ConfirmPairing: %v", err)
	}

	paired, err := r.store.IsPaired(ctx, "dev-a", "dev-b")
	if err != nil {
		t.Fatalf("IsPaired: %v", err)
	}
	if !paired {
		t.Error("IsPaired = false after ConfirmPairing, want true")
	}
}
