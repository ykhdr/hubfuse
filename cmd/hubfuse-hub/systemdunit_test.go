package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestHubUnit_PinsEverySettingThatEarnedItsPlace asserts the unit setting by
// setting with the reason each is there. A generated-file test is the only kind
// available — systemd cannot be driven in CI — so each assertion names what
// would break rather than merely that a key is present.
func TestHubUnit_PinsEverySettingThatEarnedItsPlace(t *testing.T) {
	body, err := hubUnitBody("/home/alice/go/bin/hubfuse-hub")
	require.NoError(t, err)
	unit := string(body)

	assert.Contains(t, unit, "ExecStart=/home/alice/go/bin/hubfuse-hub start")
	assert.Contains(t, unit, "Type=simple")

	assert.Contains(t, unit, "Restart=on-failure",
		"NOT always: `hubfuse-hub stop` exits zero on a healthy shutdown, and the hub goes out "+
			"of its way to keep that true — Restart=always would make the stop command a no-op, "+
			"the shape #98 fixed on launchd")
	assert.NotContains(t, unit, "Restart=always")

	assert.Contains(t, unit, "RestartSec=30",
		"#98's ThrottleInterval argument")
}

// TestHubUnit_HasTheInstallSectionThatMakesItStartAtBoot is separate because it
// is the setting whose absence is invisible at install time: `systemctl --user
// enable --now` starts the unit in the current session regardless, so a missing
// or system-flavoured [Install] shows up only after a reboot — with every device
// on the hub reading offline and nothing saying why.
func TestHubUnit_HasTheInstallSectionThatMakesItStartAtBoot(t *testing.T) {
	body, err := hubUnitBody("/home/alice/go/bin/hubfuse-hub")
	require.NoError(t, err)
	unit := string(body)

	assert.Contains(t, unit, "[Install]")
	assert.Contains(t, unit, "WantedBy=default.target",
		"default.target is the user manager's boot target")
	assert.NotContains(t, unit, "multi-user.target",
		"that is a SYSTEM target — linking a user unit to it is the silent version of this bug")
}

// TestHubUnit_OmitsWhatTheHubDoesNotNeed records the two settings the agent's
// unit has and this one deliberately does not, so neither is added later out of
// symmetry.
//
// Environment=PATH: the hub shells out to nothing — no exec.Command or
// exec.LookPath anywhere under internal/hub or cmd/hubfuse-hub — so a PATH would
// be cargo copied from the agent, which needs one because it runs sshfs.
//
// KillMode=mixed: the hub spawns no children, so systemd's default control-group
// kill has nothing extra to signal. The agent needs `mixed` because sshfs
// self-daemonizes into its cgroup and the default would tear live mounts down
// instead of letting the agent unmount them in order (#67).
func TestHubUnit_OmitsWhatTheHubDoesNotNeed(t *testing.T) {
	body, err := hubUnitBody("/home/alice/go/bin/hubfuse-hub")
	require.NoError(t, err)
	unit := string(body)

	assert.NotContains(t, unit, "Environment=PATH",
		"the hub execs nothing; a PATH here would be cargo from the agent's unit")
	assert.NotContains(t, unit, "KillMode",
		"the hub spawns no children, so the default control-group kill is correct")
	assert.NotContains(t, unit, "network-online.target",
		"inert in a user manager (LoadState=not-found)")
	assert.NotContains(t, unit, "StandardOutput=",
		"the journal is systemd's default and the idiomatic answer on Linux")
}

// TestSystemdEscapeExec covers systemd's own escaping hazards, which differ from
// the XML ones the macOS plist needed: ExecStart splits on whitespace unless
// quoted, and '%' introduces a specifier that must be doubled. Either mistake
// makes systemd refuse or misread the unit while install-service reports
// success.
func TestSystemdEscapeExec(t *testing.T) {
	cases := []struct {
		name, in, want, why string
	}{
		{"ordinary path untouched", "/usr/bin/hubfuse-hub", "/usr/bin/hubfuse-hub",
			"escaping must not disturb the common case"},
		{"percent doubled", "/opt/50%off/hubfuse-hub", "/opt/50%%off/hubfuse-hub",
			"a bare % is a systemd specifier, not a literal"},
		{"whitespace quoted", "/home/John Doe/hubfuse-hub", `"/home/John Doe/hubfuse-hub"`,
			"ExecStart splits on whitespace"},
		{"percent inside quoted path doubled once", "/home/John Doe/50%/h", `"/home/John Doe/50%%/h"`,
			"doubling must precede quoting"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, systemdEscapeExec(c.in), c.why)
		})
	}
}

// TestHubUnit_EscapesAPathThatWouldBreakTheUnit drives the escaping through the
// template, because a helper that is correct but unwired is the same bug.
func TestHubUnit_EscapesAPathThatWouldBreakTheUnit(t *testing.T) {
	body, err := hubUnitBody("/home/John Doe/go/bin/hubfuse-hub")
	require.NoError(t, err)

	assert.Contains(t, string(body), `ExecStart="/home/John Doe/go/bin/hubfuse-hub" start`)
}

// TestRunInstallService_RefusesOffLinux pins the refusal and that it names the
// platform it is refusing on.
func TestRunInstallService_RefusesOffLinux(t *testing.T) {
	var out bytes.Buffer
	err := runInstallService(&out, "darwin", false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "Linux-only")
	assert.Contains(t, err.Error(), "darwin")
	assert.Contains(t, err.Error(), "systemd", "and say what the requirement actually is")
	assert.Empty(t, out.String(), "a refused install must not print next steps")
}

// TestInstallServiceNextSteps_SaysWhatTheInstallCannotDoForYou pins the
// lingering instruction, which is why this text is generated rather than left to
// the README: `enable --now` succeeds without it, so an operator who stops there
// gets a working hub today and every device offline after the next reboot.
func TestInstallServiceNextSteps_SaysWhatTheInstallCannotDoForYou(t *testing.T) {
	got := installServiceNextSteps(
		"/home/alice/.config/systemd/user/hubfuse-hub.service",
		"hubfuse-hub.service",
		"/home/alice/go/bin/hubfuse-hub",
	)
	low := strings.ToLower(got)

	assert.Contains(t, got, "/home/alice/go/bin/hubfuse-hub",
		"the operator must see which binary was wired in")
	assert.Contains(t, got, "systemctl --user enable --now hubfuse-hub.service")
	assert.Contains(t, got, "loginctl enable-linger",
		"without lingering the unit does not survive a reboot and the enable above still "+
			"reported success")
	assert.Contains(t, low, "required")
	assert.Contains(t, low, "offline",
		"the consequence for a hub is every device going dark, which is worth naming")
	assert.Contains(t, low, "sudo",
		"over SSH polkit has no active session to authorise against")
	assert.Contains(t, got, "journalctl --user -u hubfuse-hub.service",
		"the macOS plist logs to a FILE, so the destination has to be stated")
}
