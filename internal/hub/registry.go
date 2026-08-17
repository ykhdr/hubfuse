package hub

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/hub/store"
	pb "github.com/ykhdr/hubfuse/proto"
)

// Registry manages device registration and event broadcasting.
type Registry struct {
	store        store.Store
	caCert       *x509.Certificate
	caKey        *rsa.PrivateKey
	subscribers  map[string]chan *pb.Event // device_id -> event channel
	mu           sync.RWMutex
	logger       *slog.Logger
	joinTokenTTL time.Duration

	// draining is set once by Drain (hub shutdown) and only ever read
	// afterwards; an atomic keeps it out of the subscriber lock. See canRecover.
	draining atomic.Bool
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

	// A hub on its way out must not accept new online rows: such a row survives
	// into the next hub start and is served to peers as an endpoint nobody is
	// listening on. This is the cheap early exit — the authoritative check is
	// in markOnlineUnlessDraining, next to the write it guards, because Drain
	// can land during the several round-trips in between. The agent retries
	// with backoff and registers against the hub that comes back. (#69)
	if r.draining.Load() {
		return nil, common.ErrHubShuttingDown
	}

	device, err := r.store.GetDevice(ctx, deviceID)
	if err != nil {
		// A device whose row is gone (pruned, or removed by `leave`) must be
		// told so explicitly — the agent turns this into a re-join prompt
		// instead of logging "registered with hub" and carrying on. (#69)
		return nil, deviceErr(err)
	}

	online, err := r.store.ListOnlineDevices(ctx)
	if err != nil {
		return nil, err
	}

	// Every write below is translated the same way as the read above: a prune
	// can land between them, and the agent must get the actionable
	// "device unknown, re-join" answer no matter which statement noticed — and
	// whatever shape that statement's failure took (see writeErr).
	storeShares := sharesFromProto(deviceID, shares)
	if err := r.store.SetShares(ctx, deviceID, storeShares); err != nil {
		return nil, r.writeErr(ctx, deviceID, err)
	}

	if err := r.store.UpdateHeartbeat(ctx, deviceID); err != nil {
		return nil, r.writeErr(ctx, deviceID, err)
	}

	if err := r.markOnlineUnlessDraining(ctx, deviceID, ip, sshPort); err != nil {
		if errors.Is(err, common.ErrHubShuttingDown) {
			return nil, err
		}
		return nil, r.writeErr(ctx, deviceID, err)
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

	// Translated like every other device-addressed write: a pruned row must
	// reach the CLI as "device not found", not as a raw store string. (#69)
	return deviceErr(r.store.UpdateDeviceNickname(ctx, deviceID, newNickname))
}

// Heartbeat records liveness for a device and, when the hub had already given
// up on it, brings it back online.
//
// A heartbeat used to only move last_heartbeat. That left a device the monitor
// had demoted stuck offline until its whole session was re-established: the
// agent process was alive, connected, and pinging, while every peer had
// unmounted its shares and had no event that would ever bring them back. So a
// heartbeat from a device the hub believes is offline is treated as what it
// demonstrably is — proof the device is up — and the transition is broadcast.
//
// ip is the caller's apparent address (best effort; may be empty). It refreshes
// last_ip so a device that recovered at a new address is announced at that
// address; the stored SSH port and share list are reused, since a heartbeat
// carries neither and only Register can change them.
//
// The promotion is conditional (MarkOnlineIfOffline): a Register racing this
// heartbeat wins the row and owns the DeviceOnline broadcast, so peers never
// see two announcements or an announcement built from data the Register was
// about to replace. (#69)
func (r *Registry) Heartbeat(ctx context.Context, deviceID, ip string) error {
	// Write liveness FIRST: the monitor's demotion is conditional on
	// last_heartbeat still being stale, so recording the beat before the
	// promotion is what makes a concurrent stale-sweep back off instead of
	// demoting the device we are about to bring back.
	if err := r.store.UpdateHeartbeat(ctx, deviceID); err != nil {
		return deviceErr(err)
	}

	device, err := r.store.GetDevice(ctx, deviceID)
	if err != nil {
		return deviceErr(err)
	}
	if device.Status != store.StatusOffline {
		// Ordinary case: nothing to announce.
		return nil
	}
	if !r.canRecover(deviceID) {
		// Offline for a reason other than "the hub gave up on a live device".
		// This is only the cheap early exit — recoverAndAnnounce re-checks the
		// same guard atomically with the write. See canRecover.
		return nil
	}
	if device.SSHPort == 0 {
		// The device has an offline row but never completed a Register, so the
		// hub has no endpoint to announce. Its own Register will announce it.
		r.logger.Warn("heartbeat from an offline device with no known SSH port; leaving it offline",
			slog.String("device_id", deviceID))
		return nil
	}

	// Read the shares BEFORE the promotion so a failure here leaves the device
	// offline and the next heartbeat retries the whole recovery, rather than
	// promoting the device with nothing to announce.
	shares, err := r.store.GetShares(ctx, deviceID)
	if err != nil {
		return err
	}

	announcedIP := device.LastIP
	if ip != "" {
		announcedIP = ip
	}

	recovered, err := r.recoverAndAnnounce(ctx, deviceID, ip, &pb.Event{
		Payload: &pb.Event_DeviceOnline{
			DeviceOnline: &pb.DeviceOnlineEvent{
				DeviceId: device.DeviceID,
				Nickname: device.Nickname,
				Ip:       announcedIP,
				SshPort:  int32(device.SSHPort),
				Shares:   sharesToProto(shares),
			},
		},
	})
	if err != nil {
		return err
	}
	if !recovered {
		return nil
	}

	// Logged outside the critical section: file I/O must not sit inside a lock
	// that gates the event fanout.
	r.logger.Info("device recovered via heartbeat",
		slog.String("device_id", deviceID),
		slog.String("nickname", device.Nickname),
		slog.String("ip", announcedIP))

	return nil
}

// canRecover reports whether a heartbeat may bring deviceID back online.
//
// Being offline is not by itself evidence that the hub "gave up on a live
// device". A device that deregistered on purpose (`hubfuse stop`) is offline
// too, and a heartbeat still in flight when its Deregister lands must not
// resurrect it: peers would be told to mount a daemon that has exited. The two
// cases are distinguishable by the event subscription — MarkOffline leaves it
// open (the device is still connected, just late), while Deregister and Leave
// close it before touching the status, and a draining hub refuses recovery
// outright. (#69)
//
// Callers must not act on this answer across a database round-trip: use
// recoverAndAnnounce, which re-evaluates it atomically with the write and the
// announcement.
func (r *Registry) canRecover(deviceID string) bool {
	if r.draining.Load() {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, subscribed := r.subscribers[deviceID]
	return subscribed
}

// recoverAndAnnounce evaluates the recovery guard, promotes the device, and
// publishes event — all inside ONE critical section — and reports whether the
// row changed.
//
// All three steps belong together. Checking canRecover and writing a couple of
// round-trips later is a check-then-act: a Deregister completing in between
// (removeSubscriber, then its own offline write) is followed by our promotion,
// and a Drain in between leaves an online row behind after the shutdown sweep
// has already passed the device by — a phantom that survives into the next hub
// start rather than self-healing. Publishing outside the section is the same
// mistake one level up: the row would end up correct while peers were left with
// DeviceOffline followed by DeviceOnline, i.e. their last word on a departed
// daemon is that it is up, and they would mount a dead endpoint.
//
// removeSubscriber (used by Deregister and Leave) and Drain both take the WRITE
// lock, so they either complete first — and this guard then refuses, leaving
// nothing to announce — or wait, and their own DeviceOffline is published after
// our DeviceOnline, which is the order peers must see. (#69)
func (r *Registry) recoverAndAnnounce(ctx context.Context, deviceID, ip string, event *pb.Event) (bool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.draining.Load() {
		return false, nil
	}
	if _, subscribed := r.subscribers[deviceID]; !subscribed {
		return false, nil
	}

	recovered, err := r.store.MarkOnlineIfOffline(ctx, deviceID, ip)
	if err != nil || !recovered {
		return false, err
	}

	r.broadcastLocked(event, deviceID)
	return true, nil
}

// markOnlineUnlessDraining writes the online row with the draining flag
// re-checked inside the same critical section, mirroring recoverAndAnnounce.
//
// Register performs several round-trips before it gets here, and Drain can land
// in any of those gaps: Hub.Stop drains, sweeps every online device offline, and
// only then calls GracefulStop — which WAITS for this very RPC. A status write
// that slipped past a top-of-function check would therefore land after the
// sweep and outlive the hub. Drain takes the write lock, so it either precedes
// this guard (and the registration is refused) or follows the write (and the
// sweep that comes after it demotes the row). (#69)
func (r *Registry) markOnlineUnlessDraining(ctx context.Context, deviceID, ip string, sshPort int) error {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if r.draining.Load() {
		return common.ErrHubShuttingDown
	}
	return r.store.UpdateDeviceStatus(ctx, deviceID, store.StatusOnline, ip, sshPort)
}

// Drain stops heartbeat-driven recovery for the rest of this hub's life. Hub
// shutdown marks every online device offline and then waits for in-flight RPCs
// to finish; without this, a heartbeat processed inside that window would flip
// a device back to online and leave a phantom-online row behind for the next
// hub start to serve to its peers.
//
// The flag is set under the WRITE lock so it cannot land inside the guarded
// windows of recoverAndAnnounce or markOnlineUnlessDraining: either it wins and
// every later promotion is refused, or it waits for the in-flight one and the
// shutdown sweep that follows demotes that device along with the rest. (#69)
func (r *Registry) Drain() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.draining.Store(true)
}

// Draining reports whether Hub.Stop has begun. The gRPC interceptors read it to
// refuse new RPCs on a hub that is going away, so a client cannot open work the
// shutdown sequence has already accounted for — a Subscribe accepted after
// CloseAllSubscribers would never be closed, and a Join would burn its
// single-use token on a hub that will not be there to honour the certificate.
// (#75)
func (r *Registry) Draining() bool {
	return r.draining.Load()
}

// writeErr classifies a failed device-addressed write. A device pruned between
// two statements of the same RPC does not always surface as ErrNotFound: with
// foreign keys enabled, inserting shares for a deleted device fails as a
// constraint violation instead. So when a write fails for any reason, ask
// whether the row is still there before deciding what the caller is told —
// one extra query, on an error path only. (#69)
func (r *Registry) writeErr(ctx context.Context, deviceID string, err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return common.ErrDeviceNotFound
	}
	if _, getErr := r.store.GetDevice(ctx, deviceID); errors.Is(getErr, store.ErrNotFound) {
		return common.ErrDeviceNotFound
	}
	return err
}

// deviceErr translates the store's own not-found sentinel into the
// protocol-level error the RPC layer speaks, and passes everything else
// through. It exists so the data layer can keep its errors free of gRPC
// status codes while callers still get a NotFound they can act on. (#69)
func deviceErr(err error) error {
	if errors.Is(err, store.ErrNotFound) {
		return common.ErrDeviceNotFound
	}
	return err
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

// Deregister removes the device's event subscription, marks it offline, and
// broadcasts a DeviceOffline event — in that order. Closing the subscription
// first is load-bearing, not cosmetic: it is what stops a heartbeat still in
// flight from recovering a device that deliberately went away (see
// recoverAndAnnounce). (#69)
func (r *Registry) Deregister(ctx context.Context, deviceID string) error {
	device, err := r.store.GetDevice(ctx, deviceID)
	if err != nil {
		return deviceErr(err)
	}

	// Close the subscription BEFORE writing the status. A heartbeat from this
	// device may still be in flight, and canRecover treats an open subscription
	// as "the device is connected, the hub merely lost patience". Closing first
	// means such a heartbeat either arrives while the device is still online (a
	// no-op) or finds no subscription to vouch for it — either way it cannot
	// resurrect a device that deliberately went away. (#69)
	r.removeSubscriber(deviceID)

	if err := r.store.UpdateDeviceStatus(ctx, deviceID, store.StatusOffline, device.LastIP, device.SSHPort); err != nil {
		return deviceErr(err)
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

// Leave permanently removes a device from the hub: it deletes the device row
// (cascading to shares, pairings, and pending invites), broadcasts a
// DeviceRemoved event to all other subscribers, and closes the caller's
// subscriber channel.
func (r *Registry) Leave(ctx context.Context, deviceID string) error {
	device, err := r.store.GetDevice(ctx, deviceID)
	if err != nil {
		return fmt.Errorf("leave: get device %q: %w", deviceID, err)
	}

	if err := r.store.DeleteDevice(ctx, deviceID); err != nil {
		return fmt.Errorf("leave: delete device %q: %w", deviceID, err)
	}

	r.BroadcastDeviceRemoved(device)

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

	r.broadcastLocked(event, excludeDevice)
}

// broadcastLocked is Broadcast's body for callers that already hold r.mu (read
// or write). It exists so a caller can publish an event in the same critical
// section that decided to publish it — see recoverAndAnnounce. Never call it
// without the lock: the sends below race Subscribe's channel replacement.
func (r *Registry) broadcastLocked(event *pb.Event, excludeDevice string) {
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

// MarkOffline demotes a stale device and broadcasts DeviceOffline, reporting
// whether the demotion actually happened. Used by the heartbeat monitor. The
// caller passes the *Device already in hand, avoiding a redundant store lookup.
//
// threshold is the staleness cut-off the caller selected the device with, and
// it is re-applied at write time. The monitor selects the stale set and writes
// each demotion afterwards; a heartbeat that arrives inside that window must
// keep the device online, or peers receive a DeviceOffline (and unmount) for a
// device that just proved it is alive — the #69 symptom. A device that is no
// longer stale (or no longer online) yields false and no broadcast. (#69)
func (r *Registry) MarkOffline(ctx context.Context, device *store.Device, threshold time.Time) (bool, error) {
	demoted, err := r.store.MarkOfflineIfStale(ctx, device.DeviceID, threshold)
	if err != nil {
		return false, err
	}
	if !demoted {
		return false, nil
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

	return true, nil
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
