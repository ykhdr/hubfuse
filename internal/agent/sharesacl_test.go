package agent

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

type stubResolver map[string]string // deviceID -> nickname

func (s stubResolver) NicknameForDeviceID(id string) (string, bool) {
	n, ok := s[id]
	return n, ok
}

func TestShareACL_Decide_AllowAllWildcard(t *testing.T) {
	acl := ShareACL{Alias: "foo", Path: "/tmp/foo", ReadOnly: true, AllowAll: true}
	d := acl.Decide("dev-1", stubResolver{"dev-1": "bob"})
	assert.True(t, d.Allow)
}

func TestShareACL_Decide_EmptyDenies(t *testing.T) {
	acl := ShareACL{Alias: "foo", Path: "/tmp/foo"}
	d := acl.Decide("dev-1", stubResolver{"dev-1": "bob"})
	assert.False(t, d.Allow, "empty allowed list must deny")
}

func TestShareACL_Decide_MatchesNickname(t *testing.T) {
	acl := ShareACL{Alias: "foo", Path: "/tmp/foo", AllowedDevices: []string{"bob"}}
	d := acl.Decide("dev-1", stubResolver{"dev-1": "bob"})
	assert.True(t, d.Allow)
}

func TestShareACL_Decide_MatchesDeviceID(t *testing.T) {
	acl := ShareACL{Alias: "foo", Path: "/tmp/foo", AllowedDevices: []string{"dev-1"}}
	d := acl.Decide("dev-1", stubResolver{})
	assert.True(t, d.Allow, "device_id match should work even without nickname resolver entry")
}

func TestShareACL_Decide_NoMatchDenies(t *testing.T) {
	acl := ShareACL{Alias: "foo", Path: "/tmp/foo", AllowedDevices: []string{"carol"}}
	d := acl.Decide("dev-1", stubResolver{"dev-1": "bob"})
	assert.False(t, d.Allow)
}

func TestShareACL_Decide_ReadOnlyFlag(t *testing.T) {
	acl := ShareACL{Alias: "foo", Path: "/tmp/foo", ReadOnly: true, AllowAll: true}
	d := acl.Decide("dev-1", stubResolver{})
	assert.True(t, d.Allow)
	assert.True(t, d.ReadOnly)
}

func TestFromShareConfigs_Defaults(t *testing.T) {
	// Empty permissions → ro; literal "all" → AllowAll; explicit list preserved.
	in := []shareConfigView{
		{Alias: "a", Path: "/p/a", Permissions: "", AllowedDevices: nil},
		{Alias: "b", Path: "/p/b", Permissions: "rw", AllowedDevices: []string{"all"}},
		{Alias: "c", Path: "/p/c", Permissions: "ro", AllowedDevices: []string{"bob", "carol"}},
	}
	got := ShareACLsFromConfig(in)
	assert.Len(t, got, 3)

	assert.Equal(t, "a", got[0].Alias)
	assert.True(t, got[0].ReadOnly, "missing permissions defaults to ro")
	assert.False(t, got[0].AllowAll)
	assert.Empty(t, got[0].AllowedDevices)

	assert.Equal(t, "b", got[1].Alias)
	assert.False(t, got[1].ReadOnly)
	assert.True(t, got[1].AllowAll, `"all" must be recognised as wildcard`)

	assert.Equal(t, "c", got[2].Alias)
	assert.True(t, got[2].ReadOnly)
	assert.False(t, got[2].AllowAll)
	assert.Equal(t, []string{"bob", "carol"}, got[2].AllowedDevices)
}

// ─── Regression tests for issue #48 (nickname ACL race) ──────────────────────

// TestShareACL_Decide_NicknameResolvedFromPairedState verifies the fix:
// once the resolver knows the paired peer's nickname (from the persisted
// cache loaded at startup), an allowed-by-nickname peer is permitted even
// when it has not yet appeared in onlineDevices.
func TestShareACL_Decide_NicknameResolvedFromPairedState(t *testing.T) {
	acl := ShareACL{Alias: "macshare", Path: "/tmp/m", AllowedDevices: []string{"server"}}
	// Fixed state: resolver knows the paired peer's nickname even though the
	// peer was not in onlineDevices (here modelled by the persisted-fallback
	// stub returning ok=true).
	r := stubResolver{"dev-server": "server"}
	d := acl.Decide("dev-server", r)
	assert.True(t, d.Allow, "authorized peer must be allowed once nickname resolves from paired state")
}

// TestShareACL_Decide_UnresolvedNicknameStillDeniesUnlisted verifies the
// security guarantee: an unlisted peer that cannot be resolved must be denied
// (no fail-open) — the fix must not widen the allow set for unauthorized peers.
func TestShareACL_Decide_UnresolvedNicknameStillDeniesUnlisted(t *testing.T) {
	acl := ShareACL{Alias: "macshare", Path: "/tmp/m", AllowedDevices: []string{"server"}}
	// Unlisted peer; resolver reports ok=false (unresolved window). Must DENY.
	d := acl.Decide("dev-attacker", stubResolver{})
	assert.False(t, d.Allow, "unlisted peer with unresolved nickname must stay denied (no fail-open)")
}

// TestShareACL_Decide_WrongNicknameDenies verifies that a connecting device
// that resolves to a different nickname (not listed in allowed-devices) is
// still denied — the resolver entry alone does not grant access.
func TestShareACL_Decide_WrongNicknameDenies(t *testing.T) {
	acl := ShareACL{Alias: "macshare", Path: "/tmp/m", AllowedDevices: []string{"server"}}
	// Resolver resolves the connecting device to a DIFFERENT nickname → deny.
	d := acl.Decide("dev-x", stubResolver{"dev-x": "laptop"})
	assert.False(t, d.Allow)
}

func TestResolveSharePath_Basic(t *testing.T) {
	got, err := ResolveSharePath("/srv/docs", "/docs/a/b.txt", "docs")
	assert.NoError(t, err)
	assert.Equal(t, "/srv/docs/a/b.txt", got)
}

func TestResolveSharePath_AliasRoot(t *testing.T) {
	got, err := ResolveSharePath("/srv/docs", "/docs", "docs")
	assert.NoError(t, err)
	assert.Equal(t, "/srv/docs", got)
}

func TestResolveSharePath_TrailingSlash(t *testing.T) {
	got, err := ResolveSharePath("/srv/docs", "/docs/", "docs")
	assert.NoError(t, err)
	assert.Equal(t, "/srv/docs", got)
}

func TestResolveSharePath_RejectsTraversal(t *testing.T) {
	_, err := ResolveSharePath("/srv/docs", "/docs/../../etc/passwd", "docs")
	assert.Error(t, err, "must reject path that escapes share root after cleaning")
}

func TestResolveSharePath_RejectsWrongAlias(t *testing.T) {
	_, err := ResolveSharePath("/srv/docs", "/other/file", "docs")
	assert.Error(t, err, "alias mismatch must error")
}

func TestResolveSharePath_RootShareRoot(t *testing.T) {
	// shareRoot "/" used to fail the strings.HasPrefix(joined, "//") check
	// for every legitimate child. It now behaves correctly via filepath.Rel.
	got, err := ResolveSharePath("/", "/root/a.txt", "root")
	assert.NoError(t, err)
	assert.Equal(t, "/a.txt", got)
}
