package store

import "time"

// Device represents a registered device in the hub.
type Device struct {
	DeviceID      string
	Nickname      string
	LastIP        string
	SSHPort       int
	Status        string // "online" | "offline"
	LastHeartbeat time.Time
}

// Share represents a filesystem share exported by a device.
type Share struct {
	DeviceID       string
	Alias          string
	Permissions    string   // "ro" | "rw"
	AllowedDevices []string // stored as JSON array in DB
}

// Pairing represents a mutual trust relationship between two devices.
type Pairing struct {
	DeviceA  string
	DeviceB  string
	PairedAt time.Time
}

// PendingInvite represents an outstanding pairing invitation that has not yet
// been accepted or expired.
type PendingInvite struct {
	InviteCode    string
	FromDevice    string
	ToDevice      string
	FromPublicKey string
	ExpiresAt     time.Time
	Attempts      int
}
