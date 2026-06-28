package config

import (
	"path/filepath"
	"reflect"
	"testing"
)

func cfgWithShare(allowed ...string) *Config {
	return &Config{
		Shares: []ShareConfig{
			{Path: "/data", Alias: "docs", Permissions: "ro", AllowedDevices: append([]string(nil), allowed...)},
		},
	}
}

func TestAllowDevices(t *testing.T) {
	tests := []struct {
		name      string
		initial   []string
		add       []string
		wantList  []string
		wantAdded []string
	}{
		{"add to empty", nil, []string{"a", "b"}, []string{"a", "b"}, []string{"a", "b"}},
		{"dedupe against existing", []string{"a"}, []string{"a", "b"}, []string{"a", "b"}, []string{"b"}},
		{"dedupe within input", nil, []string{"a", "a", "b"}, []string{"a", "b"}, []string{"a", "b"}},
		{"all already present is no-op", []string{"a", "b"}, []string{"a", "b"}, []string{"a", "b"}, nil},
		{"preserves order", []string{"x"}, []string{"y", "z"}, []string{"x", "y", "z"}, []string{"y", "z"}},
		{"all token is ordinary", []string{"a"}, []string{"all"}, []string{"a", "all"}, []string{"all"}},
		{"case sensitive", []string{"dev1"}, []string{"Dev1"}, []string{"dev1", "Dev1"}, []string{"Dev1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := cfgWithShare(tt.initial...)
			added, err := cfg.AllowDevices("docs", tt.add)
			if err != nil {
				t.Fatalf("AllowDevices: unexpected error: %v", err)
			}
			if !reflect.DeepEqual(added, tt.wantAdded) {
				t.Errorf("added = %v, want %v", added, tt.wantAdded)
			}
			if !reflect.DeepEqual(cfg.Shares[0].AllowedDevices, tt.wantList) {
				t.Errorf("AllowedDevices = %v, want %v", cfg.Shares[0].AllowedDevices, tt.wantList)
			}
		})
	}
}

func TestAllowDevices_UnknownAlias(t *testing.T) {
	cfg := cfgWithShare("a")
	if _, err := cfg.AllowDevices("nope", []string{"b"}); err == nil {
		t.Fatal("expected error for unknown alias, got nil")
	}
}

func TestDenyDevices(t *testing.T) {
	tests := []struct {
		name         string
		initial      []string
		deny         []string
		wantList     []string
		wantRemoved  []string
		wantNotFound []string
	}{
		{"remove one", []string{"a", "b"}, []string{"a"}, []string{"b"}, []string{"a"}, nil},
		{"remove all empties list", []string{"a", "b"}, []string{"a", "b"}, []string{}, []string{"a", "b"}, nil},
		{"not found warns", []string{"a"}, []string{"ghost"}, []string{"a"}, nil, []string{"ghost"}},
		{"not found deduped", []string{"a"}, []string{"ghost", "ghost"}, []string{"a"}, nil, []string{"ghost"}},
		{"mixed found and not", []string{"a", "b"}, []string{"a", "ghost"}, []string{"b"}, []string{"a"}, []string{"ghost"}},
		{"all token removable", []string{"all", "a"}, []string{"all"}, []string{"a"}, []string{"all"}, nil},
		{"removes duplicate stored tokens", []string{"a", "a", "b"}, []string{"a"}, []string{"b"}, []string{"a", "a"}, nil},
		{"case sensitive miss", []string{"dev1"}, []string{"Dev1"}, []string{"dev1"}, nil, []string{"Dev1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := cfgWithShare(tt.initial...)
			removed, notFound, err := cfg.DenyDevices("docs", tt.deny)
			if err != nil {
				t.Fatalf("DenyDevices: unexpected error: %v", err)
			}
			if !reflect.DeepEqual(removed, tt.wantRemoved) {
				t.Errorf("removed = %v, want %v", removed, tt.wantRemoved)
			}
			if !reflect.DeepEqual(notFound, tt.wantNotFound) {
				t.Errorf("notFound = %v, want %v", notFound, tt.wantNotFound)
			}
			if !reflect.DeepEqual(cfg.Shares[0].AllowedDevices, tt.wantList) {
				t.Errorf("AllowedDevices = %v, want %v", cfg.Shares[0].AllowedDevices, tt.wantList)
			}
		})
	}
}

func TestDenyDevices_UnknownAlias(t *testing.T) {
	cfg := cfgWithShare("a")
	if _, _, err := cfg.DenyDevices("nope", []string{"a"}); err == nil {
		t.Fatal("expected error for unknown alias, got nil")
	}
}

func TestAllowsAll(t *testing.T) {
	if !cfgWithShare("a", "all").AllowsAll("docs") {
		t.Error("AllowsAll = false, want true when list contains \"all\"")
	}
	if cfgWithShare("a", "b").AllowsAll("docs") {
		t.Error("AllowsAll = true, want false when list lacks \"all\"")
	}
	if cfgWithShare("all").AllowsAll("nope") {
		t.Error("AllowsAll = true for unknown alias, want false")
	}
}

// TestAllowDenyRoundTrip exercises the full Save -> Load cycle to confirm the
// on-disk allowed-devices node reflects mutations, and that the node disappears
// once the list is emptied (Save omits it for an empty list).
func TestAllowDenyRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.kdl")

	cfg := &Config{
		Device: DeviceConfig{Nickname: "me"},
		Hub:    HubConfig{Address: "hub:9443"},
		Agent:  AgentConfig{SSHPort: 2222, MountTool: "sshfs"},
		Shares: []ShareConfig{{Path: "/data", Alias: "docs", Permissions: "ro"}},
	}

	// allow -> save -> load
	if _, err := cfg.AllowDevices("docs", []string{"laptop", "phone"}); err != nil {
		t.Fatalf("AllowDevices: %v", err)
	}
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := reloaded.Shares[0].AllowedDevices; !reflect.DeepEqual(got, []string{"laptop", "phone"}) {
		t.Fatalf("after allow round-trip = %v, want [laptop phone]", got)
	}

	// deny everything -> save -> load: the allowed-devices node must be gone.
	if _, _, err := reloaded.DenyDevices("docs", []string{"laptop", "phone"}); err != nil {
		t.Fatalf("DenyDevices: %v", err)
	}
	if err := Save(path, reloaded); err != nil {
		t.Fatalf("Save: %v", err)
	}
	final, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := final.Shares[0].AllowedDevices; len(got) != 0 {
		t.Fatalf("after deny round-trip = %v, want empty", got)
	}
}
