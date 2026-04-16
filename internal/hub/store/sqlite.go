package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS devices (
    device_id      TEXT PRIMARY KEY,
    nickname       TEXT UNIQUE,
    last_ip        TEXT,
    ssh_port       INTEGER,
    status         TEXT,
    last_heartbeat DATETIME
);

CREATE TABLE IF NOT EXISTS shares (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    device_id       TEXT NOT NULL REFERENCES devices(device_id),
    alias           TEXT,
    permissions     TEXT,
    allowed_devices TEXT
);

CREATE TABLE IF NOT EXISTS pairings (
    device_a  TEXT,
    device_b  TEXT,
    paired_at DATETIME,
    PRIMARY KEY (device_a, device_b)
);

CREATE TABLE IF NOT EXISTS pending_invites (
    invite_code    TEXT PRIMARY KEY,
    from_device    TEXT,
    to_device      TEXT,
    from_public_key TEXT,
    expires_at     DATETIME,
    attempts       INTEGER DEFAULT 0
);
`

var _ Store = (*sqliteStore)(nil)

// sqliteStore is the SQLite-backed implementation of Store.
type sqliteStore struct {
	db *sql.DB
}

// NewSQLiteStore opens (or creates) a SQLite database at dbPath, runs the
// schema migrations, and returns a ready-to-use Store. Use ":memory:" for
// an in-process, ephemeral database suitable for tests.
func NewSQLiteStore(dbPath string) (Store, error) {
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite database %q: %w", dbPath, err)
	}

	db.SetMaxOpenConns(1)

	if _, err := db.Exec("PRAGMA foreign_keys = ON"); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable foreign keys: %w", err)
	}

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("run schema migrations: %w", err)
	}

	return &sqliteStore{db: db}, nil
}

// Close releases the underlying database connection.
func (s *sqliteStore) Close() error {
	return s.db.Close()
}

// --- Devices ---

// CreateDevice inserts a new device record.
func (s *sqliteStore) CreateDevice(ctx context.Context, d *Device) error {
	const q = `
		INSERT INTO devices (device_id, nickname, last_ip, ssh_port, status, last_heartbeat)
		VALUES (?, ?, ?, ?, ?, ?)`
	_, err := s.db.ExecContext(ctx, q,
		d.DeviceID, d.Nickname, d.LastIP, d.SSHPort, string(d.Status), d.LastHeartbeat.UTC(),
	)
	if err != nil {
		return fmt.Errorf("create device %q: %w", d.DeviceID, err)
	}
	return nil
}

// GetDevice retrieves a device by its device_id.
func (s *sqliteStore) GetDevice(ctx context.Context, deviceID string) (*Device, error) {
	const q = `
		SELECT device_id, nickname, last_ip, ssh_port, status, last_heartbeat
		FROM devices WHERE device_id = ?`
	row := s.db.QueryRowContext(ctx, q, deviceID)
	d, err := scanDevice(row)
	if err != nil {
		return nil, fmt.Errorf("get device %q: %w", deviceID, err)
	}
	return d, nil
}

// GetDeviceByNickname retrieves a device by its nickname.
func (s *sqliteStore) GetDeviceByNickname(ctx context.Context, nickname string) (*Device, error) {
	const q = `
		SELECT device_id, nickname, last_ip, ssh_port, status, last_heartbeat
		FROM devices WHERE nickname = ?`
	row := s.db.QueryRowContext(ctx, q, nickname)
	d, err := scanDevice(row)
	if err != nil {
		return nil, fmt.Errorf("get device by nickname %q: %w", nickname, err)
	}
	return d, nil
}

// ListOnlineDevices returns all devices whose status is "online".
func (s *sqliteStore) ListOnlineDevices(ctx context.Context) ([]*Device, error) {
	const q = `
		SELECT device_id, nickname, last_ip, ssh_port, status, last_heartbeat
		FROM devices WHERE status = 'online'`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list online devices: %w", err)
	}
	defer rows.Close()
	return scanDevices(rows)
}

// ListAllDevices returns all devices regardless of status.
func (s *sqliteStore) ListAllDevices(ctx context.Context) ([]*Device, error) {
	const q = `
		SELECT device_id, nickname, last_ip, ssh_port, status, last_heartbeat
		FROM devices`
	rows, err := s.db.QueryContext(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("list all devices: %w", err)
	}
	defer rows.Close()
	return scanDevices(rows)
}

// UpdateDeviceStatus sets the status, last_ip, and ssh_port for a device.
func (s *sqliteStore) UpdateDeviceStatus(ctx context.Context, deviceID string, status DeviceStatus, ip string, sshPort int) error {
	const q = `UPDATE devices SET status = ?, last_ip = ?, ssh_port = ? WHERE device_id = ?`
	_, err := s.db.ExecContext(ctx, q, string(status), ip, sshPort, deviceID)
	if err != nil {
		return fmt.Errorf("update device status %q: %w", deviceID, err)
	}
	return nil
}

// UpdateDeviceNickname changes the nickname of a device.
func (s *sqliteStore) UpdateDeviceNickname(ctx context.Context, deviceID string, nickname string) error {
	const q = `UPDATE devices SET nickname = ? WHERE device_id = ?`
	_, err := s.db.ExecContext(ctx, q, nickname, deviceID)
	if err != nil {
		return fmt.Errorf("update device nickname %q: %w", deviceID, err)
	}
	return nil
}

// UpdateHeartbeat records the current UTC time as the last_heartbeat for a device.
func (s *sqliteStore) UpdateHeartbeat(ctx context.Context, deviceID string) error {
	const q = `UPDATE devices SET last_heartbeat = ? WHERE device_id = ?`
	_, err := s.db.ExecContext(ctx, q, time.Now().UTC(), deviceID)
	if err != nil {
		return fmt.Errorf("update heartbeat for device %q: %w", deviceID, err)
	}
	return nil
}

// GetStaleDevices returns devices that are online but have not sent a heartbeat
// since threshold.
func (s *sqliteStore) GetStaleDevices(ctx context.Context, threshold time.Time) ([]*Device, error) {
	const q = `
		SELECT device_id, nickname, last_ip, ssh_port, status, last_heartbeat
		FROM devices WHERE status = 'online' AND last_heartbeat < ?`
	rows, err := s.db.QueryContext(ctx, q, threshold.UTC())
	if err != nil {
		return nil, fmt.Errorf("get stale devices: %w", err)
	}
	defer rows.Close()
	return scanDevices(rows)
}

// DeleteDevice removes a device record by device_id.
func (s *sqliteStore) DeleteDevice(ctx context.Context, deviceID string) error {
	const q = `DELETE FROM devices WHERE device_id = ?`
	_, err := s.db.ExecContext(ctx, q, deviceID)
	if err != nil {
		return fmt.Errorf("delete device %q: %w", deviceID, err)
	}
	return nil
}

// --- Shares ---

// SetShares replaces all shares for the given device inside a single
// transaction: existing shares are deleted and the new ones inserted.
func (s *sqliteStore) SetShares(ctx context.Context, deviceID string, shares []*Share) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction for SetShares: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if _, err := tx.ExecContext(ctx, `DELETE FROM shares WHERE device_id = ?`, deviceID); err != nil {
		return fmt.Errorf("delete existing shares for device %q: %w", deviceID, err)
	}

	const ins = `INSERT INTO shares (device_id, alias, permissions, allowed_devices) VALUES (?, ?, ?, ?)`
	for _, sh := range shares {
		adJSON, err := json.Marshal(sh.AllowedDevices)
		if err != nil {
			return fmt.Errorf("marshal allowed_devices for share %q: %w", sh.Alias, err)
		}
		if _, err := tx.ExecContext(ctx, ins, deviceID, sh.Alias, string(sh.Permissions), string(adJSON)); err != nil {
			return fmt.Errorf("insert share %q for device %q: %w", sh.Alias, deviceID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit SetShares for device %q: %w", deviceID, err)
	}
	return nil
}

// GetShares returns all shares registered for the given device.
func (s *sqliteStore) GetShares(ctx context.Context, deviceID string) ([]*Share, error) {
	const q = `SELECT device_id, alias, permissions, allowed_devices FROM shares WHERE device_id = ?`
	rows, err := s.db.QueryContext(ctx, q, deviceID)
	if err != nil {
		return nil, fmt.Errorf("get shares for device %q: %w", deviceID, err)
	}
	defer rows.Close()

	var shares []*Share
	for rows.Next() {
		var sh Share
		var permStr string
		var adJSON string
		if err := rows.Scan(&sh.DeviceID, &sh.Alias, &permStr, &adJSON); err != nil {
			return nil, fmt.Errorf("scan share row: %w", err)
		}
		sh.Permissions = Permission(permStr)
		if err := json.Unmarshal([]byte(adJSON), &sh.AllowedDevices); err != nil {
			return nil, fmt.Errorf("unmarshal allowed_devices: %w", err)
		}
		shares = append(shares, &sh)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate share rows: %w", err)
	}
	return shares, nil
}

// GetSharesForDevices returns shares for the given deviceIDs in a single
// query, grouped by device_id. Devices with no shares are absent from the
// returned map.
func (s *sqliteStore) GetSharesForDevices(ctx context.Context, deviceIDs []string) (map[string][]*Share, error) {
	result := make(map[string][]*Share, len(deviceIDs))
	if len(deviceIDs) == 0 {
		return result, nil
	}

	placeholders := make([]string, len(deviceIDs))
	args := make([]any, len(deviceIDs))
	for i, id := range deviceIDs {
		placeholders[i] = "?"
		args[i] = id
	}
	q := "SELECT device_id, alias, permissions, allowed_devices FROM shares WHERE device_id IN (" +
		strings.Join(placeholders, ",") + ")"

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("get shares for devices: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var sh Share
		var permStr string
		var adJSON string
		if err := rows.Scan(&sh.DeviceID, &sh.Alias, &permStr, &adJSON); err != nil {
			return nil, fmt.Errorf("scan share row: %w", err)
		}
		sh.Permissions = Permission(permStr)
		if err := json.Unmarshal([]byte(adJSON), &sh.AllowedDevices); err != nil {
			return nil, fmt.Errorf("unmarshal allowed_devices: %w", err)
		}
		result[sh.DeviceID] = append(result[sh.DeviceID], &sh)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate share rows: %w", err)
	}
	return result, nil
}

// --- Pairings ---

// CreatePairing records a trust relationship between two devices.
func (s *sqliteStore) CreatePairing(ctx context.Context, deviceA, deviceB string) error {
	const q = `INSERT INTO pairings (device_a, device_b, paired_at) VALUES (?, ?, ?)`
	_, err := s.db.ExecContext(ctx, q, deviceA, deviceB, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("create pairing (%q, %q): %w", deviceA, deviceB, err)
	}
	return nil
}

// GetPairing returns the pairing record for (deviceA, deviceB). The lookup is
// order-sensitive.
func (s *sqliteStore) GetPairing(ctx context.Context, deviceA, deviceB string) (*Pairing, error) {
	const q = `SELECT device_a, device_b, paired_at FROM pairings WHERE device_a = ? AND device_b = ?`
	row := s.db.QueryRowContext(ctx, q, deviceA, deviceB)
	var p Pairing
	var pairedAtStr string
	if err := row.Scan(&p.DeviceA, &p.DeviceB, &pairedAtStr); err != nil {
		return nil, fmt.Errorf("get pairing (%q, %q): %w", deviceA, deviceB, err)
	}
	t, err := parseDateTime(pairedAtStr)
	if err != nil {
		return nil, fmt.Errorf("parse paired_at: %w", err)
	}
	p.PairedAt = t
	return &p, nil
}

// IsPaired reports whether a pairing exists between two devices, checking both
// orderings.
func (s *sqliteStore) IsPaired(ctx context.Context, deviceA, deviceB string) (bool, error) {
	const q = `
		SELECT COUNT(*) FROM pairings
		WHERE (device_a = ? AND device_b = ?) OR (device_a = ? AND device_b = ?)`
	var count int
	if err := s.db.QueryRowContext(ctx, q, deviceA, deviceB, deviceB, deviceA).Scan(&count); err != nil {
		return false, fmt.Errorf("is paired (%q, %q): %w", deviceA, deviceB, err)
	}
	return count > 0, nil
}

// --- Pending Invites ---

// CreateInvite stores a new pending invite.
func (s *sqliteStore) CreateInvite(ctx context.Context, inv *PendingInvite) error {
	const q = `
		INSERT INTO pending_invites (invite_code, from_device, to_device, from_public_key, expires_at, attempts)
		VALUES (?, ?, ?, ?, ?, ?)`
	_, err := s.db.ExecContext(ctx, q,
		inv.InviteCode, inv.FromDevice, inv.ToDevice, inv.FromPublicKey,
		inv.ExpiresAt.UTC(), inv.Attempts,
	)
	if err != nil {
		return fmt.Errorf("create invite %q: %w", inv.InviteCode, err)
	}
	return nil
}

// GetInvite retrieves a pending invite by its invite code.
func (s *sqliteStore) GetInvite(ctx context.Context, code string) (*PendingInvite, error) {
	const q = `
		SELECT invite_code, from_device, to_device, from_public_key, expires_at, attempts
		FROM pending_invites WHERE invite_code = ?`
	row := s.db.QueryRowContext(ctx, q, code)
	var inv PendingInvite
	var expiresAt string
	if err := row.Scan(&inv.InviteCode, &inv.FromDevice, &inv.ToDevice, &inv.FromPublicKey, &expiresAt, &inv.Attempts); err != nil {
		return nil, fmt.Errorf("get invite %q: %w", code, err)
	}
	t, err := parseDateTime(expiresAt)
	if err != nil {
		return nil, fmt.Errorf("parse expires_at for invite %q: %w", code, err)
	}
	inv.ExpiresAt = t
	return &inv, nil
}

// IncrementInviteAttempts atomically increments the attempts counter for an invite.
func (s *sqliteStore) IncrementInviteAttempts(ctx context.Context, code string) error {
	const q = `UPDATE pending_invites SET attempts = attempts + 1 WHERE invite_code = ?`
	_, err := s.db.ExecContext(ctx, q, code)
	if err != nil {
		return fmt.Errorf("increment invite attempts %q: %w", code, err)
	}
	return nil
}

// DeleteInvite removes a pending invite by its invite code.
func (s *sqliteStore) DeleteInvite(ctx context.Context, code string) error {
	const q = `DELETE FROM pending_invites WHERE invite_code = ?`
	_, err := s.db.ExecContext(ctx, q, code)
	if err != nil {
		return fmt.Errorf("delete invite %q: %w", code, err)
	}
	return nil
}

// DeleteExpiredInvites removes all invites whose expires_at timestamp is in
// the past.
func (s *sqliteStore) DeleteExpiredInvites(ctx context.Context) error {
	const q = `DELETE FROM pending_invites WHERE expires_at < ?`
	_, err := s.db.ExecContext(ctx, q, time.Now().UTC())
	if err != nil {
		return fmt.Errorf("delete expired invites: %w", err)
	}
	return nil
}

// --- helpers ---

// rowScanner is satisfied by both *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// scanDevice scans a single device row.
func scanDevice(row rowScanner) (*Device, error) {
	var d Device
	var statusStr string
	var lastHeartbeat string
	if err := row.Scan(&d.DeviceID, &d.Nickname, &d.LastIP, &d.SSHPort, &statusStr, &lastHeartbeat); err != nil {
		return nil, err
	}
	d.Status = DeviceStatus(statusStr)
	t, err := parseDateTime(lastHeartbeat)
	if err != nil {
		return nil, fmt.Errorf("parse last_heartbeat: %w", err)
	}
	d.LastHeartbeat = t
	return &d, nil
}

// scanDevices iterates over rows and scans each into a Device.
func scanDevices(rows *sql.Rows) ([]*Device, error) {
	var devices []*Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, fmt.Errorf("scan device row: %w", err)
		}
		devices = append(devices, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate device rows: %w", err)
	}
	return devices, nil
}

// parseDateTime attempts to parse a datetime string using several common
// formats that SQLite may produce.
func parseDateTime(s string) (time.Time, error) {
	formats := []string{
		time.RFC3339Nano,
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02 15:04:05.999999999+00:00",
		"2006-01-02 15:04:05+00:00",
		"2006-01-02 15:04:05.999999999Z07:00",
		"2006-01-02 15:04:05Z07:00",
		"2006-01-02T15:04:05.999999999Z",
		"2006-01-02T15:04:05Z",
		"2006-01-02 15:04:05",
	}
	for _, f := range formats {
		if t, err := time.Parse(f, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("cannot parse datetime %q", s)
}
