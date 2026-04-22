package agent

// DeviceResolver maps a peer device_id to its hub-advertised nickname.
// Returns ok=false if the mapping is not yet known (e.g. right after pairing,
// before the hub has emitted a DeviceOnline event).
type DeviceResolver interface {
	NicknameForDeviceID(id string) (string, bool)
}

// ShareACL is a runtime, flattened representation of a share's access policy.
// It is derived from config.ShareConfig by ShareACLsFromConfig.
type ShareACL struct {
	Alias          string
	Path           string
	ReadOnly       bool
	AllowAll       bool     // true when AllowedDevices contained the literal "all"
	AllowedDevices []string // remaining tokens (nicknames or device_ids)
}

// AccessDecision is the result of evaluating a ShareACL for a specific peer.
type AccessDecision struct {
	Allow    bool
	ReadOnly bool
}

// Decide returns whether deviceID may access the share and, if so, whether
// the share is read-only.
func (a ShareACL) Decide(deviceID string, resolver DeviceResolver) AccessDecision {
	if a.AllowAll {
		return AccessDecision{Allow: true, ReadOnly: a.ReadOnly}
	}
	if len(a.AllowedDevices) == 0 {
		return AccessDecision{Allow: false}
	}
	var nickname string
	if resolver != nil {
		if n, ok := resolver.NicknameForDeviceID(deviceID); ok {
			nickname = n
		}
	}
	for _, tok := range a.AllowedDevices {
		if tok == deviceID || (nickname != "" && tok == nickname) {
			return AccessDecision{Allow: true, ReadOnly: a.ReadOnly}
		}
	}
	return AccessDecision{Allow: false}
}

// shareConfigView is the minimal shape of config.ShareConfig that the ACL
// layer depends on. Defined here to keep sharesacl.go free of a direct
// dependency on internal/agent/config at test time.
type shareConfigView struct {
	Alias          string
	Path           string
	Permissions    string
	AllowedDevices []string
}

// ShareACLsFromConfig flattens a slice of share configs into runtime ACLs.
// Applies secure defaults: missing permissions = "ro"; the reserved token
// "all" is lifted into AllowAll and removed from the token list.
func ShareACLsFromConfig(shares []shareConfigView) []ShareACL {
	out := make([]ShareACL, 0, len(shares))
	for _, s := range shares {
		acl := ShareACL{
			Alias:    s.Alias,
			Path:     s.Path,
			ReadOnly: s.Permissions != "rw", // "" and anything other than "rw" → ro
		}
		for _, tok := range s.AllowedDevices {
			if tok == "all" {
				acl.AllowAll = true
				continue
			}
			acl.AllowedDevices = append(acl.AllowedDevices, tok)
		}
		out = append(out, acl)
	}
	return out
}
