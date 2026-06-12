package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLoadNicknames_AbsentFile(t *testing.T) {
	dir := t.TempDir()
	m, err := LoadNicknames(dir)
	require.NoError(t, err)
	assert.NotNil(t, m)
	assert.Empty(t, m)
}

func TestLoadNicknames_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	want := map[string]string{
		"dev-server": "server",
		"dev-laptop": "laptop",
	}
	require.NoError(t, SaveNicknames(dir, want))

	got, err := LoadNicknames(dir)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestLoadNicknames_InvalidDeviceIDRejected(t *testing.T) {
	dir := t.TempDir()
	bad := map[string]string{"../../etc": "evil"}
	data, err := json.Marshal(bad)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, nicknamesFile), data, 0644))

	_, err = LoadNicknames(dir)
	assert.Error(t, err, "invalid device_id key must be rejected on load")
}

func TestSaveNicknames_InvalidDeviceIDRejected(t *testing.T) {
	dir := t.TempDir()
	err := SaveNicknames(dir, map[string]string{"../escape": "bad"})
	assert.Error(t, err, "SaveNicknames must reject invalid device_id keys")
}

func TestSaveNicknames_AtomicOverwrite(t *testing.T) {
	dir := t.TempDir()
	initial := map[string]string{"dev-a": "alice"}
	require.NoError(t, SaveNicknames(dir, initial))

	updated := map[string]string{"dev-a": "alice", "dev-b": "bob"}
	require.NoError(t, SaveNicknames(dir, updated))

	got, err := LoadNicknames(dir)
	require.NoError(t, err)
	assert.Equal(t, updated, got)

	// No temp files should be left behind.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, filepath.Ext(e.Name()) == ".tmp", "temp file left behind: %s", e.Name())
	}
}

func TestSaveNicknames_Durability(t *testing.T) {
	dir := t.TempDir()
	want := map[string]string{"dev-x": "xenon"}
	require.NoError(t, SaveNicknames(dir, want))

	// Read the raw file to make sure it exists and parses.
	data, err := os.ReadFile(filepath.Join(dir, nicknamesFile))
	require.NoError(t, err)

	var parsed map[string]string
	require.NoError(t, json.Unmarshal(data, &parsed))
	assert.Equal(t, want, parsed)
}

func TestSetNickname_NoopOnEmpty(t *testing.T) {
	dir := t.TempDir()
	// An empty nickname must not create the file.
	require.NoError(t, SetNickname(dir, "dev-x", ""))
	_, err := os.Stat(filepath.Join(dir, nicknamesFile))
	assert.True(t, os.IsNotExist(err), "nicknames.json must not be created for empty nickname")
}

func TestSetNickname_PersistsNickname(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, SetNickname(dir, "dev-server", "server"))

	m, err := LoadNicknames(dir)
	require.NoError(t, err)
	assert.Equal(t, "server", m["dev-server"])
}

func TestSetNickname_InvalidDeviceID(t *testing.T) {
	dir := t.TempDir()
	err := SetNickname(dir, "../escape", "evil")
	assert.Error(t, err)
}

// TestNicknameForDeviceID_OfflineFallback proves the race is closed: a Daemon
// with empty onlineDevices but a populated nicknames map resolves the peer
// correctly without any network interaction.
func TestNicknameForDeviceID_OfflineFallback(t *testing.T) {
	d := &Daemon{
		onlineDevices: make(map[string]*OnlineDevice),
		nicknames:     map[string]string{"dev-server": "server"},
		logger:        discardLogger(),
	}

	nick, ok := d.NicknameForDeviceID("dev-server")
	assert.True(t, ok, "must resolve from persisted nicknames when peer is offline")
	assert.Equal(t, "server", nick)

	_, ok2 := d.NicknameForDeviceID("unknown")
	assert.False(t, ok2, "unknown device_id must return ok=false")
}

// TestNicknameForDeviceID_LiveWinsOverCache verifies that onlineDevices takes
// priority over the persisted cache (in case the peer has since been renamed).
func TestNicknameForDeviceID_LiveWinsOverCache(t *testing.T) {
	d := &Daemon{
		onlineDevices: map[string]*OnlineDevice{
			"dev-server": {DeviceID: "dev-server", Nickname: "server-new"},
		},
		nicknames: map[string]string{"dev-server": "server-old"},
		logger:    discardLogger(),
	}

	nick, ok := d.NicknameForDeviceID("dev-server")
	assert.True(t, ok)
	assert.Equal(t, "server-new", nick, "live onlineDevices must take priority over persisted cache")
}

// TestRememberNickname_ConcurrentSafety exercises rememberNickname from
// multiple goroutines under the -race detector.  It verifies that the final
// persisted file is valid JSON and that no updates are silently lost for
// distinct device IDs.
func TestRememberNickname_ConcurrentSafety(t *testing.T) {
	base := t.TempDir()
	kd := filepath.Join(base, "known_devices")
	require.NoError(t, os.MkdirAll(kd, 0700))

	d := &Daemon{
		nicknames:     make(map[string]string),
		logger:        discardLogger(),
		dataDir:       base,
		onlineDevices: make(map[string]*OnlineDevice),
	}

	const n = 20
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("dev-%02d", i)
			d.rememberNickname(id, fmt.Sprintf("nick-%02d", i))
		}(i)
	}
	wg.Wait()

	// All n distinct entries must be present in memory.
	d.mu.RLock()
	inMem := len(d.nicknames)
	d.mu.RUnlock()
	assert.Equal(t, n, inMem, "all %d nicknames must be in memory after concurrent rememberNickname", n)

	// The persisted file must parse cleanly.
	got, err := LoadNicknames(kd)
	require.NoError(t, err)
	// At least 1 entry must have been flushed (best-effort — the last flush wins).
	assert.NotEmpty(t, got, "at least one nickname must have been flushed to disk")
}
