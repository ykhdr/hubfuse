package agent

import (
	"context"

	agentconfig "github.com/ykhdr/hubfuse/internal/agent/config"
	pb "github.com/ykhdr/hubfuse/proto"
)

// onConfigChange is called by the config Watcher whenever the config file is
// modified. It computes a diff against the previous config and reacts to
// changes in shares and mounts.
func (d *Daemon) onConfigChange(old, new *agentconfig.Config) {
	diff := agentconfig.ComputeDiff(old, new)

	if diff.SharesChanged {
		// Push updated shares to the hub.
		shares := configSharesToProto(new.Shares)
		if err := d.hubClient.UpdateShares(context.Background(), shares); err != nil {
			d.logger.Error("failed to update shares on hub", "error", err)
		}
		// Rebuild the SSH server's alias->path mapping.
		d.sshServer.UpdateShares(sharesToMap(new.Shares))
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
	}

	d.mu.Lock()
	d.config = new
	d.mu.Unlock()
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

// sharesToMap converts a slice of ShareConfig to an alias->path map for the
// SSH server.
func sharesToMap(shares []agentconfig.ShareConfig) map[string]string {
	m := make(map[string]string, len(shares))
	for _, s := range shares {
		m[s.Alias] = s.Path
	}
	return m
}

// tryMount attempts to mount the share described by mc if the target device is
// currently online and paired.
func (d *Daemon) tryMount(mc agentconfig.MountConfig) {
	d.mu.RLock()
	var info *OnlineDevice
	for _, dev := range d.onlineDevices {
		if dev.Nickname == mc.Device {
			info = dev
			break
		}
	}
	d.mu.RUnlock()

	if info == nil {
		d.logger.Debug("tryMount: device not online, skipping",
			"device", mc.Device,
			"share", mc.Share,
		)
		return
	}

	if !d.isPaired(info.DeviceID) {
		d.logger.Debug("tryMount: device not paired, skipping",
			"device", mc.Device,
			"share", mc.Share,
		)
		return
	}

	if err := d.mounter.Mount(context.Background(), mc, info.IP, info.SSHPort); err != nil {
		d.logger.Error("tryMount failed",
			"device", mc.Device,
			"share", mc.Share,
			"error", err,
		)
	}
}
