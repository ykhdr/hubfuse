package hub

import (
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"time"

	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/hub/store"
	pb "github.com/ykhdr/hubfuse/proto"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	inviteCodeTTL      = 10 * time.Minute
	maxPairingAttempts = 5

	// inviteAlphabet is the set of characters used in generated invite codes.
	inviteAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
)

// RequestPairing initiates a pairing request from fromDevice to toDevice.
// toDevice is a human-readable nickname. Both devices must exist and be online.
// Returns common.ErrPairingAlreadyExists if they are already paired. On success
// it sends a PairingRequested event to the target device and returns the
// generated invite code.
func (r *Registry) RequestPairing(ctx context.Context, fromDevice, toDevice, publicKey string) (string, error) {
	from, err := r.store.GetDevice(ctx, fromDevice)
	if err != nil {
		return "", common.ErrDeviceNotFound
	}

	to, err := r.store.GetDeviceByNickname(ctx, toDevice)
	if err != nil {
		return "", status.Errorf(codes.NotFound, "no device with nickname %q", toDevice)
	}

	if from.Status != store.StatusOnline {
		return "", common.ErrDeviceOffline
	}
	if to.Status != store.StatusOnline {
		return "", common.ErrDeviceOffline
	}

	paired, err := r.store.IsPaired(ctx, fromDevice, to.DeviceID)
	if err != nil {
		return "", err
	}
	if paired {
		return "", common.ErrPairingAlreadyExists
	}

	code := GenerateInviteCode()

	inv := &store.PendingInvite{
		InviteCode:    code,
		FromDevice:    fromDevice,
		ToDevice:      to.DeviceID,
		FromPublicKey: publicKey,
		ExpiresAt:     time.Now().Add(inviteCodeTTL),
		Attempts:      0,
	}
	if err := r.store.CreateInvite(ctx, inv); err != nil {
		return "", err
	}

	event := &pb.Event{
		Payload: &pb.Event_PairingRequested{
			PairingRequested: &pb.PairingRequestedEvent{
				FromDeviceId: from.DeviceID,
				FromNickname: from.Nickname,
			},
		},
	}
	r.sendToDevice(to.DeviceID, event)

	return code, nil
}

// ConfirmPairing validates an invite code and completes the pairing. Returns
// the initiator's public key on success.
func (r *Registry) ConfirmPairing(ctx context.Context, deviceID, inviteCode, publicKey string) (peerPublicKey string, err error) {
	inv, err := r.store.GetInvite(ctx, inviteCode)
	if err != nil {
		return "", common.ErrInvalidInviteCode
	}

	if inv.ToDevice != deviceID {
		return "", common.ErrInvalidInviteCode
	}

	if time.Now().After(inv.ExpiresAt) {
		return "", common.ErrInviteExpired
	}

	if inv.Attempts >= maxPairingAttempts {
		return "", common.ErrMaxAttemptsExceeded
	}

	if err := r.store.IncrementInviteAttempts(ctx, inviteCode); err != nil {
		return "", err
	}

	if err := r.store.CreatePairing(ctx, inv.FromDevice, deviceID); err != nil {
		return "", err
	}

	if err := r.store.DeleteInvite(ctx, inviteCode); err != nil {
		return "", err
	}

	event := &pb.Event{
		Payload: &pb.Event_PairingCompleted{
			PairingCompleted: &pb.PairingCompletedEvent{
				PeerDeviceId:  deviceID,
				PeerPublicKey: publicKey,
			},
		},
	}
	r.sendToDevice(inv.FromDevice, event)

	return inv.FromPublicKey, nil
}

// sendToDevice delivers an event to a single subscribed device. If the channel
// is full the send is skipped and a warning is logged.
func (r *Registry) sendToDevice(deviceID string, event *pb.Event) {
	r.mu.RLock()
	ch, ok := r.subscribers[deviceID]
	r.mu.RUnlock()

	if !ok {
		return
	}

	select {
	case ch <- event:
	default:
		r.logger.Warn("event channel full, dropping event",
			slog.String("device_id", deviceID))
	}
}

// GenerateInviteCode generates a random invite code in the format HUB-XXX-YYY
// where each X and Y is drawn from A-Z0-9 using crypto/rand.
func GenerateInviteCode() string {
	const segLen = 3
	seg1 := randomSegment(segLen)
	seg2 := randomSegment(segLen)
	return fmt.Sprintf("HUB-%s-%s", seg1, seg2)
}

// randomSegment generates a random string of n characters from inviteAlphabet
// using rejection sampling to avoid modulo bias. Bytes >= 252 (the largest
// multiple of 36 that fits in a byte is 252 = 36×7) are rejected and redrawn.
func randomSegment(n int) string {
	const alphabetLen = byte(len(inviteAlphabet))
	// 252 is the largest multiple of 36 that is <= 255, so bytes in [0,251]
	// map uniformly across the 36-character alphabet without bias.
	const maxUnbiased = 252
	buf := make([]byte, n)
	filled := 0
	for filled < n {
		var b [1]byte
		if _, err := rand.Read(b[:]); err != nil {
			panic(fmt.Sprintf("crypto/rand read: %v", err))
		}
		if b[0] >= maxUnbiased {
			continue
		}
		buf[filled] = inviteAlphabet[b[0]%alphabetLen]
		filled++
	}
	return string(buf)
}
