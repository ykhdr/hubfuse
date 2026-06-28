package config

import "fmt"

// findShare returns a pointer to the share with the given alias, or an error if
// no such share exists. The pointer indexes into c.Shares so callers can mutate
// the live element (ranging over c.Shares yields copies, which would silently
// drop changes).
func (c *Config) findShare(alias string) (*ShareConfig, error) {
	for i := range c.Shares {
		if c.Shares[i].Alias == alias {
			return &c.Shares[i], nil
		}
	}
	return nil, fmt.Errorf("share %q not found", alias)
}

// AllowDevices appends the given device tokens to the named share's
// AllowedDevices list, skipping any that are already present (dedupe against the
// existing list and within the input). Existing order is preserved and new
// tokens are appended in input order. Matching is case-sensitive, consistent
// with the ACL layer (ShareACL.Decide). The reserved token "all" is treated as
// an ordinary token here; the ACL layer lifts it to AllowAll at load time.
//
// It returns the tokens that were newly added (empty when every token was
// already present, i.e. a no-op), or an error if the alias is unknown.
func (c *Config) AllowDevices(alias string, devices []string) (added []string, err error) {
	share, err := c.findShare(alias)
	if err != nil {
		return nil, err
	}

	present := make(map[string]struct{}, len(share.AllowedDevices))
	for _, d := range share.AllowedDevices {
		present[d] = struct{}{}
	}

	for _, d := range devices {
		if _, ok := present[d]; ok {
			continue
		}
		present[d] = struct{}{}
		share.AllowedDevices = append(share.AllowedDevices, d)
		added = append(added, d)
	}
	return added, nil
}

// DenyDevices removes the given device tokens from the named share's
// AllowedDevices list. Matching is case-sensitive. Removing the last token is
// allowed and leaves an empty list (no devices allowed). The reserved token
// "all" is removed like any other token.
//
// It returns the tokens that were removed and, separately, the requested tokens
// that were not present in the list (deduplicated, so a repeated request warns
// once), or an error if the alias is unknown.
func (c *Config) DenyDevices(alias string, devices []string) (removed, notFound []string, err error) {
	share, err := c.findShare(alias)
	if err != nil {
		return nil, nil, err
	}

	want := make(map[string]struct{}, len(devices))
	for _, d := range devices {
		want[d] = struct{}{}
	}

	// Drop every occurrence of a requested token (a hand-edited config may list
	// the same token twice), recording which tokens were actually found.
	found := make(map[string]struct{}, len(want))
	kept := share.AllowedDevices[:0]
	for _, d := range share.AllowedDevices {
		if _, drop := want[d]; drop {
			removed = append(removed, d)
			found[d] = struct{}{}
			continue
		}
		kept = append(kept, d)
	}
	share.AllowedDevices = kept

	// Requested tokens never present in the list are reported missing, in input
	// order and deduplicated.
	seen := make(map[string]struct{}, len(want))
	for _, d := range devices {
		if _, ok := found[d]; ok {
			continue
		}
		if _, dup := seen[d]; dup {
			continue
		}
		seen[d] = struct{}{}
		notFound = append(notFound, d)
	}
	return removed, notFound, nil
}

// AllowsAll reports whether the named share's list contains the reserved "all"
// token. Returns false (no error) if the alias is unknown.
func (c *Config) AllowsAll(alias string) bool {
	share, err := c.findShare(alias)
	if err != nil {
		return false
	}
	for _, d := range share.AllowedDevices {
		if d == "all" {
			return true
		}
	}
	return false
}
