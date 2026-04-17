package helpers

import (
	"strings"
	"testing"
)

// DeviceRow is one parsed line from `hubfuse devices` output. Columns are
// DEVICE_ID, NICKNAME, STATUS, IP (left-aligned table; see cmd/hubfuse/main.go
// for the exact format string).
type DeviceRow struct {
	DeviceID string
	Nickname string
	Status   string
	IP       string
}

// TryDevices runs `hubfuse devices` on the agent and returns the combined
// output and whether the command exited zero. Unlike run/runExpectFail, it
// NEVER fails the test — callers that only care about the success path should
// assert on the returned bool themselves.
func (a *Agent) TryDevices(t *testing.T) (string, bool) {
	t.Helper()
	return a.tryRun(t, "devices")
}

// PeerStatus returns the parsed row for the given nickname, if present.
// Status strings are "online", "offline", or "registered". The second return
// is false when the hub is unreachable OR the peer is not listed.
func (a *Agent) PeerStatus(t *testing.T, nickname string) (DeviceRow, bool) {
	t.Helper()
	out, ok := a.TryDevices(t)
	if !ok {
		return DeviceRow{}, false
	}
	return ParseDeviceRow(out, nickname)
}

// ParseDeviceRow locates a row for `nickname` in `devices` output. It is
// tolerant of the exact column widths (skips lines that do not parse) and is
// case-sensitive on the nickname match.
func ParseDeviceRow(devicesOutput, nickname string) (DeviceRow, bool) {
	for _, line := range strings.Split(devicesOutput, "\n") {
		fields := strings.Fields(line)
		// Header line: "DEVICE ID NICKNAME STATUS IP" — skip.
		// Separator line: all dashes — skip.
		// Data line: 3 or 4 fields (IP may be empty for offline devices).
		if len(fields) < 3 {
			continue
		}
		// Skip header: first data row starts with a non-"DEVICE" id.
		if fields[0] == "DEVICE" {
			continue
		}
		// Nickname is field[1] (since DEVICE_ID is one token).
		if fields[1] != nickname {
			continue
		}
		row := DeviceRow{
			DeviceID: fields[0],
			Nickname: fields[1],
			Status:   fields[2],
		}
		if len(fields) >= 4 {
			row.IP = fields[3]
		}
		return row, true
	}
	return DeviceRow{}, false
}
