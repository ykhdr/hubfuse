package agent

import (
	"context"
	"log/slog"

	agentconfig "github.com/ykhdr/hubfuse/internal/agent/config"
	pb "github.com/ykhdr/hubfuse/proto"
)

// onConfigChange is called by the config Watcher whenever the config file is
// modified. It computes a diff against the previous config and reacts to
// changes in shares and mounts.
func (d *Daemon) onConfigChange(old, new *agentconfig.Config) {
	diff := agentconfig.ComputeDiff(old, new)

	// Swap the cached config BEFORE publishing the new shares to the hub. The
	// supervisor re-runs sessionOnce (Register) on every reconnect, concurrently
	// with this watcher; sessionOnce snapshots d.config and re-Registers its
	// shares. If we published UpdateShares(new) while d.config still held the old
	// shares, a concurrent sessionOnce could snapshot the old shares and Register
	// them AFTER UpdateShares(new) landed — leaving the hub with obsolete shares.
	// Swapping first makes the in-memory config consistent with the publish, so a
	// sessionOnce that runs after this point necessarily snapshots the new shares.
	// Nothing between here and the swap site reads d.config, so the move is safe;
	// the lock is never held across the blocking UpdateShares RPC. (#61)
	d.mu.Lock()
	d.config = new
	d.mu.Unlock()

	if diff.SharesChanged {
		// Push updated shares to the hub.
		shares := configSharesToProto(new.Shares)
		if err := d.updateSharesFn(context.Background(), shares); err != nil {
			d.logger.Error("failed to update shares on hub", "error", err)
		}
		// Push ACL snapshot to the SSH server and surface any shares that
		// the new secure defaults would make inaccessible.
		acls := sharesToACL(new.Shares)
		warnInaccessibleShares(d.logger, acls)
		d.sshServer.UpdateShares(acls)
	}

	for _, mc := range diff.MountsAdded {
		d.tryMount(mc)
	}

	for _, mc := range diff.MountsRemoved {
		if err := d.mounter.Unmount(mc.Device, mc.Share); err != nil {
			d.logger.Warn("unmount removed config entry",
				"device", mc.Device,
				"share", mc.Share,
				"error", err,
			)
		}
		// Restore the target to a normal mode now that it has been removed from
		// config — a 0o500 dir lingering after mount remove would surprise the
		// user. Run outside the error check so a failed unmount does not skip
		// the perm restore. (#49 guard-target)
		if err := d.mounter.unguardTarget(mc.To); err != nil {
			d.logger.Warn("restore mount target perms after config removal",
				"to", mc.To,
				"error", err,
			)
		}
	}
}

// configSharesToProto converts a slice of ShareConfig to proto Share messages.
func configSharesToProto(shares []agentconfig.ShareConfig) []*pb.Share {
	result := make([]*pb.Share, 0, len(shares))
	for _, s := range shares {
		result = append(result, &pb.Share{
			Alias:          s.Alias,
			Permissions:    s.Permissions,
			AllowedDevices: s.AllowedDevices,
		})
	}
	return result
}

// sharesToACL flattens a slice of ShareConfig into runtime ACLs, applying the
// secure defaults described by ShareACLsFromConfig (missing permissions -> ro;
// literal "all" -> wildcard; empty list -> deny).
func sharesToACL(shares []agentconfig.ShareConfig) []ShareACL {
	views := make([]shareConfigView, 0, len(shares))
	for _, s := range shares {
		views = append(views, shareConfigView{
			Alias:          s.Alias,
			Path:           s.Path,
			Permissions:    s.Permissions,
			AllowedDevices: s.AllowedDevices,
		})
	}
	return ShareACLsFromConfig(views)
}

// warnInaccessibleShares logs a warning for each share that is unreachable
// under the new ACL semantics (no allowed_devices and no "all" wildcard).
func warnInaccessibleShares(logger *slog.Logger, acls []ShareACL) {
	for _, acl := range acls {
		if !acl.AllowAll && len(acl.AllowedDevices) == 0 {
			logger.Warn("share has no allowed-devices and is inaccessible", "alias", acl.Alias)
		}
	}
}

// tryMount attempts to mount the share described by mc if the target device is
// currently online and paired.
func (d *Daemon) tryMount(mc agentconfig.MountConfig) {
	// Snapshot the endpoint primitives BY VALUE under the lock rather than
	// aliasing the map's *OnlineDevice across the unlocked region below. A
	// concurrent handleDeviceOnline replaces the map entry with a fresh
	// *OnlineDevice (it never mutates the published one), so today the snapshot is
	// at worst a stale-but-valid endpoint; copying the values keeps tryMount from
	// reading a struct another goroutine could later mutate in place (the pattern
	// handleSharesUpdated already uses for .Shares) and makes the resolve→Mount
	// handoff self-contained. (#61)
	var (
		found    bool
		deviceID string
		ip       string
		sshPort  int
	)
	d.mu.RLock()
	for _, dev := range d.onlineDevices {
		if dev.Nickname == mc.Device {
			found = true
			deviceID = dev.DeviceID
			ip = dev.IP
			sshPort = dev.SSHPort
			break
		}
	}
	d.mu.RUnlock()

	if !found {
		d.logger.Debug("tryMount: device not online, skipping",
			"device", mc.Device,
			"share", mc.Share,
		)
		// Guard the target so it cannot accept stray local writes while the
		// device is offline. (#49 guard-target)
		if err := d.mounter.guardTarget(mc.To); err != nil {
			d.logger.Warn("guard mount target (device offline)", "to", mc.To, "error", err)
		}
		return
	}

	if !d.isPaired(deviceID) {
		d.logger.Debug("tryMount: device not paired, skipping",
			"device", mc.Device,
			"share", mc.Share,
		)
		// Guard the target so it cannot accept stray local writes while the
		// device is unpaired. (#49 guard-target)
		if err := d.mounter.guardTarget(mc.To); err != nil {
			d.logger.Warn("guard mount target (device unpaired)", "to", mc.To, "error", err)
		}
		return
	}

	if err := d.mounter.Mount(context.Background(), mc, deviceID, ip, sshPort); err != nil {
		d.logger.Error("tryMount failed",
			"device", mc.Device,
			"share", mc.Share,
			"error", err,
		)
	}
}
