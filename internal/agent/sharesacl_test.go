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
