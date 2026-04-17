package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─── NormalizePermissions ─────────────────────────────────────────────────────

func TestNormalizePermissions(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"ro", "ro"},
		{"rw", "rw"},
		{"read-only", "ro"},
		{"read-write", "rw"},
		{"", ""},
		{"other", "other"},
	}

	for _, tc := range tests {
		got := NormalizePermissions(tc.input)
		assert.Equal(t, tc.want, got, "NormalizePermissions(%q)", tc.input)
	}
}

// ─── ExpandTilde ──────────────────────────────────────────────────────────────

func TestExpandTilde_WithTilde(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home directory:", err)
	}

	got := ExpandTilde("~/foo/bar")
	want := filepath.Join(home, "foo/bar")
	assert.Equal(t, want, got)
}

func TestExpandTilde_NoTilde(t *testing.T) {
	path := "/absolute/path"
	got := ExpandTilde(path)
	assert.Equal(t, path, got)
}

func TestExpandTilde_RelativePath(t *testing.T) {
	path := "relative/path"
	got := ExpandTilde(path)
	assert.Equal(t, path, got)
}

func TestExpandTilde_TildeOnly(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home directory:", err)
	}

	got := ExpandTilde("~")
	assert.Equal(t, home, got)
}

// ─── DefaultConfig ────────────────────────────────────────────────────────────

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	require.NotNil(t, cfg)
	assert.Equal(t, 2222, cfg.Agent.SSHPort)
}

// ─── Load ─────────────────────────────────────────────────────────────────────

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.kdl")
	err := os.WriteFile(path, []byte(content), 0644)
	require.NoError(t, err, "write temp config")
	return path
}

func TestLoad_BasicDevice(t *testing.T) {
	path := writeTemp(t, `
device {
    nickname "laptop"
}
`)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "laptop", cfg.Device.Nickname)
}

func TestLoad_HubAddress(t *testing.T) {
	path := writeTemp(t, `
hub {
    address "192.168.1.100:9090"
}
`)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, "192.168.1.100:9090", cfg.Hub.Address)
}

func TestLoad_AgentSSHPort(t *testing.T) {
	path := writeTemp(t, `
agent {
    ssh-port 2222
}
`)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, 2222, cfg.Agent.SSHPort)
}

func TestLoad_DefaultSSHPort(t *testing.T) {
	// When ssh-port is not set, it should default to 2222.
	path := writeTemp(t, `
device {
    nickname "x"
}
`)
	cfg, err := Load(path)
	require.NoError(t, err)
	assert.Equal(t, 2222, cfg.Agent.SSHPort)
}

func TestLoad_NormalizePermissions(t *testing.T) {
	path := writeTemp(t, `
shares {
    share "/home/user/photos" alias="photos" permissions="read-only" {
        allowed-devices "all"
    }
    share "/home/user/docs" alias="docs" permissions="read-write" {
        allowed-devices "desktop"
    }
}
`)
	cfg, err := Load(path)
	require.NoError(t, err)
	require.Len(t, cfg.Shares, 2)
	assert.Equal(t, "ro", cfg.Shares[0].Permissions)
	assert.Equal(t, "rw", cfg.Shares[1].Permissions)
}

func TestLoad_TildeExpansion(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home directory:", err)
	}

	path := writeTemp(t, `
mounts {
    mount device="desktop" share="projects" to="~/remote/desktop"
}
`)
	cfg, err := Load(path)
	require.NoError(t, err)
	require.Len(t, cfg.Mounts, 1)
	want := filepath.Join(home, "remote/desktop")
	assert.Equal(t, want, cfg.Mounts[0].To)
}

func TestLoad_Mounts(t *testing.T) {
	path := writeTemp(t, `
mounts {
    mount device="desktop" share="projects" to="/mnt/projects"
    mount device="nas" share="media" to="/mnt/media"
}
`)
	cfg, err := Load(path)
	require.NoError(t, err)
	require.Len(t, cfg.Mounts, 2)
	assert.Equal(t, "desktop", cfg.Mounts[0].Device)
	assert.Equal(t, "media", cfg.Mounts[1].Share)
}

func TestLoad_NonExistentFile(t *testing.T) {
	_, err := Load("/does/not/exist/config.kdl")
	assert.Error(t, err)
}

func TestLoad_FullConfig(t *testing.T) {
	path := writeTemp(t, `
device {
    nickname "laptop"
}

hub {
    address "192.168.1.100:9090"
}

agent {
    ssh-port 2222
}

shares {
    share "/home/user/projects" alias="projects" permissions="rw" {
        allowed-devices "desktop" "tablet"
    }
    share "/home/user/photos" alias="photos" permissions="ro" {
        allowed-devices "all"
    }
}

mounts {
    mount device="desktop" share="projects" to="/mnt/desktop-projects"
    mount device="nas" share="media" to="/mnt/media"
}
`)
	cfg, err := Load(path)
	require.NoError(t, err)

	assert.Equal(t, "laptop", cfg.Device.Nickname)
	assert.Equal(t, "192.168.1.100:9090", cfg.Hub.Address)
	assert.Equal(t, 2222, cfg.Agent.SSHPort)

	require.Len(t, cfg.Shares, 2)
	assert.Equal(t, "/home/user/projects", cfg.Shares[0].Path)
	assert.Equal(t, "projects", cfg.Shares[0].Alias)
	assert.Equal(t, "rw", cfg.Shares[0].Permissions)
	assert.Len(t, cfg.Shares[0].AllowedDevices, 2)

	require.Len(t, cfg.Mounts, 2)
	assert.Equal(t, "desktop", cfg.Mounts[0].Device)
	assert.Equal(t, "projects", cfg.Mounts[0].Share)
}

func TestLoad_AllowedDevices(t *testing.T) {
	path := writeTemp(t, `
shares {
    share "/data" alias="data" permissions="rw" {
        allowed-devices "device-a" "device-b" "device-c"
    }
}
`)
	cfg, err := Load(path)
	require.NoError(t, err)
	require.Len(t, cfg.Shares, 1)
	assert.Equal(t, []string{"device-a", "device-b", "device-c"}, cfg.Shares[0].AllowedDevices)
}

func TestLoad_InvalidKDL(t *testing.T) {
	path := writeTemp(t, `this is not { valid kdl syntax !!!`)
	_, err := Load(path)
	assert.Error(t, err)
}

