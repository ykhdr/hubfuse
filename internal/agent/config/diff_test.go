package config

import (
	"testing"
)

// ─── ComputeDiff ──────────────────────────────────────────────────────────────

func TestComputeDiff_NilOldAndNew(t *testing.T) {
	diff := ComputeDiff(nil, nil)
	if diff.SharesChanged {
		t.Error("SharesChanged should be false when both configs are nil")
	}
	if len(diff.MountsAdded) != 0 {
		t.Errorf("MountsAdded = %v, want empty", diff.MountsAdded)
	}
	if len(diff.MountsRemoved) != 0 {
		t.Errorf("MountsRemoved = %v, want empty", diff.MountsRemoved)
	}
}

func TestComputeDiff_NilOld(t *testing.T) {
	newCfg := &Config{
		Shares: []ShareConfig{
			{Path: "/data", Alias: "data", Permissions: "rw"},
		},
		Mounts: []MountConfig{
			{Device: "desktop", Share: "projects", To: "/mnt/projects"},
		},
	}

	diff := ComputeDiff(nil, newCfg)
	if !diff.SharesChanged {
		t.Error("SharesChanged should be true when old is nil and new has shares")
	}
	if len(diff.MountsAdded) != 1 {
		t.Fatalf("len(MountsAdded) = %d, want 1", len(diff.MountsAdded))
	}
	if diff.MountsAdded[0].Device != "desktop" {
		t.Errorf("MountsAdded[0].Device = %q, want \"desktop\"", diff.MountsAdded[0].Device)
	}
	if len(diff.MountsRemoved) != 0 {
		t.Errorf("MountsRemoved = %v, want empty", diff.MountsRemoved)
	}
}

func TestComputeDiff_NilNew(t *testing.T) {
	oldCfg := &Config{
		Shares: []ShareConfig{
			{Path: "/data", Alias: "data", Permissions: "rw"},
		},
		Mounts: []MountConfig{
			{Device: "desktop", Share: "projects", To: "/mnt/projects"},
		},
	}

	diff := ComputeDiff(oldCfg, nil)
	if !diff.SharesChanged {
		t.Error("SharesChanged should be true when new is nil and old has shares")
	}
	if len(diff.MountsRemoved) != 1 {
		t.Fatalf("len(MountsRemoved) = %d, want 1", len(diff.MountsRemoved))
	}
	if diff.MountsRemoved[0].Device != "desktop" {
		t.Errorf("MountsRemoved[0].Device = %q, want \"desktop\"", diff.MountsRemoved[0].Device)
	}
	if len(diff.MountsAdded) != 0 {
		t.Errorf("MountsAdded = %v, want empty", diff.MountsAdded)
	}
}

func TestComputeDiff_Identical(t *testing.T) {
	cfg := &Config{
		Shares: []ShareConfig{
			{Path: "/data", Alias: "data", Permissions: "rw", AllowedDevices: []string{"device-a"}},
		},
		Mounts: []MountConfig{
			{Device: "desktop", Share: "projects", To: "/mnt/projects"},
		},
	}

	diff := ComputeDiff(cfg, cfg)
	if diff.SharesChanged {
		t.Error("SharesChanged should be false for identical configs")
	}
	if len(diff.MountsAdded) != 0 {
		t.Errorf("MountsAdded = %v, want empty", diff.MountsAdded)
	}
	if len(diff.MountsRemoved) != 0 {
		t.Errorf("MountsRemoved = %v, want empty", diff.MountsRemoved)
	}
}

func TestComputeDiff_SharesChanged(t *testing.T) {
	oldCfg := &Config{
		Shares: []ShareConfig{
			{Path: "/data", Alias: "data", Permissions: "rw"},
		},
	}
	newCfg := &Config{
		Shares: []ShareConfig{
			{Path: "/data", Alias: "data", Permissions: "ro"}, // permission changed
		},
	}

	diff := ComputeDiff(oldCfg, newCfg)
	if !diff.SharesChanged {
		t.Error("SharesChanged should be true when permissions changed")
	}
}

func TestComputeDiff_SharesAddedItem(t *testing.T) {
	oldCfg := &Config{
		Shares: []ShareConfig{
			{Path: "/data", Alias: "data", Permissions: "rw"},
		},
	}
	newCfg := &Config{
		Shares: []ShareConfig{
			{Path: "/data", Alias: "data", Permissions: "rw"},
			{Path: "/photos", Alias: "photos", Permissions: "ro"},
		},
	}

	diff := ComputeDiff(oldCfg, newCfg)
	if !diff.SharesChanged {
		t.Error("SharesChanged should be true when a share is added")
	}
}

func TestComputeDiff_MountsAdded(t *testing.T) {
	oldCfg := &Config{
		Mounts: []MountConfig{
			{Device: "desktop", Share: "projects", To: "/mnt/projects"},
		},
	}
	newCfg := &Config{
		Mounts: []MountConfig{
			{Device: "desktop", Share: "projects", To: "/mnt/projects"},
			{Device: "nas", Share: "media", To: "/mnt/media"},
		},
	}

	diff := ComputeDiff(oldCfg, newCfg)
	if len(diff.MountsAdded) != 1 {
		t.Fatalf("len(MountsAdded) = %d, want 1", len(diff.MountsAdded))
	}
	if diff.MountsAdded[0].Device != "nas" {
		t.Errorf("MountsAdded[0].Device = %q, want \"nas\"", diff.MountsAdded[0].Device)
	}
	if len(diff.MountsRemoved) != 0 {
		t.Errorf("MountsRemoved = %v, want empty", diff.MountsRemoved)
	}
}

func TestComputeDiff_MountsRemoved(t *testing.T) {
	oldCfg := &Config{
		Mounts: []MountConfig{
			{Device: "desktop", Share: "projects", To: "/mnt/projects"},
			{Device: "nas", Share: "media", To: "/mnt/media"},
		},
	}
	newCfg := &Config{
		Mounts: []MountConfig{
			{Device: "desktop", Share: "projects", To: "/mnt/projects"},
		},
	}

	diff := ComputeDiff(oldCfg, newCfg)
	if len(diff.MountsRemoved) != 1 {
		t.Fatalf("len(MountsRemoved) = %d, want 1", len(diff.MountsRemoved))
	}
	if diff.MountsRemoved[0].Device != "nas" {
		t.Errorf("MountsRemoved[0].Device = %q, want \"nas\"", diff.MountsRemoved[0].Device)
	}
	if len(diff.MountsAdded) != 0 {
		t.Errorf("MountsAdded = %v, want empty", diff.MountsAdded)
	}
}

func TestComputeDiff_MountsAddedAndRemoved(t *testing.T) {
	oldCfg := &Config{
		Mounts: []MountConfig{
			{Device: "desktop", Share: "projects", To: "/mnt/projects"},
			{Device: "nas", Share: "media", To: "/mnt/media"},
		},
	}
	newCfg := &Config{
		Mounts: []MountConfig{
			{Device: "desktop", Share: "projects", To: "/mnt/projects"},
			{Device: "tablet", Share: "music", To: "/mnt/music"},
		},
	}

	diff := ComputeDiff(oldCfg, newCfg)
	if len(diff.MountsAdded) != 1 {
		t.Fatalf("len(MountsAdded) = %d, want 1", len(diff.MountsAdded))
	}
	if diff.MountsAdded[0].Device != "tablet" {
		t.Errorf("MountsAdded[0].Device = %q, want \"tablet\"", diff.MountsAdded[0].Device)
	}
	if len(diff.MountsRemoved) != 1 {
		t.Fatalf("len(MountsRemoved) = %d, want 1", len(diff.MountsRemoved))
	}
	if diff.MountsRemoved[0].Device != "nas" {
		t.Errorf("MountsRemoved[0].Device = %q, want \"nas\"", diff.MountsRemoved[0].Device)
	}
}

func TestComputeDiff_AllowedDevicesChanged(t *testing.T) {
	oldCfg := &Config{
		Shares: []ShareConfig{
			{Path: "/data", Alias: "data", Permissions: "rw", AllowedDevices: []string{"device-a"}},
		},
	}
	newCfg := &Config{
		Shares: []ShareConfig{
			{Path: "/data", Alias: "data", Permissions: "rw", AllowedDevices: []string{"device-a", "device-b"}},
		},
	}

	diff := ComputeDiff(oldCfg, newCfg)
	if !diff.SharesChanged {
		t.Error("SharesChanged should be true when AllowedDevices changed")
	}
}

func TestComputeDiff_SharesOrderIndependent(t *testing.T) {
	// Two configs with the same shares in different order must NOT report
	// SharesChanged — the comparison must be order-insensitive.
	cfgA := &Config{
		Shares: []ShareConfig{
			{Path: "/data", Alias: "data", Permissions: "rw"},
			{Path: "/photos", Alias: "photos", Permissions: "ro"},
		},
	}
	cfgB := &Config{
		Shares: []ShareConfig{
			{Path: "/photos", Alias: "photos", Permissions: "ro"},
			{Path: "/data", Alias: "data", Permissions: "rw"},
		},
	}

	diff := ComputeDiff(cfgA, cfgB)
	if diff.SharesChanged {
		t.Error("SharesChanged should be false when shares are identical but in different order")
	}
}

func TestComputeDiff_MountKeyDistinctByShareAndDevice(t *testing.T) {
	// Same device, different shares — both should appear as separate entries.
	oldCfg := &Config{
		Mounts: []MountConfig{
			{Device: "desktop", Share: "projects", To: "/mnt/projects"},
		},
	}
	newCfg := &Config{
		Mounts: []MountConfig{
			{Device: "desktop", Share: "docs", To: "/mnt/docs"},
		},
	}

	diff := ComputeDiff(oldCfg, newCfg)
	if len(diff.MountsAdded) != 1 {
		t.Fatalf("len(MountsAdded) = %d, want 1", len(diff.MountsAdded))
	}
	if diff.MountsAdded[0].Share != "docs" {
		t.Errorf("MountsAdded[0].Share = %q, want \"docs\"", diff.MountsAdded[0].Share)
	}
	if len(diff.MountsRemoved) != 1 {
		t.Fatalf("len(MountsRemoved) = %d, want 1", len(diff.MountsRemoved))
	}
	if diff.MountsRemoved[0].Share != "projects" {
		t.Errorf("MountsRemoved[0].Share = %q, want \"projects\"", diff.MountsRemoved[0].Share)
	}
}
