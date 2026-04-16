package agent

import (
	"context"
	"fmt"
	"path/filepath"

	pb "github.com/ykhdr/hubfuse/proto"
)

// handleEvent dispatches an incoming hub event to the appropriate handler.
func (d *Daemon) handleEvent(event *pb.Event) {
	switch p := event.Payload.(type) {
	case *pb.Event_DeviceOnline:
		d.handleDeviceOnline(p.DeviceOnline)
	case *pb.Event_DeviceOffline:
		d.handleDeviceOffline(p.DeviceOffline)
	case *pb.Event_SharesUpdated:
		d.handleSharesUpdated(p.SharesUpdated)
	case *pb.Event_PairingRequested:
		d.handlePairingRequested(p.PairingRequested)
	case *pb.Event_PairingCompleted:
		d.handlePairingCompleted(p.PairingCompleted)
	case *pb.Event_SubscribeReady:
		// Handled by the connector; nothing to do here.
	case *pb.Event_DeviceRemoved:
		d.handleDeviceRemoved(p.DeviceRemoved)
	default:
		d.logger.Warn("received unknown event type")
	}
}

// handleDeviceOnline adds the device to onlineDevices and, if it is paired and
// has a mount configured, auto-mounts.
func (d *Daemon) handleDeviceOnline(e *pb.DeviceOnlineEvent) {
	d.logger.Info("device came online",
		"device_id", e.DeviceId,
		"nickname", e.Nickname,
		"ip", e.Ip,
	)

	shares := make([]string, 0, len(e.Shares))
	for _, s := range e.Shares {
		shares = append(shares, s.Alias)
	}

	info := &OnlineDevice{
		DeviceID: e.DeviceId,
		Nickname: e.Nickname,
		IP:       e.Ip,
		SSHPort:  int(e.SshPort),
		Shares:   shares,
	}

	d.mu.Lock()
	d.onlineDevices[e.DeviceId] = info
	d.mu.Unlock()

	mc, shouldMount := d.shouldMount(e.Nickname)
	if !shouldMount {
		return
	}
	if !d.isPaired(e.DeviceId) {
		return
	}
	if err := d.mounter.Mount(context.Background(), mc, info.IP, info.SSHPort); err != nil {
		d.logger.Error("auto-mount on device-online failed",
			"device", e.Nickname,
			"share", mc.Share,
			"error", err,
		)
	}
}

// handleDeviceOffline removes the device from onlineDevices and unmounts any
// shares that were mounted from it.
func (d *Daemon) handleDeviceOffline(e *pb.DeviceOfflineEvent) {
	d.logger.Info("device went offline",
		"device_id", e.DeviceId,
		"nickname", e.Nickname,
	)

	d.mu.Lock()
	delete(d.onlineDevices, e.DeviceId)
	d.mu.Unlock()

	if err := d.mounter.UnmountDevice(e.Nickname); err != nil {
		d.logger.Warn("unmount device shares on offline",
			"nickname", e.Nickname,
			"error", err,
		)
	}
}

// handleDeviceRemoved removes the device from onlineDevices and unmounts any
// active mounts. Used when the hub prunes long-inactive devices.
func (d *Daemon) handleDeviceRemoved(e *pb.DeviceRemovedEvent) {
	d.logger.Info("device pruned from hub",
		"device_id", e.DeviceId,
		"nickname", e.Nickname,
	)

	d.mu.Lock()
	delete(d.onlineDevices, e.DeviceId)
	d.mu.Unlock()

	if err := d.mounter.UnmountDevice(e.Nickname); err != nil {
		d.logger.Warn("unmount device shares on removal",
			"nickname", e.Nickname,
			"error", err,
		)
	}
}

// handleSharesUpdated refreshes the known share list for a device. If any
// shares that we are currently mounting were removed, we unmount them.
func (d *Daemon) handleSharesUpdated(e *pb.SharesUpdatedEvent) {
	d.logger.Info("device shares updated", "device_id", e.DeviceId)

	d.mu.Lock()
	info, ok := d.onlineDevices[e.DeviceId]
	if ok {
		newShares := make([]string, 0, len(e.Shares))
		for _, s := range e.Shares {
			newShares = append(newShares, s.Alias)
		}
		info.Shares = newShares
	}
	d.mu.Unlock()

	if !ok {
		return
	}

	newShareSet := make(map[string]struct{}, len(e.Shares))
	for _, s := range e.Shares {
		newShareSet[s.Alias] = struct{}{}
	}

	for _, mnt := range d.mounter.ActiveMounts() {
		if mnt.Device != info.Nickname {
			continue
		}
		if _, stillExists := newShareSet[mnt.Share]; !stillExists {
			if err := d.mounter.Unmount(mnt.Device, mnt.Share); err != nil {
				d.logger.Warn("failed to unmount removed share",
					"device", mnt.Device,
					"share", mnt.Share,
					"error", err,
				)
			}
		}
	}
}

// handlePairingRequested logs a pairing request so the user can act on it via
// the CLI. The daemon does not handle interactive user input directly.
func (d *Daemon) handlePairingRequested(e *pb.PairingRequestedEvent) {
	d.logger.Info(
		fmt.Sprintf("Pairing requested from %q. Run 'hubfuse pair-confirm' to accept.", e.FromNickname),
		"from_device_id", e.FromDeviceId,
		"from_nickname", e.FromNickname,
	)
}

// handlePairingCompleted saves the peer's public key to known_devices (keyed
// by device_id) and, if the peer is currently online and has a mount
// configured, auto-mounts.
func (d *Daemon) handlePairingCompleted(e *pb.PairingCompletedEvent) {
	d.logger.Info("pairing completed", "peer_device_id", e.PeerDeviceId)

	knownDevicesDir := filepath.Join(d.dataDir, "known_devices")

	if err := SavePeerPublicKey(knownDevicesDir, e.PeerDeviceId, e.PeerPublicKey); err != nil {
		d.logger.Error("failed to save peer public key",
			"peer_device_id", e.PeerDeviceId,
			"error", err,
		)
		return
	}

	d.mu.RLock()
	info, online := d.onlineDevices[e.PeerDeviceId]
	d.mu.RUnlock()

	if !online {
		return
	}

	mc, shouldMount := d.shouldMount(info.Nickname)
	if !shouldMount {
		return
	}

	if err := d.mounter.Mount(context.Background(), mc, info.IP, info.SSHPort); err != nil {
		d.logger.Error("auto-mount after pairing failed",
			"device", info.Nickname,
			"share", mc.Share,
			"error", err,
		)
	}
}
