package store

import (
	"context"
	"testing"
	"time"
)

// newTestStore opens an in-memory SQLite store and registers a cleanup that
// closes it when the test ends.
func newTestStore(t *testing.T) Store {
	t.Helper()
	s, err := NewSQLiteStore(":memory:")
	if err != nil {
		t.Fatalf("NewSQLiteStore(:memory:): %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func makeDevice(id, nickname string) *Device {
	return &Device{
		DeviceID:      id,
		Nickname:      nickname,
		LastIP:        "192.168.1.1",
		SSHPort:       22,
		Status:        "offline",
		LastHeartbeat: time.Now().UTC().Truncate(time.Second),
	}
}

// --- Device tests ---

func TestCreateDevice(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	d := makeDevice("dev-1", "alice")
	if err := s.CreateDevice(ctx, d); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}

	got, err := s.GetDevice(ctx, "dev-1")
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	if got.DeviceID != d.DeviceID {
		t.Errorf("DeviceID = %q, want %q", got.DeviceID, d.DeviceID)
	}
	if got.Nickname != d.Nickname {
		t.Errorf("Nickname = %q, want %q", got.Nickname, d.Nickname)
	}
	if got.SSHPort != d.SSHPort {
		t.Errorf("SSHPort = %d, want %d", got.SSHPort, d.SSHPort)
	}
	if got.Status != d.Status {
		t.Errorf("Status = %q, want %q", got.Status, d.Status)
	}
}

func TestGetDevice_NotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, err := s.GetDevice(ctx, "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent device, got nil")
	}
}

func TestCreateDevice_DuplicateNickname(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.CreateDevice(ctx, makeDevice("dev-1", "alice")); err != nil {
		t.Fatalf("CreateDevice first: %v", err)
	}
	// Second device with same nickname should fail.
	err := s.CreateDevice(ctx, makeDevice("dev-2", "alice"))
	if err == nil {
		t.Fatal("expected error for duplicate nickname, got nil")
	}
}

func TestCreateDevice_DuplicateID(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.CreateDevice(ctx, makeDevice("dev-1", "alice")); err != nil {
		t.Fatalf("CreateDevice first: %v", err)
	}
	err := s.CreateDevice(ctx, makeDevice("dev-1", "bob"))
	if err == nil {
		t.Fatal("expected error for duplicate device_id, got nil")
	}
}

func TestGetDeviceByNickname(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	d := makeDevice("dev-1", "alice")
	if err := s.CreateDevice(ctx, d); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}

	got, err := s.GetDeviceByNickname(ctx, "alice")
	if err != nil {
		t.Fatalf("GetDeviceByNickname: %v", err)
	}
	if got.DeviceID != "dev-1" {
		t.Errorf("DeviceID = %q, want %q", got.DeviceID, "dev-1")
	}
}

func TestGetDeviceByNickname_NotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, err := s.GetDeviceByNickname(ctx, "nobody")
	if err == nil {
		t.Fatal("expected error for nonexistent nickname, got nil")
	}
}

func TestListOnlineDevices(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	online := makeDevice("dev-1", "alice")
	online.Status = "online"
	offline := makeDevice("dev-2", "bob")
	offline.Status = "offline"

	if err := s.CreateDevice(ctx, online); err != nil {
		t.Fatalf("CreateDevice online: %v", err)
	}
	if err := s.CreateDevice(ctx, offline); err != nil {
		t.Fatalf("CreateDevice offline: %v", err)
	}

	list, err := s.ListOnlineDevices(ctx)
	if err != nil {
		t.Fatalf("ListOnlineDevices: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("len(list) = %d, want 1", len(list))
	}
	if list[0].DeviceID != "dev-1" {
		t.Errorf("DeviceID = %q, want %q", list[0].DeviceID, "dev-1")
	}
}

func TestListOnlineDevices_Empty(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	list, err := s.ListOnlineDevices(ctx)
	if err != nil {
		t.Fatalf("ListOnlineDevices: %v", err)
	}
	if len(list) != 0 {
		t.Errorf("expected empty list, got %d devices", len(list))
	}
}

func TestUpdateDeviceStatus(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.CreateDevice(ctx, makeDevice("dev-1", "alice")); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}

	if err := s.UpdateDeviceStatus(ctx, "dev-1", "online", "10.0.0.5", 2222); err != nil {
		t.Fatalf("UpdateDeviceStatus: %v", err)
	}

	got, err := s.GetDevice(ctx, "dev-1")
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	if got.Status != "online" {
		t.Errorf("Status = %q, want %q", got.Status, "online")
	}
	if got.LastIP != "10.0.0.5" {
		t.Errorf("LastIP = %q, want %q", got.LastIP, "10.0.0.5")
	}
	if got.SSHPort != 2222 {
		t.Errorf("SSHPort = %d, want 2222", got.SSHPort)
	}
}

func TestUpdateDeviceNickname(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.CreateDevice(ctx, makeDevice("dev-1", "alice")); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}

	if err := s.UpdateDeviceNickname(ctx, "dev-1", "alicia"); err != nil {
		t.Fatalf("UpdateDeviceNickname: %v", err)
	}

	got, err := s.GetDevice(ctx, "dev-1")
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	if got.Nickname != "alicia" {
		t.Errorf("Nickname = %q, want %q", got.Nickname, "alicia")
	}
}

func TestUpdateHeartbeat(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	d := makeDevice("dev-1", "alice")
	d.LastHeartbeat = time.Now().UTC().Add(-1 * time.Hour)
	if err := s.CreateDevice(ctx, d); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}

	before := time.Now().UTC()
	if err := s.UpdateHeartbeat(ctx, "dev-1"); err != nil {
		t.Fatalf("UpdateHeartbeat: %v", err)
	}
	after := time.Now().UTC()

	got, err := s.GetDevice(ctx, "dev-1")
	if err != nil {
		t.Fatalf("GetDevice: %v", err)
	}
	if got.LastHeartbeat.Before(before) || got.LastHeartbeat.After(after) {
		t.Errorf("LastHeartbeat %v not in expected range [%v, %v]",
			got.LastHeartbeat, before, after)
	}
}

func TestGetStaleDevices(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	old := makeDevice("dev-old", "alice")
	old.Status = "online"
	old.LastHeartbeat = time.Now().UTC().Add(-10 * time.Minute)

	fresh := makeDevice("dev-fresh", "bob")
	fresh.Status = "online"
	fresh.LastHeartbeat = time.Now().UTC()

	offlineOld := makeDevice("dev-offline", "charlie")
	offlineOld.Status = "offline"
	offlineOld.LastHeartbeat = time.Now().UTC().Add(-10 * time.Minute)

	for _, d := range []*Device{old, fresh, offlineOld} {
		if err := s.CreateDevice(ctx, d); err != nil {
			t.Fatalf("CreateDevice %q: %v", d.DeviceID, err)
		}
	}

	// Threshold is 5 minutes ago — only dev-old is stale online.
	threshold := time.Now().UTC().Add(-5 * time.Minute)
	stale, err := s.GetStaleDevices(ctx, threshold)
	if err != nil {
		t.Fatalf("GetStaleDevices: %v", err)
	}
	if len(stale) != 1 {
		t.Fatalf("len(stale) = %d, want 1", len(stale))
	}
	if stale[0].DeviceID != "dev-old" {
		t.Errorf("stale DeviceID = %q, want %q", stale[0].DeviceID, "dev-old")
	}
}

func TestGetStaleDevices_NoneStale(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	d := makeDevice("dev-1", "alice")
	d.Status = "online"
	d.LastHeartbeat = time.Now().UTC()
	if err := s.CreateDevice(ctx, d); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}

	threshold := time.Now().UTC().Add(-5 * time.Minute)
	stale, err := s.GetStaleDevices(ctx, threshold)
	if err != nil {
		t.Fatalf("GetStaleDevices: %v", err)
	}
	if len(stale) != 0 {
		t.Errorf("expected 0 stale devices, got %d", len(stale))
	}
}

func TestDeleteDevice(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.CreateDevice(ctx, makeDevice("dev-1", "alice")); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}

	if err := s.DeleteDevice(ctx, "dev-1"); err != nil {
		t.Fatalf("DeleteDevice: %v", err)
	}

	_, err := s.GetDevice(ctx, "dev-1")
	if err == nil {
		t.Fatal("expected error after deletion, got nil")
	}
}

// --- Share tests ---

func TestSetShares_AndGet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.CreateDevice(ctx, makeDevice("dev-1", "alice")); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}

	shares := []*Share{
		{DeviceID: "dev-1", Alias: "docs", Permissions: "ro", AllowedDevices: []string{"dev-2", "dev-3"}},
		{DeviceID: "dev-1", Alias: "music", Permissions: "rw", AllowedDevices: []string{}},
	}
	if err := s.SetShares(ctx, "dev-1", shares); err != nil {
		t.Fatalf("SetShares: %v", err)
	}

	got, err := s.GetShares(ctx, "dev-1")
	if err != nil {
		t.Fatalf("GetShares: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2", len(got))
	}

	// Build a map for order-independent comparison.
	byAlias := make(map[string]*Share, len(got))
	for _, sh := range got {
		byAlias[sh.Alias] = sh
	}

	docs, ok := byAlias["docs"]
	if !ok {
		t.Fatal("share 'docs' not found")
	}
	if docs.Permissions != "ro" {
		t.Errorf("docs.Permissions = %q, want %q", docs.Permissions, "ro")
	}
	if len(docs.AllowedDevices) != 2 {
		t.Errorf("docs.AllowedDevices len = %d, want 2", len(docs.AllowedDevices))
	}

	music, ok := byAlias["music"]
	if !ok {
		t.Fatal("share 'music' not found")
	}
	if music.Permissions != "rw" {
		t.Errorf("music.Permissions = %q, want %q", music.Permissions, "rw")
	}
}

func TestSetShares_ReplacesExisting(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.CreateDevice(ctx, makeDevice("dev-1", "alice")); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}

	original := []*Share{
		{DeviceID: "dev-1", Alias: "old-share", Permissions: "ro", AllowedDevices: []string{}},
	}
	if err := s.SetShares(ctx, "dev-1", original); err != nil {
		t.Fatalf("SetShares original: %v", err)
	}

	replacement := []*Share{
		{DeviceID: "dev-1", Alias: "new-share", Permissions: "rw", AllowedDevices: []string{"dev-2"}},
	}
	if err := s.SetShares(ctx, "dev-1", replacement); err != nil {
		t.Fatalf("SetShares replacement: %v", err)
	}

	got, err := s.GetShares(ctx, "dev-1")
	if err != nil {
		t.Fatalf("GetShares: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
	if got[0].Alias != "new-share" {
		t.Errorf("Alias = %q, want %q", got[0].Alias, "new-share")
	}
}

func TestSetShares_Empty(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.CreateDevice(ctx, makeDevice("dev-1", "alice")); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}

	if err := s.SetShares(ctx, "dev-1", []*Share{}); err != nil {
		t.Fatalf("SetShares empty: %v", err)
	}

	got, err := s.GetShares(ctx, "dev-1")
	if err != nil {
		t.Fatalf("GetShares: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 shares, got %d", len(got))
	}
}

func TestGetShares_NoShares(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.CreateDevice(ctx, makeDevice("dev-1", "alice")); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}

	got, err := s.GetShares(ctx, "dev-1")
	if err != nil {
		t.Fatalf("GetShares: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected 0 shares, got %d", len(got))
	}
}

// --- Pairing tests ---

func TestCreatePairing(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.CreatePairing(ctx, "dev-a", "dev-b"); err != nil {
		t.Fatalf("CreatePairing: %v", err)
	}
}

func TestGetPairing(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.CreatePairing(ctx, "dev-a", "dev-b"); err != nil {
		t.Fatalf("CreatePairing: %v", err)
	}

	p, err := s.GetPairing(ctx, "dev-a", "dev-b")
	if err != nil {
		t.Fatalf("GetPairing: %v", err)
	}
	if p.DeviceA != "dev-a" {
		t.Errorf("DeviceA = %q, want %q", p.DeviceA, "dev-a")
	}
	if p.DeviceB != "dev-b" {
		t.Errorf("DeviceB = %q, want %q", p.DeviceB, "dev-b")
	}
	if p.PairedAt.IsZero() {
		t.Error("PairedAt is zero")
	}
}

func TestGetPairing_NotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, err := s.GetPairing(ctx, "dev-a", "dev-b")
	if err == nil {
		t.Fatal("expected error for nonexistent pairing, got nil")
	}
}

func TestGetPairing_ReverseOrderNotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Store (a, b) — lookup (b, a) should fail since GetPairing is order-sensitive.
	if err := s.CreatePairing(ctx, "dev-a", "dev-b"); err != nil {
		t.Fatalf("CreatePairing: %v", err)
	}

	_, err := s.GetPairing(ctx, "dev-b", "dev-a")
	if err == nil {
		t.Fatal("expected error for reverse-order lookup, got nil")
	}
}

func TestIsPaired_BothOrderings(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.CreatePairing(ctx, "dev-a", "dev-b"); err != nil {
		t.Fatalf("CreatePairing: %v", err)
	}

	// Forward ordering.
	ok, err := s.IsPaired(ctx, "dev-a", "dev-b")
	if err != nil {
		t.Fatalf("IsPaired (a,b): %v", err)
	}
	if !ok {
		t.Error("IsPaired(a,b) = false, want true")
	}

	// Reverse ordering.
	ok, err = s.IsPaired(ctx, "dev-b", "dev-a")
	if err != nil {
		t.Fatalf("IsPaired (b,a): %v", err)
	}
	if !ok {
		t.Error("IsPaired(b,a) = false, want true")
	}
}

func TestIsPaired_NotPaired(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	ok, err := s.IsPaired(ctx, "dev-a", "dev-b")
	if err != nil {
		t.Fatalf("IsPaired: %v", err)
	}
	if ok {
		t.Error("IsPaired = true for non-existent pairing, want false")
	}
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
	if err := s.CreateInvite(ctx, inv); err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}
}

func TestGetInvite(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	inv := makeInvite("code-abc", 5*time.Minute)
	if err := s.CreateInvite(ctx, inv); err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}

	got, err := s.GetInvite(ctx, "code-abc")
	if err != nil {
		t.Fatalf("GetInvite: %v", err)
	}
	if got.InviteCode != inv.InviteCode {
		t.Errorf("InviteCode = %q, want %q", got.InviteCode, inv.InviteCode)
	}
	if got.FromDevice != inv.FromDevice {
		t.Errorf("FromDevice = %q, want %q", got.FromDevice, inv.FromDevice)
	}
	if got.ToDevice != inv.ToDevice {
		t.Errorf("ToDevice = %q, want %q", got.ToDevice, inv.ToDevice)
	}
	if got.FromPublicKey != inv.FromPublicKey {
		t.Errorf("FromPublicKey = %q, want %q", got.FromPublicKey, inv.FromPublicKey)
	}
	if got.Attempts != 0 {
		t.Errorf("Attempts = %d, want 0", got.Attempts)
	}
}

func TestGetInvite_NotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, err := s.GetInvite(ctx, "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent invite, got nil")
	}
}

func TestIncrementInviteAttempts(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	inv := makeInvite("code-abc", 5*time.Minute)
	if err := s.CreateInvite(ctx, inv); err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}

	for i := 1; i <= 3; i++ {
		if err := s.IncrementInviteAttempts(ctx, "code-abc"); err != nil {
			t.Fatalf("IncrementInviteAttempts (iteration %d): %v", i, err)
		}
		got, err := s.GetInvite(ctx, "code-abc")
		if err != nil {
			t.Fatalf("GetInvite (iteration %d): %v", i, err)
		}
		if got.Attempts != i {
			t.Errorf("Attempts = %d, want %d", got.Attempts, i)
		}
	}
}

func TestDeleteInvite(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	inv := makeInvite("code-abc", 5*time.Minute)
	if err := s.CreateInvite(ctx, inv); err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}

	if err := s.DeleteInvite(ctx, "code-abc"); err != nil {
		t.Fatalf("DeleteInvite: %v", err)
	}

	_, err := s.GetInvite(ctx, "code-abc")
	if err == nil {
		t.Fatal("expected error after deletion, got nil")
	}
}

func TestDeleteExpiredInvites(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	expired := makeInvite("code-expired", -1*time.Minute) // already in the past
	valid := makeInvite("code-valid", 10*time.Minute)

	if err := s.CreateInvite(ctx, expired); err != nil {
		t.Fatalf("CreateInvite expired: %v", err)
	}
	if err := s.CreateInvite(ctx, valid); err != nil {
		t.Fatalf("CreateInvite valid: %v", err)
	}

	if err := s.DeleteExpiredInvites(ctx); err != nil {
		t.Fatalf("DeleteExpiredInvites: %v", err)
	}

	// Expired invite should be gone.
	_, err := s.GetInvite(ctx, "code-expired")
	if err == nil {
		t.Fatal("expected error for expired invite after cleanup, got nil")
	}

	// Valid invite should still exist.
	got, err := s.GetInvite(ctx, "code-valid")
	if err != nil {
		t.Fatalf("GetInvite valid: %v", err)
	}
	if got.InviteCode != "code-valid" {
		t.Errorf("InviteCode = %q, want %q", got.InviteCode, "code-valid")
	}
}

func TestInviteLifecycle(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	inv := makeInvite("lifecycle-code", 5*time.Minute)
	if err := s.CreateInvite(ctx, inv); err != nil {
		t.Fatalf("CreateInvite: %v", err)
	}

	// Increment attempts twice.
	if err := s.IncrementInviteAttempts(ctx, "lifecycle-code"); err != nil {
		t.Fatalf("IncrementInviteAttempts: %v", err)
	}
	if err := s.IncrementInviteAttempts(ctx, "lifecycle-code"); err != nil {
		t.Fatalf("IncrementInviteAttempts: %v", err)
	}

	got, err := s.GetInvite(ctx, "lifecycle-code")
	if err != nil {
		t.Fatalf("GetInvite: %v", err)
	}
	if got.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2", got.Attempts)
	}

	// Delete the invite.
	if err := s.DeleteInvite(ctx, "lifecycle-code"); err != nil {
		t.Fatalf("DeleteInvite: %v", err)
	}

	_, err = s.GetInvite(ctx, "lifecycle-code")
	if err == nil {
		t.Fatal("expected error after deletion, got nil")
	}
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
			if err == nil {
				t.Fatalf("%s: expected error, got nil", tc.name)
			}
			// Errors must not be nil but need not be wrapped — just ensure
			// they satisfy the error interface.
			var _ error = err
		})
	}
}

// TestUpdateHeartbeat_Idempotent verifies that calling UpdateHeartbeat multiple
// times on the same device succeeds without error.
func TestUpdateHeartbeat_Idempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.CreateDevice(ctx, makeDevice("dev-1", "alice")); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}

	for i := 0; i < 3; i++ {
		if err := s.UpdateHeartbeat(ctx, "dev-1"); err != nil {
			t.Fatalf("UpdateHeartbeat (round %d): %v", i, err)
		}
	}
}

// TestDeleteExpiredInvites_NoExpired verifies that deleting expired invites
// when none exist does not return an error.
func TestDeleteExpiredInvites_NoExpired(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.DeleteExpiredInvites(ctx); err != nil {
		t.Fatalf("DeleteExpiredInvites (empty): %v", err)
	}
}

// TestSetShares_NilAllowedDevices verifies that a nil AllowedDevices slice
// is stored and retrieved cleanly.
func TestSetShares_NilAllowedDevices(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.CreateDevice(ctx, makeDevice("dev-1", "alice")); err != nil {
		t.Fatalf("CreateDevice: %v", err)
	}

	shares := []*Share{
		{DeviceID: "dev-1", Alias: "data", Permissions: "ro", AllowedDevices: nil},
	}
	if err := s.SetShares(ctx, "dev-1", shares); err != nil {
		t.Fatalf("SetShares: %v", err)
	}

	got, err := s.GetShares(ctx, "dev-1")
	if err != nil {
		t.Fatalf("GetShares: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len(got) = %d, want 1", len(got))
	}
}

// TestPairing_Duplicate verifies that inserting the same pairing twice returns
// an error.
func TestPairing_Duplicate(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	if err := s.CreatePairing(ctx, "dev-a", "dev-b"); err != nil {
		t.Fatalf("CreatePairing first: %v", err)
	}
	err := s.CreatePairing(ctx, "dev-a", "dev-b")
	if err == nil {
		t.Fatal("expected error for duplicate pairing, got nil")
	}
}

