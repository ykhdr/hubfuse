package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestStore opens an in-memory SQLite store and registers a cleanup that
// closes it when the test ends.
func newTestStore(t *testing.T) Store {
	t.Helper()
	s, err := NewSQLiteStore(":memory:")
	require.NoError(t, err, "NewSQLiteStore(:memory:)")
	t.Cleanup(func() { s.Close() })
	return s
}

func makeDevice(id, nickname string) *Device {
	return &Device{
		DeviceID:      id,
		Nickname:      nickname,
		LastIP:        "192.168.1.1",
		SSHPort:       22,
		Status:        StatusOffline,
		LastHeartbeat: time.Now().UTC().Truncate(time.Second),
	}
}

// --- Device tests ---

func TestCreateDevice(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	d := makeDevice("dev-1", "alice")
	require.NoError(t, s.CreateDevice(ctx, d), "CreateDevice")

	got, err := s.GetDevice(ctx, "dev-1")
	require.NoError(t, err, "GetDevice")
	assert.Equal(t, d.DeviceID, got.DeviceID)
	assert.Equal(t, d.Nickname, got.Nickname)
	assert.Equal(t, d.SSHPort, got.SSHPort)
	assert.Equal(t, d.Status, got.Status)
}

func TestGetDevice_NotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, err := s.GetDevice(ctx, "nonexistent")
	assert.Error(t, err)
}

func TestCreateDevice_DuplicateNickname(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.CreateDevice(ctx, makeDevice("dev-1", "alice")), "CreateDevice first")
	// Second device with same nickname should fail.
	err := s.CreateDevice(ctx, makeDevice("dev-2", "alice"))
	assert.Error(t, err, "expected error for duplicate nickname")
}

func TestCreateDevice_DuplicateID(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.CreateDevice(ctx, makeDevice("dev-1", "alice")), "CreateDevice first")
	err := s.CreateDevice(ctx, makeDevice("dev-1", "bob"))
	assert.Error(t, err, "expected error for duplicate device_id")
}

func TestGetDeviceByNickname(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	d := makeDevice("dev-1", "alice")
	require.NoError(t, s.CreateDevice(ctx, d), "CreateDevice")

	got, err := s.GetDeviceByNickname(ctx, "alice")
	require.NoError(t, err, "GetDeviceByNickname")
	assert.Equal(t, "dev-1", got.DeviceID)
}

func TestGetDeviceByNickname_NotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, err := s.GetDeviceByNickname(ctx, "nobody")
	assert.Error(t, err, "expected error for nonexistent nickname")
}

func TestListOnlineDevices(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	online := makeDevice("dev-1", "alice")
	online.Status = StatusOnline
	offline := makeDevice("dev-2", "bob")
	offline.Status = StatusOffline

	require.NoError(t, s.CreateDevice(ctx, online), "CreateDevice online")
	require.NoError(t, s.CreateDevice(ctx, offline), "CreateDevice offline")

	list, err := s.ListOnlineDevices(ctx)
	require.NoError(t, err, "ListOnlineDevices")
	require.Len(t, list, 1)
	assert.Equal(t, "dev-1", list[0].DeviceID)
}

func TestListOnlineDevices_Empty(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	list, err := s.ListOnlineDevices(ctx)
	require.NoError(t, err, "ListOnlineDevices")
	assert.Empty(t, list)
}

func TestUpdateDeviceStatus(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.CreateDevice(ctx, makeDevice("dev-1", "alice")), "CreateDevice")
	require.NoError(t, s.UpdateDeviceStatus(ctx, "dev-1", StatusOnline, "10.0.0.5", 2222), "UpdateDeviceStatus")

	got, err := s.GetDevice(ctx, "dev-1")
	require.NoError(t, err, "GetDevice")
	assert.Equal(t, StatusOnline, got.Status)
	assert.Equal(t, "10.0.0.5", got.LastIP)
	assert.Equal(t, 2222, got.SSHPort)
}

func TestUpdateDeviceNickname(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.CreateDevice(ctx, makeDevice("dev-1", "alice")), "CreateDevice")
	require.NoError(t, s.UpdateDeviceNickname(ctx, "dev-1", "alicia"), "UpdateDeviceNickname")

	got, err := s.GetDevice(ctx, "dev-1")
	require.NoError(t, err, "GetDevice")
	assert.Equal(t, "alicia", got.Nickname)
}

func TestUpdateHeartbeat(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	d := makeDevice("dev-1", "alice")
	d.LastHeartbeat = time.Now().UTC().Add(-1 * time.Hour)
	require.NoError(t, s.CreateDevice(ctx, d), "CreateDevice")

	before := time.Now().UTC()
	require.NoError(t, s.UpdateHeartbeat(ctx, "dev-1"), "UpdateHeartbeat")
	after := time.Now().UTC()

	got, err := s.GetDevice(ctx, "dev-1")
	require.NoError(t, err, "GetDevice")
	assert.False(t, got.LastHeartbeat.Before(before) || got.LastHeartbeat.After(after),
		"LastHeartbeat %v not in expected range [%v, %v]", got.LastHeartbeat, before, after)
}

func TestGetStaleDevices(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	old := makeDevice("dev-old", "alice")
	old.Status = StatusOnline
	old.LastHeartbeat = time.Now().UTC().Add(-10 * time.Minute)

	fresh := makeDevice("dev-fresh", "bob")
	fresh.Status = StatusOnline
	fresh.LastHeartbeat = time.Now().UTC()

	offlineOld := makeDevice("dev-offline", "charlie")
	offlineOld.Status = StatusOffline
	offlineOld.LastHeartbeat = time.Now().UTC().Add(-10 * time.Minute)

	for _, d := range []*Device{old, fresh, offlineOld} {
		require.NoError(t, s.CreateDevice(ctx, d), "CreateDevice %q", d.DeviceID)
	}

	// Threshold is 5 minutes ago — only dev-old is stale online.
	threshold := time.Now().UTC().Add(-5 * time.Minute)
	stale, err := s.GetStaleDevices(ctx, threshold)
	require.NoError(t, err, "GetStaleDevices")
	require.Len(t, stale, 1)
	assert.Equal(t, "dev-old", stale[0].DeviceID)
}

func TestGetStaleDevices_NoneStale(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	d := makeDevice("dev-1", "alice")
	d.Status = StatusOnline
	d.LastHeartbeat = time.Now().UTC()
	require.NoError(t, s.CreateDevice(ctx, d), "CreateDevice")

	threshold := time.Now().UTC().Add(-5 * time.Minute)
	stale, err := s.GetStaleDevices(ctx, threshold)
	require.NoError(t, err, "GetStaleDevices")
	assert.Empty(t, stale)
}

func TestDeleteDevice(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.CreateDevice(ctx, makeDevice("dev-1", "alice")), "CreateDevice")
	require.NoError(t, s.DeleteDevice(ctx, "dev-1"), "DeleteDevice")

	_, err := s.GetDevice(ctx, "dev-1")
	assert.Error(t, err, "expected error after deletion")
}

func TestDeletePrunedDevices(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	old := makeDevice("dev-old", "oldie")
	old.LastHeartbeat = time.Now().UTC().Add(-48 * time.Hour)

	active := makeDevice("dev-active", "active")
	active.LastHeartbeat = time.Now().UTC().Add(-48 * time.Hour)

	recent := makeDevice("dev-recent", "recent")

	online := makeDevice("dev-online", "online")
	online.Status = StatusOnline
	online.LastHeartbeat = time.Now().UTC().Add(-48 * time.Hour)

	other := makeDevice("dev-other", "other")

	for _, d := range []*Device{old, active, recent, online, other} {
		if err := s.CreateDevice(ctx, d); err != nil {
			t.Fatalf("CreateDevice %q: %v", d.DeviceID, err)
		}
	}

	shares := []*Share{{DeviceID: old.DeviceID, Alias: "docs", Permissions: PermRO}}
	if err := s.SetShares(ctx, old.DeviceID, shares); err != nil {
		t.Fatalf("SetShares: %v", err)
	}

	if err := s.CreatePairing(ctx, old.DeviceID, other.DeviceID); err != nil {
		t.Fatalf("CreatePairing: %v", err)
	}

	invite := &PendingInvite{
		InviteCode:    "INV-1",
		FromDevice:    old.DeviceID,
		ToDevice:      other.DeviceID,
		FromPublicKey: "ssh-ed25519 AAAAC3Nza",
		ExpiresAt:     time.Now().UTC().Add(time.Hour),
	}
	if err := s.CreateInvite(ctx, invite); err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}

	threshold := time.Now().UTC().Add(-24 * time.Hour)
	pruned, err := s.DeletePrunedDevices(ctx, threshold, []string{active.DeviceID})
	if err != nil {
		t.Fatalf("DeletePrunedDevices: %v", err)
	}
	if len(pruned) != 1 || pruned[0].DeviceID != old.DeviceID {
		t.Fatalf("pruned devices = %v, want only %s", pruned, old.DeviceID)
	}

	if _, err := s.GetDevice(ctx, old.DeviceID); err == nil {
		t.Fatal("expected pruned device to be removed")
	}

	if _, err := s.GetDevice(ctx, active.DeviceID); err != nil {
		t.Fatalf("active device should remain: %v", err)
	}
	if _, err := s.GetDevice(ctx, recent.DeviceID); err != nil {
		t.Fatalf("recent device should remain: %v", err)
	}
	if _, err := s.GetDevice(ctx, online.DeviceID); err != nil {
		t.Fatalf("online device should remain: %v", err)
	}

	if paired, err := s.IsPaired(ctx, old.DeviceID, other.DeviceID); err == nil && paired {
		t.Fatal("pairing still exists after pruning old device")
	}

	if shares, err := s.GetShares(ctx, old.DeviceID); err != nil {
		t.Fatalf("GetShares pruned device: %v", err)
	} else if len(shares) != 0 {
		t.Fatalf("expected shares to be removed, got %d", len(shares))
	}

	if _, err := s.GetInvite(ctx, invite.InviteCode); err == nil {
		t.Fatal("expected invite to be removed after pruning device")
	}
}

// --- Share tests ---

func TestSetShares_AndGet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.CreateDevice(ctx, makeDevice("dev-1", "alice")), "CreateDevice")

	shares := []*Share{
		{DeviceID: "dev-1", Alias: "docs", Permissions: PermRO, AllowedDevices: []string{"dev-2", "dev-3"}},
		{DeviceID: "dev-1", Alias: "music", Permissions: PermRW, AllowedDevices: []string{}},
	}
	require.NoError(t, s.SetShares(ctx, "dev-1", shares), "SetShares")

	got, err := s.GetShares(ctx, "dev-1")
	require.NoError(t, err, "GetShares")
	require.Len(t, got, 2)

	// Build a map for order-independent comparison.
	byAlias := make(map[string]*Share, len(got))
	for _, sh := range got {
		byAlias[sh.Alias] = sh
	}

	docs, ok := byAlias["docs"]
	require.True(t, ok, "share 'docs' not found")
	assert.Equal(t, PermRO, docs.Permissions)
	assert.Len(t, docs.AllowedDevices, 2)

	music, ok := byAlias["music"]
	require.True(t, ok, "share 'music' not found")
	assert.Equal(t, PermRW, music.Permissions)
}

func TestSetShares_ReplacesExisting(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.CreateDevice(ctx, makeDevice("dev-1", "alice")), "CreateDevice")

	original := []*Share{
		{DeviceID: "dev-1", Alias: "old-share", Permissions: PermRO, AllowedDevices: []string{}},
	}
	require.NoError(t, s.SetShares(ctx, "dev-1", original), "SetShares original")

	replacement := []*Share{
		{DeviceID: "dev-1", Alias: "new-share", Permissions: PermRW, AllowedDevices: []string{"dev-2"}},
	}
	require.NoError(t, s.SetShares(ctx, "dev-1", replacement), "SetShares replacement")

	got, err := s.GetShares(ctx, "dev-1")
	require.NoError(t, err, "GetShares")
	require.Len(t, got, 1)
	assert.Equal(t, "new-share", got[0].Alias)
}

func TestSetShares_Empty(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.CreateDevice(ctx, makeDevice("dev-1", "alice")), "CreateDevice")
	require.NoError(t, s.SetShares(ctx, "dev-1", []*Share{}), "SetShares empty")

	got, err := s.GetShares(ctx, "dev-1")
	require.NoError(t, err, "GetShares")
	assert.Empty(t, got)
}

func TestGetShares_NoShares(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.CreateDevice(ctx, makeDevice("dev-1", "alice")), "CreateDevice")

	got, err := s.GetShares(ctx, "dev-1")
	require.NoError(t, err, "GetShares")
	assert.Empty(t, got)
}

// --- Pairing tests ---

func TestCreatePairing(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.CreatePairing(ctx, "dev-a", "dev-b"), "CreatePairing")
}

func TestGetPairing(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.CreatePairing(ctx, "dev-a", "dev-b"), "CreatePairing")

	p, err := s.GetPairing(ctx, "dev-a", "dev-b")
	require.NoError(t, err, "GetPairing")
	assert.Equal(t, "dev-a", p.DeviceA)
	assert.Equal(t, "dev-b", p.DeviceB)
	assert.False(t, p.PairedAt.IsZero(), "PairedAt is zero")
}

func TestGetPairing_NotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, err := s.GetPairing(ctx, "dev-a", "dev-b")
	assert.Error(t, err, "expected error for nonexistent pairing")
}

func TestGetPairing_ReverseOrderNotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Store (a, b) — lookup (b, a) should fail since GetPairing is order-sensitive.
	require.NoError(t, s.CreatePairing(ctx, "dev-a", "dev-b"), "CreatePairing")

	_, err := s.GetPairing(ctx, "dev-b", "dev-a")
	assert.Error(t, err, "expected error for reverse-order lookup")
}

func TestIsPaired_BothOrderings(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.CreatePairing(ctx, "dev-a", "dev-b"), "CreatePairing")

	// Forward ordering.
	ok, err := s.IsPaired(ctx, "dev-a", "dev-b")
	require.NoError(t, err, "IsPaired (a,b)")
	assert.True(t, ok, "IsPaired(a,b) = false, want true")

	// Reverse ordering.
	ok, err = s.IsPaired(ctx, "dev-b", "dev-a")
	require.NoError(t, err, "IsPaired (b,a)")
	assert.True(t, ok, "IsPaired(b,a) = false, want true")
}

func TestIsPaired_NotPaired(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	ok, err := s.IsPaired(ctx, "dev-a", "dev-b")
	require.NoError(t, err, "IsPaired")
	assert.False(t, ok, "IsPaired = true for non-existent pairing, want false")
}

// --- Invite tests ---

func makeInvite(code string, expiresIn time.Duration) *PendingInvite {
	return &PendingInvite{
		InviteCode:    code,
		FromDevice:    "dev-from",
		ToDevice:      "dev-to",
		FromPublicKey: "ssh-rsa AAAA...",
		ExpiresAt:     time.Now().UTC().Add(expiresIn).Truncate(time.Second),
		Attempts:      0,
	}
}

func TestCreateInvite(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	inv := makeInvite("code-abc", 5*time.Minute)
	require.NoError(t, s.CreateInvite(ctx, inv), "CreateInvite")
}

func TestGetInvite(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	inv := makeInvite("code-abc", 5*time.Minute)
	require.NoError(t, s.CreateInvite(ctx, inv), "CreateInvite")

	got, err := s.GetInvite(ctx, "code-abc")
	require.NoError(t, err, "GetInvite")
	assert.Equal(t, inv.InviteCode, got.InviteCode)
	assert.Equal(t, inv.FromDevice, got.FromDevice)
	assert.Equal(t, inv.ToDevice, got.ToDevice)
	assert.Equal(t, inv.FromPublicKey, got.FromPublicKey)
	assert.Equal(t, 0, got.Attempts)
}

func TestGetInvite_NotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, err := s.GetInvite(ctx, "nonexistent")
	assert.Error(t, err, "expected error for nonexistent invite")
}

func TestIncrementInviteAttempts(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	inv := makeInvite("code-abc", 5*time.Minute)
	require.NoError(t, s.CreateInvite(ctx, inv), "CreateInvite")

	for i := 1; i <= 3; i++ {
		require.NoError(t, s.IncrementInviteAttempts(ctx, "code-abc"), "IncrementInviteAttempts (iteration %d)", i)
		got, err := s.GetInvite(ctx, "code-abc")
		require.NoErrorf(t, err, "GetInvite (iteration %d)", i)
		assert.Equal(t, i, got.Attempts)
	}
}

func TestDeleteInvite(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	inv := makeInvite("code-abc", 5*time.Minute)
	require.NoError(t, s.CreateInvite(ctx, inv), "CreateInvite")
	require.NoError(t, s.DeleteInvite(ctx, "code-abc"), "DeleteInvite")

	_, err := s.GetInvite(ctx, "code-abc")
	assert.Error(t, err, "expected error after deletion")
}

func TestDeleteExpiredInvites(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	expired := makeInvite("code-expired", -1*time.Minute) // already in the past
	valid := makeInvite("code-valid", 10*time.Minute)

	require.NoError(t, s.CreateInvite(ctx, expired), "CreateInvite expired")
	require.NoError(t, s.CreateInvite(ctx, valid), "CreateInvite valid")
	require.NoError(t, s.DeleteExpiredInvites(ctx), "DeleteExpiredInvites")

	// Expired invite should be gone.
	_, err := s.GetInvite(ctx, "code-expired")
	assert.Error(t, err, "expected error for expired invite after cleanup")

	// Valid invite should still exist.
	got, err := s.GetInvite(ctx, "code-valid")
	require.NoError(t, err, "GetInvite valid")
	assert.Equal(t, "code-valid", got.InviteCode)
}

func TestInviteLifecycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	inv := makeInvite("lifecycle-code", 5*time.Minute)
	require.NoError(t, s.CreateInvite(ctx, inv), "CreateInvite")

	// Increment attempts twice.
	require.NoError(t, s.IncrementInviteAttempts(ctx, "lifecycle-code"), "IncrementInviteAttempts")
	require.NoError(t, s.IncrementInviteAttempts(ctx, "lifecycle-code"), "IncrementInviteAttempts")

	got, err := s.GetInvite(ctx, "lifecycle-code")
	require.NoError(t, err, "GetInvite")
	assert.Equal(t, 2, got.Attempts)

	// Delete the invite.
	require.NoError(t, s.DeleteInvite(ctx, "lifecycle-code"), "DeleteInvite")

	_, err = s.GetInvite(ctx, "lifecycle-code")
	assert.Error(t, err, "expected error after deletion")
}

// TestStoreErrors is a sanity check that the store wraps errors and does not
// panic on operations against an empty database.
func TestStoreErrors(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Operations on nonexistent entities should return non-nil errors.
	errCases := []struct {
		name string
		fn   func() error
	}{
		{"GetDevice", func() error { _, err := s.GetDevice(ctx, "x"); return err }},
		{"GetDeviceByNickname", func() error { _, err := s.GetDeviceByNickname(ctx, "x"); return err }},
		{"GetPairing", func() error { _, err := s.GetPairing(ctx, "a", "b"); return err }},
		{"GetInvite", func() error { _, err := s.GetInvite(ctx, "x"); return err }},
	}

	for _, tc := range errCases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.fn()
			assert.Error(t, err, "%s: expected error, got nil", tc.name)
			// Errors must not be nil but need not be wrapped — just ensure
			// they satisfy the error interface.
			var _ = err
		})
	}
}

func TestListAllDevices(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	d1 := makeDevice("dev-1", "alice")
	d2 := makeDevice("dev-2", "bob")
	require.NoError(t, s.CreateDevice(ctx, d1), "CreateDevice d1")
	require.NoError(t, s.CreateDevice(ctx, d2), "CreateDevice d2")
	// Mark d1 online.
	require.NoError(t, s.UpdateDeviceStatus(ctx, "dev-1", StatusOnline, "10.0.0.1", 2222), "UpdateDeviceStatus")

	devices, err := s.ListAllDevices(ctx)
	require.NoError(t, err, "ListAllDevices")
	require.Len(t, devices, 2)

	statusMap := map[string]DeviceStatus{}
	for _, d := range devices {
		statusMap[d.DeviceID] = d.Status
	}
	assert.Equal(t, StatusOnline, statusMap["dev-1"])
	assert.Equal(t, StatusOffline, statusMap["dev-2"])
}

// TestUpdateHeartbeat_Idempotent verifies that calling UpdateHeartbeat multiple
// times on the same device succeeds without error.
func TestUpdateHeartbeat_Idempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.CreateDevice(ctx, makeDevice("dev-1", "alice")), "CreateDevice")

	for i := 0; i < 3; i++ {
		require.NoError(t, s.UpdateHeartbeat(ctx, "dev-1"), "UpdateHeartbeat (round %d)", i)
	}
}

// TestDeleteExpiredInvites_NoExpired verifies that deleting expired invites
// when none exist does not return an error.
func TestDeleteExpiredInvites_NoExpired(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.DeleteExpiredInvites(ctx), "DeleteExpiredInvites (empty)")
}

// TestSetShares_NilAllowedDevices verifies that a nil AllowedDevices slice
// is stored and retrieved cleanly.
func TestSetShares_NilAllowedDevices(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.CreateDevice(ctx, makeDevice("dev-1", "alice")), "CreateDevice")

	shares := []*Share{
		{DeviceID: "dev-1", Alias: "data", Permissions: PermRO, AllowedDevices: nil},
	}
	require.NoError(t, s.SetShares(ctx, "dev-1", shares), "SetShares")

	got, err := s.GetShares(ctx, "dev-1")
	require.NoError(t, err, "GetShares")
	require.Len(t, got, 1)
}

// TestPairing_Duplicate verifies that inserting the same pairing twice returns
// an error.
func TestPairing_Duplicate(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	require.NoError(t, s.CreatePairing(ctx, "dev-a", "dev-b"), "CreatePairing first")
	err := s.CreatePairing(ctx, "dev-a", "dev-b")
	assert.Error(t, err, "expected error for duplicate pairing")
}

// --- Join Token tests ---

func TestJoinTokenCRUD(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)
	jt := &JoinToken{
		Token:     "HUB-ABC-123",
		ExpiresAt: now.Add(10 * time.Minute),
		CreatedAt: now,
	}

	// Create
	require.NoError(t, s.CreateJoinToken(ctx, jt), "CreateJoinToken")

	// Get returns same fields.
	got, err := s.GetJoinToken(ctx, "HUB-ABC-123")
	require.NoError(t, err, "GetJoinToken")
	assert.Equal(t, jt.Token, got.Token)
	require.WithinDuration(t, jt.ExpiresAt, got.ExpiresAt, time.Second)
	require.WithinDuration(t, jt.CreatedAt, got.CreatedAt, time.Second)

	// Claim removes the row and returns true exactly once.
	claimed, err := s.ClaimJoinToken(ctx, "HUB-ABC-123", time.Now())
	require.NoError(t, err, "ClaimJoinToken (1)")
	assert.True(t, claimed, "first claim should succeed")

	claimed, err = s.ClaimJoinToken(ctx, "HUB-ABC-123", time.Now())
	require.NoError(t, err, "ClaimJoinToken (2)")
	assert.False(t, claimed, "second claim should fail (row already gone)")

	_, err = s.GetJoinToken(ctx, "HUB-ABC-123")
	assert.Error(t, err, "expected error after claim consumed the row")
}

func TestClaimJoinToken_RejectsExpired(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	require.NoError(t, s.CreateJoinToken(ctx, &JoinToken{
		Token:     "HUB-EXP-777",
		ExpiresAt: now.Add(-time.Minute),
		CreatedAt: now.Add(-10 * time.Minute),
	}))

	claimed, err := s.ClaimJoinToken(ctx, "HUB-EXP-777", now)
	require.NoError(t, err, "ClaimJoinToken expired")
	assert.False(t, claimed, "expired token must not be claimable")

	// Row is still there — only the sweeper removes expired rows.
	got, err := s.GetJoinToken(ctx, "HUB-EXP-777")
	require.NoError(t, err, "expired row should remain until sweep")
	assert.Equal(t, "HUB-EXP-777", got.Token)
}

func TestDeleteExpiredJoinTokens(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Second)

	expired := &JoinToken{
		Token:     "HUB-EXP-001",
		ExpiresAt: now.Add(-1 * time.Minute),
		CreatedAt: now.Add(-2 * time.Minute),
	}
	live := &JoinToken{
		Token:     "HUB-LIV-002",
		ExpiresAt: now.Add(10 * time.Minute),
		CreatedAt: now,
	}

	require.NoError(t, s.CreateJoinToken(ctx, expired), "CreateJoinToken expired")
	require.NoError(t, s.CreateJoinToken(ctx, live), "CreateJoinToken live")

	require.NoError(t, s.DeleteExpiredJoinTokens(ctx), "DeleteExpiredJoinTokens")

	// Live token still gettable.
	got, err := s.GetJoinToken(ctx, "HUB-LIV-002")
	require.NoError(t, err, "GetJoinToken live after cleanup")
	assert.Equal(t, "HUB-LIV-002", got.Token)

	// Expired token gone.
	_, err = s.GetJoinToken(ctx, "HUB-EXP-001")
	assert.Error(t, err, "expected error for expired join token after cleanup")
}
