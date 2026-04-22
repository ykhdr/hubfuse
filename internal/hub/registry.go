package hub

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"log/slog"
	"sync"
	"time"

	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/hub/store"
	pb "github.com/ykhdr/hubfuse/proto"
)

// Registry manages device registration and event broadcasting.
type Registry struct {
	store         store.Store
	caCert        *x509.Certificate
	caKey         *rsa.PrivateKey
	subscribers   map[string]chan *pb.Event // device_id -> event channel
	mu            sync.RWMutex
	logger        *slog.Logger
	joinTokenTTL  time.Duration
}

// NewRegistry creates a new Registry backed by the given store. joinTokenTTL
// controls how long issued join tokens remain valid; pass 0 to use the
// default (10 minutes).
func NewRegistry(s store.Store, caCert *x509.Certificate, caKey *rsa.PrivateKey, logger *slog.Logger, joinTokenTTL time.Duration) *Registry {
	if joinTokenTTL == 0 {
		joinTokenTTL = defaultJoinTokenTTL
	}
	return &Registry{
		store:        s,
		caCert:       caCert,
		caKey:        caKey,
		subscribers:  make(map[string]chan *pb.Event),
		logger:       logger,
		joinTokenTTL: joinTokenTTL,
	}
}

// Join creates a device record in the store and returns a signed client TLS
// certificate, private key, and the CA certificate in PEM form. It validates
// joinToken against the pending_join_tokens table and returns
// common.ErrInvalidJoinToken or common.ErrJoinTokenExpired on failure. The
// token is atomically consumed before any device state changes, so a token
// is single-use even under concurrent Joins. It returns common.ErrNicknameTaken
// if the nickname is already in use. ip is the caller's apparent address
// (best effort; may be empty).
func (r *Registry) Join(ctx context.Context, deviceID, nickname, ip, joinToken string) (certPEM, keyPEM, caCertPEM []byte, err error) {
	if err := r.consumeJoinToken(ctx, joinToken); err != nil {
		return nil, nil, nil, err
	}

	existing, _ := r.store.GetDeviceByNickname(ctx, nickname)
	if existing != nil {
		return nil, nil, nil, common.ErrNicknameTaken
	}

	d := &store.Device{
		DeviceID:      deviceID,
		Nickname:      nickname,
		LastIP:        ip,
		Status:        store.StatusRegistered,
		LastHeartbeat: time.Now().UTC(),
	}
	if err := r.store.CreateDevice(ctx, d); err != nil {
		return nil, nil, nil, err
	}

	certPEM, keyPEM, err = common.SignClientCert(r.caCert, r.caKey, deviceID)
	if err != nil {
		return nil, nil, nil, err
	}

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

	device, err := r.store.GetDevice(ctx, deviceID)
	if err != nil {
		return nil, err
	}

	online, err := r.store.ListOnlineDevices(ctx)
	if err != nil {
		return nil, err
	}

	storeShares := sharesFromProto(deviceID, shares)
	if err := r.store.SetShares(ctx, deviceID, storeShares); err != nil {
		return nil, err
	}

	if err := r.store.UpdateHeartbeat(ctx, deviceID); err != nil {
		return nil, err
	}

	if err := r.store.UpdateDeviceStatus(ctx, deviceID, store.StatusOnline, ip, sshPort); err != nil {
		return nil, err
	}

	current := &store.Device{
		DeviceID: device.DeviceID,
		Nickname: device.Nickname,
		LastIP:   ip,
		SSHPort:  sshPort,
		Status:   store.StatusOnline,
	}
	found := false
	for i, d := range online {
		if d.DeviceID == deviceID {
			online[i] = current
			found = true
			break
		}
	}
	if !found {
		online = append(online, current)
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

	if err := r.store.UpdateDeviceStatus(ctx, deviceID, store.StatusOffline, device.LastIP, device.SSHPort); err != nil {
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

// Subscribe creates a buffered event channel for the device. If the device
// already has a registered channel (e.g. due to reconnect), the old channel
// is closed and replaced. The returned function unsubscribes and closes the
// channel.
func (r *Registry) Subscribe(deviceID string) (<-chan *pb.Event, func()) {
	ch := make(chan *pb.Event, 64)

	r.mu.Lock()
	if old, ok := r.subscribers[deviceID]; ok {
		close(old)
	}
	r.subscribers[deviceID] = ch
	r.mu.Unlock()

	unsub := func() {
		r.mu.Lock()
		if existing, ok := r.subscribers[deviceID]; ok && existing == ch {
			delete(r.subscribers, deviceID)
			close(ch)
		}
		r.mu.Unlock()
	}

	return ch, unsub
}

// ActiveSubscribers returns the device IDs that currently have an active
// subscription stream.
func (r *Registry) ActiveSubscribers() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ids := make([]string, 0, len(r.subscribers))
	for id := range r.subscribers {
		ids = append(ids, id)
	}
	return ids
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

// BroadcastAll sends event to every subscriber. Use this when there is no
// sender to exclude (e.g. hub-initiated broadcasts during shutdown).
func (r *Registry) BroadcastAll(event *pb.Event) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	for deviceID, ch := range r.subscribers {
		select {
		case ch <- event:
		default:
			r.logger.Warn("event channel full, dropping event",
				slog.String("device_id", deviceID))
		}
	}
}

// GetShares returns all shares registered for the given device.
func (r *Registry) GetShares(ctx context.Context, deviceID string) ([]*store.Share, error) {
	return r.store.GetShares(ctx, deviceID)
}

// GetSharesForDevices returns shares for each of the given device IDs in a
// single query, keyed by device_id. Devices with no shares are absent from
// the returned map.
func (r *Registry) GetSharesForDevices(ctx context.Context, deviceIDs []string) (map[string][]*store.Share, error) {
	return r.store.GetSharesForDevices(ctx, deviceIDs)
}

// ListDevicesWithShares returns all devices regardless of status together with
// a map of their shares, keyed by device_id.
func (r *Registry) ListDevicesWithShares(ctx context.Context) ([]*store.Device, map[string][]*store.Share, error) {
	devices, err := r.store.ListAllDevices(ctx)
	if err != nil {
		return nil, nil, err
	}

	ids := make([]string, 0, len(devices))
	for _, d := range devices {
		ids = append(ids, d.DeviceID)
	}

	shares, err := r.store.GetSharesForDevices(ctx, ids)
	if err != nil {
		return nil, nil, err
	}

	return devices, shares, nil
}

// MarkOffline marks the given device offline and broadcasts DeviceOffline.
// Used by the heartbeat monitor for stale devices. The caller passes the
// *Device already in hand, avoiding a redundant store lookup.
func (r *Registry) MarkOffline(ctx context.Context, device *store.Device) error {
	if err := r.store.UpdateDeviceStatus(ctx, device.DeviceID, store.StatusOffline, device.LastIP, device.SSHPort); err != nil {
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
	r.Broadcast(event, device.DeviceID)

	return nil
}

// BroadcastDeviceRemoved sends a DeviceRemoved event to all subscribers except
// the removed device.
func (r *Registry) BroadcastDeviceRemoved(device *store.Device) {
	event := &pb.Event{
		Payload: &pb.Event_DeviceRemoved{
			DeviceRemoved: &pb.DeviceRemovedEvent{
				DeviceId: device.DeviceID,
				Nickname: device.Nickname,
			},
		},
	}
	r.Broadcast(event, device.DeviceID)
}

// removeSubscriber removes and closes a subscriber channel if present.
func (r *Registry) removeSubscriber(deviceID string) {
	r.mu.Lock()
	if ch, ok := r.subscribers[deviceID]; ok {
		delete(r.subscribers, deviceID)
		close(ch)
	}
	r.mu.Unlock()
}

// sharesToProto converts store.Share records to pb.Share messages.
func sharesToProto(shares []*store.Share) []*pb.Share {
	result := make([]*pb.Share, 0, len(shares))
	for _, s := range shares {
		result = append(result, &pb.Share{
			Alias:          s.Alias,
			Permissions:    string(s.Permissions),
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
			Permissions:    store.Permission(s.Permissions),
			AllowedDevices: s.AllowedDevices,
		})
	}
	return result
}
