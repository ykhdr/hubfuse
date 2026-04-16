package store

import (
	"context"
	"time"
)

// Store defines the data access operations for the hub database.
type Store interface {
	// Devices

	// CreateDevice inserts a new device record. Returns an error if a device
	// with the same device_id or nickname already exists.
	CreateDevice(ctx context.Context, d *Device) error

	// GetDevice retrieves a device by its device_id. Returns nil and an error
	// if no matching device exists.
	GetDevice(ctx context.Context, deviceID string) (*Device, error)

	// GetDeviceByNickname retrieves a device by its human-readable nickname.
	// Returns nil and an error if no matching device exists.
	GetDeviceByNickname(ctx context.Context, nickname string) (*Device, error)

	// ListOnlineDevices returns all devices whose status is "online".
	ListOnlineDevices(ctx context.Context) ([]*Device, error)

	// ListAllDevices returns all devices regardless of status.
	ListAllDevices(ctx context.Context) ([]*Device, error)

	// UpdateDeviceStatus sets the status, last_ip, and ssh_port for a device.
	UpdateDeviceStatus(ctx context.Context, deviceID string, status string, ip string, sshPort int) error

	// UpdateDeviceNickname changes the nickname of a device.
	UpdateDeviceNickname(ctx context.Context, deviceID string, nickname string) error

	// UpdateHeartbeat records the current time as the last_heartbeat for a device.
	UpdateHeartbeat(ctx context.Context, deviceID string) error

	// GetStaleDevices returns devices whose status is "online" and whose
	// last_heartbeat is earlier than threshold.
	GetStaleDevices(ctx context.Context, threshold time.Time) ([]*Device, error)

	// DeleteDevice removes a device and all its associated data.
	DeleteDevice(ctx context.Context, deviceID string) error

	// Shares

	// SetShares replaces all shares for the given device with the provided
	// slice. Any previously stored shares for deviceID are deleted first.
	SetShares(ctx context.Context, deviceID string, shares []*Share) error

	// GetShares returns all shares registered for the given device.
	GetShares(ctx context.Context, deviceID string) ([]*Share, error)

	// GetSharesForDevices returns shares for every device ID in the given
	// slice in a single query. The result is keyed by device_id; missing
	// keys mean no shares for that device.
	GetSharesForDevices(ctx context.Context, deviceIDs []string) (map[string][]*Share, error)

	// Pairings

	// CreatePairing records a bidirectional trust relationship between two
	// devices identified by deviceA and deviceB.
	CreatePairing(ctx context.Context, deviceA, deviceB string) error

	// GetPairing returns the pairing record for the given device pair. The
	// lookup is order-sensitive: call IsPaired if order is unknown.
	GetPairing(ctx context.Context, deviceA, deviceB string) (*Pairing, error)

	// IsPaired reports whether a pairing exists between the two devices,
	// checking both (deviceA, deviceB) and (deviceB, deviceA) orderings.
	IsPaired(ctx context.Context, deviceA, deviceB string) (bool, error)

	// Pending Invites

	// CreateInvite stores a new pending invite.
	CreateInvite(ctx context.Context, inv *PendingInvite) error

	// GetInvite retrieves a pending invite by its invite code.
	GetInvite(ctx context.Context, code string) (*PendingInvite, error)

	// IncrementInviteAttempts atomically increments the attempts counter for
	// the invite identified by code.
	IncrementInviteAttempts(ctx context.Context, code string) error

	// DeleteInvite removes a pending invite by its invite code.
	DeleteInvite(ctx context.Context, code string) error

	// DeleteExpiredInvites removes all invites whose expires_at is in the past.
	DeleteExpiredInvites(ctx context.Context) error

	// Close releases the underlying database connection.
	Close() error
}
