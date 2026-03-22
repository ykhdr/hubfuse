package config

import (
	"os"
	"path/filepath"
	"testing"
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
		if got != tc.want {
			t.Errorf("NormalizePermissions(%q) = %q, want %q", tc.input, got, tc.want)
		}
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
	if got != want {
		t.Errorf("ExpandTilde(~/foo/bar) = %q, want %q", got, want)
	}
}

func TestExpandTilde_NoTilde(t *testing.T) {
	path := "/absolute/path"
	got := ExpandTilde(path)
	if got != path {
		t.Errorf("ExpandTilde(%q) = %q, want unchanged", path, got)
	}
}

func TestExpandTilde_RelativePath(t *testing.T) {
	path := "relative/path"
	got := ExpandTilde(path)
	if got != path {
		t.Errorf("ExpandTilde(%q) = %q, want unchanged", path, got)
	}
}

func TestExpandTilde_TildeOnly(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home directory:", err)
	}

	got := ExpandTilde("~")
	if got != home {
		t.Errorf("ExpandTilde(~) = %q, want %q", got, home)
	}
}

// ─── DefaultConfig ────────────────────────────────────────────────────────────

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg == nil {
		t.Fatal("DefaultConfig() returned nil")
	}
	if cfg.Agent.SSHPort != 2222 {
		t.Errorf("default SSHPort = %d, want 2222", cfg.Agent.SSHPort)
	}
}

// ─── Load ─────────────────────────────────────────────────────────────────────

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.kdl")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("write temp config: %v", err)
	}
	return path
}

func TestLoad_BasicDevice(t *testing.T) {
	path := writeTemp(t, `
device {
    nickname "laptop"
}
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.Device.Nickname != "laptop" {
		t.Errorf("Nickname = %q, want %q", cfg.Device.Nickname, "laptop")
	}
}

func TestLoad_HubAddress(t *testing.T) {
	path := writeTemp(t, `
hub {
    address "192.168.1.100:9090"
}
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.Hub.Address != "192.168.1.100:9090" {
		t.Errorf("Address = %q, want %q", cfg.Hub.Address, "192.168.1.100:9090")
	}
}

func TestLoad_AgentSSHPort(t *testing.T) {
	path := writeTemp(t, `
agent {
    ssh-port 2222
}
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.Agent.SSHPort != 2222 {
		t.Errorf("SSHPort = %d, want 2222", cfg.Agent.SSHPort)
	}
}

func TestLoad_DefaultSSHPort(t *testing.T) {
	// When ssh-port is not set, it should default to 2222.
	path := writeTemp(t, `
device {
    nickname "x"
}
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.Agent.SSHPort != 2222 {
		t.Errorf("default SSHPort = %d, want 2222", cfg.Agent.SSHPort)
	}
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
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if len(cfg.Shares) != 2 {
		t.Fatalf("len(Shares) = %d, want 2", len(cfg.Shares))
	}
	if cfg.Shares[0].Permissions != "ro" {
		t.Errorf("Shares[0].Permissions = %q, want \"ro\"", cfg.Shares[0].Permissions)
	}
	if cfg.Shares[1].Permissions != "rw" {
		t.Errorf("Shares[1].Permissions = %q, want \"rw\"", cfg.Shares[1].Permissions)
	}
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
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if len(cfg.Mounts) != 1 {
		t.Fatalf("len(Mounts) = %d, want 1", len(cfg.Mounts))
	}
	want := filepath.Join(home, "remote/desktop")
	if cfg.Mounts[0].To != want {
		t.Errorf("Mounts[0].To = %q, want %q", cfg.Mounts[0].To, want)
	}
}

func TestLoad_Mounts(t *testing.T) {
	path := writeTemp(t, `
mounts {
    mount device="desktop" share="projects" to="/mnt/projects"
    mount device="nas" share="media" to="/mnt/media"
}
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if len(cfg.Mounts) != 2 {
		t.Fatalf("len(Mounts) = %d, want 2", len(cfg.Mounts))
	}
	if cfg.Mounts[0].Device != "desktop" {
		t.Errorf("Mounts[0].Device = %q, want \"desktop\"", cfg.Mounts[0].Device)
	}
	if cfg.Mounts[1].Share != "media" {
		t.Errorf("Mounts[1].Share = %q, want \"media\"", cfg.Mounts[1].Share)
	}
}

func TestLoad_NonExistentFile(t *testing.T) {
	_, err := Load("/does/not/exist/config.kdl")
	if err == nil {
		t.Fatal("expected error for non-existent file, got nil")
	}
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
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}

	if cfg.Device.Nickname != "laptop" {
		t.Errorf("Nickname = %q, want \"laptop\"", cfg.Device.Nickname)
	}
	if cfg.Hub.Address != "192.168.1.100:9090" {
		t.Errorf("Hub.Address = %q, want \"192.168.1.100:9090\"", cfg.Hub.Address)
	}
	if cfg.Agent.SSHPort != 2222 {
		t.Errorf("Agent.SSHPort = %d, want 2222", cfg.Agent.SSHPort)
	}

	if len(cfg.Shares) != 2 {
		t.Fatalf("len(Shares) = %d, want 2", len(cfg.Shares))
	}
	if cfg.Shares[0].Path != "/home/user/projects" {
		t.Errorf("Shares[0].Path = %q, want \"/home/user/projects\"", cfg.Shares[0].Path)
	}
	if cfg.Shares[0].Alias != "projects" {
		t.Errorf("Shares[0].Alias = %q, want \"projects\"", cfg.Shares[0].Alias)
	}
	if cfg.Shares[0].Permissions != "rw" {
		t.Errorf("Shares[0].Permissions = %q, want \"rw\"", cfg.Shares[0].Permissions)
	}
	if len(cfg.Shares[0].AllowedDevices) != 2 {
		t.Errorf("len(Shares[0].AllowedDevices) = %d, want 2", len(cfg.Shares[0].AllowedDevices))
	}

	if len(cfg.Mounts) != 2 {
		t.Fatalf("len(Mounts) = %d, want 2", len(cfg.Mounts))
	}
	if cfg.Mounts[0].Device != "desktop" {
		t.Errorf("Mounts[0].Device = %q, want \"desktop\"", cfg.Mounts[0].Device)
	}
	if cfg.Mounts[0].Share != "projects" {
		t.Errorf("Mounts[0].Share = %q, want \"projects\"", cfg.Mounts[0].Share)
	}
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
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if len(cfg.Shares) != 1 {
		t.Fatalf("len(Shares) = %d, want 1", len(cfg.Shares))
	}
	devices := cfg.Shares[0].AllowedDevices
	if len(devices) != 3 {
		t.Fatalf("len(AllowedDevices) = %d, want 3", len(devices))
	}
	want := []string{"device-a", "device-b", "device-c"}
	for i, w := range want {
		if devices[i] != w {
			t.Errorf("AllowedDevices[%d] = %q, want %q", i, devices[i], w)
		}
	}
}

func TestLoad_InvalidKDL(t *testing.T) {
	path := writeTemp(t, `this is not { valid kdl syntax !!!`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid KDL, got nil")
	}
}

