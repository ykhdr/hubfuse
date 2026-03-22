package config

import "sort"

// ConfigDiff summarises the differences between two Config values.
type ConfigDiff struct {
	SharesChanged bool
	MountsAdded   []MountConfig
	MountsRemoved []MountConfig
}

// ComputeDiff returns a ConfigDiff describing what changed between old and new.
// Either argument may be nil (treated as an empty config).
func ComputeDiff(old, new *Config) ConfigDiff {
	var diff ConfigDiff

	oldShares := sharesFrom(old)
	newShares := sharesFrom(new)
	diff.SharesChanged = !sharesEqual(oldShares, newShares)

	oldMounts := mountsFrom(old)
	newMounts := mountsFrom(new)
	diff.MountsAdded = mountsNotIn(newMounts, oldMounts)
	diff.MountsRemoved = mountsNotIn(oldMounts, newMounts)

	return diff
}

// sharesFrom returns the Shares slice for cfg, handling nil cfg.
func sharesFrom(cfg *Config) []ShareConfig {
	if cfg == nil {
		return nil
	}
	return cfg.Shares
}

// mountsFrom returns the Mounts slice for cfg, handling nil cfg.
func mountsFrom(cfg *Config) []MountConfig {
	if cfg == nil {
		return nil
	}
	return cfg.Mounts
}

// sharesEqual reports whether two ShareConfig slices are equivalent,
// regardless of the order in which the shares appear.
func sharesEqual(a, b []ShareConfig) bool {
	if len(a) != len(b) {
		return false
	}
	// Sort copies by Alias so comparison is order-independent.
	sortedA := sortedShares(a)
	sortedB := sortedShares(b)
	for i := range sortedA {
		if !shareEqual(sortedA[i], sortedB[i]) {
			return false
		}
	}
	return true
}

// sortedShares returns a copy of shares sorted by Alias.
func sortedShares(shares []ShareConfig) []ShareConfig {
	cp := make([]ShareConfig, len(shares))
	copy(cp, shares)
	sort.Slice(cp, func(i, j int) bool {
		return cp[i].Alias < cp[j].Alias
	})
	return cp
}

// shareEqual reports whether two ShareConfig values are equivalent.
func shareEqual(a, b ShareConfig) bool {
	if a.Path != b.Path || a.Alias != b.Alias || a.Permissions != b.Permissions {
		return false
	}
	if len(a.AllowedDevices) != len(b.AllowedDevices) {
		return false
	}
	for i := range a.AllowedDevices {
		if a.AllowedDevices[i] != b.AllowedDevices[i] {
			return false
		}
	}
	return true
}

// mountKey returns a string key identifying a mount by its (device, share) pair.
func mountKey(m MountConfig) string {
	return m.Device + "\x00" + m.Share
}

// mountsNotIn returns entries from a that are not present in b (by device+share key).
func mountsNotIn(a, b []MountConfig) []MountConfig {
	inB := make(map[string]struct{}, len(b))
	for _, m := range b {
		inB[mountKey(m)] = struct{}{}
	}

	var result []MountConfig
	for _, m := range a {
		if _, found := inB[mountKey(m)]; !found {
			result = append(result, m)
		}
	}
	return result
}
