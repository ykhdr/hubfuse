package hub

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"sync"

	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/hub/store"
	pb "github.com/ykhdr/hubfuse/proto"
)

// Registry manages device registration and event broadcasting.
type Registry struct {
	store       store.Store
	caCert      *x509.Certificate
	caKey       *rsa.PrivateKey
	subscribers map[string]chan *pb.Event // device_id -> event channel
	mu          sync.RWMutex
	logger      *slog.Logger
}

// NewRegistry creates a new Registry backed by the given store.
func NewRegistry(s store.Store, caCert *x509.Certificate, caKey *rsa.PrivateKey, logger *slog.Logger) *Registry {
	return &Registry{
		store:       s,
		caCert:      caCert,
		caKey:       caKey,
		subscribers: make(map[string]chan *pb.Event),
		logger:      logger,
	}
}

// Join creates a device record in the store and returns a signed client TLS
// certificate, private key, and the CA certificate in PEM form. It returns
// common.ErrNicknameTaken if the nickname is already in use.
func (r *Registry) Join(ctx context.Context, deviceID, nickname string) (certPEM, keyPEM, caCertPEM []byte, err error) {
	// Check nickname uniqueness before inserting.
	existing, _ := r.store.GetDeviceByNickname(ctx, nickname)
	if existing != nil {
		return nil, nil, nil, common.ErrNicknameTaken
	}

	d := &store.Device{
		DeviceID: deviceID,
		Nickname: nickname,
		Status:   "offline",
	}
	if err := r.store.CreateDevice(ctx, d); err != nil {
		return nil, nil, nil, err
	}

	certPEM, keyPEM, err = common.SignClientCert(r.caCert, r.caKey, deviceID)
	if err != nil {
		return nil, nil, nil, err
	}

	// Encode the CA certificate DER bytes as PEM.
	caCertPEM = pem.EncodeToMemory(&pem.Block{
		Type:  "CERTIFICATE",
		Bytes: r.caCert.Raw,
	})

	return certPEM, keyPEM, caCertPEM, nil
}

// Register marks a device as online, updates its IP/port and shares, then
// returns the list of all currently online devices. It also broadcasts a
// DeviceOnline event to all other subscribers.
func (r *Registry) Register(ctx context.Context, deviceID, ip string, sshPort int, shares []*pb.Share, protocolVersion int) ([]*store.Device, error) {
	if protocolVersion != common.ProtocolVersion {
		return nil, common.ErrUnsupportedProtocol
	}

	if err := r.store.UpdateDeviceStatus(ctx, deviceID, "online", ip, sshPort); err != nil {
		return nil, err
	}

	storeShares := sharesFromProto(deviceID, shares)
	if err := r.store.SetShares(ctx, deviceID, storeShares); err != nil {
		return nil, err
	}

	online, err := r.store.ListOnlineDevices(ctx)
	if err != nil {
		return nil, err
	}

	// Build the DeviceOnline event.
	device, err := r.store.GetDevice(ctx, deviceID)
	if err != nil {
		return nil, err
	}

	event := &pb.Event{
		Payload: &pb.Event_DeviceOnline{
			DeviceOnline: &pb.DeviceOnlineEvent{
				DeviceId: device.DeviceID,
				Nickname: device.Nickname,
				Ip:       ip,
				SshPort:  int32(sshPort),
				Shares:   shares,
			},
		},
	}
	r.Broadcast(event, deviceID)

	return online, nil
}

// Rename updates the device's nickname. Returns common.ErrNicknameTaken if the
// new nickname is already in use by another device.
func (r *Registry) Rename(ctx context.Context, deviceID, newNickname string) error {
	existing, _ := r.store.GetDeviceByNickname(ctx, newNickname)
	if existing != nil && existing.DeviceID != deviceID {
		return common.ErrNicknameTaken
	}

	return r.store.UpdateDeviceNickname(ctx, deviceID, newNickname)
}

// Heartbeat updates the heartbeat timestamp for a device.
func (r *Registry) Heartbeat(ctx context.Context, deviceID string) error {
	return r.store.UpdateHeartbeat(ctx, deviceID)
}

// UpdateShares replaces the shares for a device and broadcasts a SharesUpdated
// event to all other subscribers.
func (r *Registry) UpdateShares(ctx context.Context, deviceID string, shares []*pb.Share) error {
	storeShares := sharesFromProto(deviceID, shares)
	if err := r.store.SetShares(ctx, deviceID, storeShares); err != nil {
		return err
	}

	event := &pb.Event{
		Payload: &pb.Event_SharesUpdated{
			SharesUpdated: &pb.SharesUpdatedEvent{
				DeviceId: deviceID,
				Shares:   shares,
			},
		},
	}
	r.Broadcast(event, deviceID)

	return nil
}

// Deregister marks a device as offline, broadcasts a DeviceOffline event, and
// removes the device's event subscription.
func (r *Registry) Deregister(ctx context.Context, deviceID string) error {
	device, err := r.store.GetDevice(ctx, deviceID)
	if err != nil {
		return err
	}

	if err := r.store.UpdateDeviceStatus(ctx, deviceID, "offline", device.LastIP, device.SSHPort); err != nil {
		return err
	}

	event := &pb.Event{
		Payload: &pb.Event_DeviceOffline{
			DeviceOffline: &pb.DeviceOfflineEvent{
				DeviceId: device.DeviceID,
				Nickname: device.Nickname,
			},
		},
	}
	r.Broadcast(event, deviceID)

	r.mu.Lock()
	if ch, ok := r.subscribers[deviceID]; ok {
		delete(r.subscribers, deviceID)
		close(ch)
	}
	r.mu.Unlock()

	return nil
}

// Subscribe creates a buffered event channel for the device. The returned
// function unsubscribes and closes the channel.
func (r *Registry) Subscribe(deviceID string) (<-chan *pb.Event, func()) {
	ch := make(chan *pb.Event, 64)

	r.mu.Lock()
	r.subscribers[deviceID] = ch
	r.mu.Unlock()

	unsub := func() {
		r.mu.Lock()
		if existing, ok := r.subscribers[deviceID]; ok && existing == ch {
			delete(r.subscribers, deviceID)
		}
		r.mu.Unlock()
		// Drain and close the channel.
		close(ch)
	}

	return ch, unsub
}

// Broadcast sends event to all subscribers except excludeDevice. If a
// subscriber's channel is full the send is skipped and a warning is logged.
func (r *Registry) Broadcast(event *pb.Event, excludeDevice string) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for deviceID, ch := range r.subscribers {
		if deviceID == excludeDevice {
			continue
		}
		select {
		case ch <- event:
		default:
			r.logger.Warn("event channel full, dropping event",
				slog.String("device_id", deviceID))
		}
	}
}

// MarkOffline marks a device offline and broadcasts DeviceOffline. Used by
// the heartbeat monitor for stale devices.
func (r *Registry) MarkOffline(ctx context.Context, deviceID string) error {
	device, err := r.store.GetDevice(ctx, deviceID)
	if err != nil {
		return err
	}

	if err := r.store.UpdateDeviceStatus(ctx, deviceID, "offline", device.LastIP, device.SSHPort); err != nil {
		return err
	}

	event := &pb.Event{
		Payload: &pb.Event_DeviceOffline{
			DeviceOffline: &pb.DeviceOfflineEvent{
				DeviceId: device.DeviceID,
				Nickname: device.Nickname,
			},
		},
	}
	r.Broadcast(event, deviceID)

	return nil
}

// sharesToProto converts store.Share records to pb.Share messages.
func sharesToProto(shares []*store.Share) []*pb.Share {
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

// sharesFromProto converts pb.Share messages to store.Share records for the
// given device.
func sharesFromProto(deviceID string, shares []*pb.Share) []*store.Share {
	result := make([]*store.Share, 0, len(shares))
	for _, s := range shares {
		result = append(result, &store.Share{
			DeviceID:       deviceID,
			Alias:          s.Alias,
			Permissions:    s.Permissions,
			AllowedDevices: s.AllowedDevices,
		})
	}
	return result
}
