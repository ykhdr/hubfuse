package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─── ComputeDiff ──────────────────────────────────────────────────────────────

func TestComputeDiff_NilOldAndNew(t *testing.T) {
	diff := ComputeDiff(nil, nil)
	assert.False(t, diff.SharesChanged, "SharesChanged should be false when both configs are nil")
	assert.Empty(t, diff.MountsAdded)
	assert.Empty(t, diff.MountsRemoved)
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
	assert.True(t, diff.SharesChanged, "SharesChanged should be true when old is nil and new has shares")
	require.Len(t, diff.MountsAdded, 1)
	assert.Equal(t, "desktop", diff.MountsAdded[0].Device)
	assert.Empty(t, diff.MountsRemoved)
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
	assert.True(t, diff.SharesChanged, "SharesChanged should be true when new is nil and old has shares")
	require.Len(t, diff.MountsRemoved, 1)
	assert.Equal(t, "desktop", diff.MountsRemoved[0].Device)
	assert.Empty(t, diff.MountsAdded)
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
	assert.False(t, diff.SharesChanged, "SharesChanged should be false for identical configs")
	assert.Empty(t, diff.MountsAdded)
	assert.Empty(t, diff.MountsRemoved)
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
	assert.True(t, diff.SharesChanged, "SharesChanged should be true when permissions changed")
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
	assert.True(t, diff.SharesChanged, "SharesChanged should be true when a share is added")
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
	require.Len(t, diff.MountsAdded, 1)
	assert.Equal(t, "nas", diff.MountsAdded[0].Device)
	assert.Empty(t, diff.MountsRemoved)
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
	require.Len(t, diff.MountsRemoved, 1)
	assert.Equal(t, "nas", diff.MountsRemoved[0].Device)
	assert.Empty(t, diff.MountsAdded)
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
	require.Len(t, diff.MountsAdded, 1)
	assert.Equal(t, "tablet", diff.MountsAdded[0].Device)
	require.Len(t, diff.MountsRemoved, 1)
	assert.Equal(t, "nas", diff.MountsRemoved[0].Device)
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
	assert.True(t, diff.SharesChanged, "SharesChanged should be true when AllowedDevices changed")
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
	assert.False(t, diff.SharesChanged, "SharesChanged should be false when shares are identical but in different order")
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
	require.Len(t, diff.MountsAdded, 1)
	assert.Equal(t, "docs", diff.MountsAdded[0].Share)
	require.Len(t, diff.MountsRemoved, 1)
	assert.Equal(t, "projects", diff.MountsRemoved[0].Share)
}
