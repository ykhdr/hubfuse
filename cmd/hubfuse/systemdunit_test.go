package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAgentUnit_PinsEverySettingThatEarnedItsPlace asserts the unit's content
// setting by setting, with the reason each is there.
//
// A generated-file test is the only kind available — systemd cannot be driven in
// CI — so the assertions have to name what would break rather than merely that a
// key is present. Task 3 adds `systemd-analyze --user verify` on top, which
// catches syntax a substring match cannot.
func TestAgentUnit_PinsEverySettingThatEarnedItsPlace(t *testing.T) {
	body, err := agentUnitBody("/home/alice/go/bin/hubfuse")
	require.NoError(t, err)
	unit := string(body)

	assert.Contains(t, unit, "ExecStart=/home/alice/go/bin/hubfuse start",
		"the unit runs the foreground start; --daemon would double-fork and leave systemd "+
			"supervising nothing")
	assert.Contains(t, unit, "Type=simple")

	assert.Contains(t, unit, "Restart=on-failure",
		"NOT always: a clean exit is a deliberate stop, and relaunching it makes `hubfuse stop` "+
			"a no-op — the launchd half of that was #98")
	assert.NotContains(t, unit, "Restart=always")

	assert.Contains(t, unit, "RestartSec=30",
		"#98's ThrottleInterval argument: a genuine crash loop costs two launches a minute")

	assert.Contains(t, unit, "KillMode=mixed",
		"systemd's default signals the whole cgroup, which would SIGTERM live sshfs mounts out "+
			"from under the agent instead of letting Shutdown's UnmountAllForce unmount them — "+
			"#67's stale-mount symptom reintroduced by the service manager")

	assert.Contains(t, unit, "Environment=PATH=",
		"a lingering manager started at boot gets systemd's compiled-in default, and a mount "+
			"tool may live outside it")

	// %h is systemd's home-directory specifier and must reach the unit INTACT.
	// It is the one place a '%' is deliberate, so the PATH deliberately does not
	// go through systemdEscapeExec — routing it there would double the sign and
	// turn the expansion into the literal text "%h", silently dropping
	// ~/.local/bin from the daemon's PATH.
	assert.Contains(t, unit, "Environment=PATH=%h/.local/bin:",
		"%h must survive unescaped; doubling it would make the entry a literal and drop "+
			"~/.local/bin from the search path")
	assert.NotContains(t, unit, "%%h",
		"the PATH must not be run through the ExecStart escaper")
}

// TestAgentUnit_HasTheInstallSectionThatMakesItStartAtBoot is separate from the
// settings test on purpose: it is the one whose absence is INVISIBLE at install
// time.
//
// A user manager has no multi-user.target. A unit written with system-unit
// muscle memory, or with no [Install] at all, links into a target that never
// activates — and `systemctl --user enable --now` still succeeds, because --now
// starts it in the current session regardless. The operator sees a working agent
// and an absent one after the next reboot, which is precisely the failure #117
// exists to remove.
func TestAgentUnit_HasTheInstallSectionThatMakesItStartAtBoot(t *testing.T) {
	body, err := agentUnitBody("/home/alice/go/bin/hubfuse")
	require.NoError(t, err)
	unit := string(body)

	assert.Contains(t, unit, "[Install]",
		"without this section `systemctl --user enable` has nothing to link and the unit never "+
			"starts at boot")
	assert.Contains(t, unit, "WantedBy=default.target",
		"default.target is the user manager's boot target; multi-user.target does not exist there")
	assert.NotContains(t, unit, "multi-user.target",
		"that is a SYSTEM target — linking a user unit to it is the silent version of this bug")
}

// TestAgentUnit_OmitsWhatWasMeasuredInert records two absences so neither is
// added back later by symmetry with some other project's unit file.
//
// After=network-online.target: measured on the target platform, that unit is
// LoadState=not-found in a user manager, so the line orders nothing. The agent
// survives a hubless start anyway (#74), so there is nothing to order against.
// Shipping a no-op with a comment implying otherwise teaches a future reader
// something false — which this repo has had to retract twice already.
//
// StandardOutput=append:: logs go to the journal, systemd's own default. A file
// destination would need the same path escaping ExecStart does, for no gain over
// `journalctl --user -u`.
func TestAgentUnit_OmitsWhatWasMeasuredInert(t *testing.T) {
	body, err := agentUnitBody("/home/alice/go/bin/hubfuse")
	require.NoError(t, err)
	unit := string(body)

	assert.NotContains(t, unit, "network-online.target",
		"inert in a user manager (LoadState=not-found), and the agent needs no such ordering")
	assert.NotContains(t, unit, "StandardOutput=",
		"the journal is the default and the idiomatic answer on Linux")
}

// TestSystemdEscapeExec covers the systemd analogue of the XML escaping the
// plist needed, and the hazards are different ones.
//
// systemd splits ExecStart on whitespace unless the argument is quoted, and '%'
// introduces a specifier — a literal one must be doubled or systemd substitutes
// something unintended or refuses the unit outright. Either way install-service
// reports success and the service never runs, which is the failure class this
// project keeps paying for.
func TestSystemdEscapeExec(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
		why  string
	}{
		{
			name: "ordinary path is untouched",
			in:   "/home/alice/go/bin/hubfuse",
			want: "/home/alice/go/bin/hubfuse",
			why:  "escaping must not disturb the overwhelmingly common case",
		},
		{
			name: "percent is doubled",
			in:   "/opt/100%pure/hubfuse",
			want: "/opt/100%%pure/hubfuse",
			why:  "a bare % is a systemd specifier, not a literal",
		},
		{
			name: "whitespace is quoted",
			in:   "/home/John Doe/bin/hubfuse",
			want: `"/home/John Doe/bin/hubfuse"`,
			why:  "ExecStart splits on whitespace, so an unquoted space becomes a second argument",
		},
		{
			name: "percent inside a quoted path is doubled once, not twice",
			in:   "/home/John Doe/100%/hubfuse",
			want: `"/home/John Doe/100%%/hubfuse"`,
			why:  "doubling must happen before quoting, or the quoting's own output gets doubled",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, systemdEscapeExec(c.in), c.why)
		})
	}
}

// TestAgentUnit_EscapesAPathThatWouldBreakTheUnit drives the escaping through
// the template, because a helper that is correct but unwired is the same bug.
func TestAgentUnit_EscapesAPathThatWouldBreakTheUnit(t *testing.T) {
	body, err := agentUnitBody("/home/John Doe/go/bin/hubfuse")
	require.NoError(t, err)

	assert.Contains(t, string(body), `ExecStart="/home/John Doe/go/bin/hubfuse" start`,
		"an unquoted space would make systemd read \"Doe/go/bin/hubfuse\" as an argument")
}

// TestRunInstallService_RefusesOffLinux pins the refusal, and that it points at
// the command that DOES work on the platform it is refusing on — the same
// contract install-agent's refusal has, now that the two name each other.
func TestRunInstallService_RefusesOffLinux(t *testing.T) {
	var out bytes.Buffer
	err := runInstallService(&out, "darwin", false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "Linux-only")
	assert.Contains(t, err.Error(), "darwin", "the refusal must name the platform it is refusing on")
	assert.Contains(t, err.Error(), "install-agent", "and point at what does work there")
	assert.Empty(t, out.String(), "a refused install must not print next steps")
}

// TestInstallServiceNextSteps_SaysWhatTheInstallCannotDoForYou pins the lingering
// instruction, which is the whole reason this text is generated rather than left
// to the README.
//
// `systemctl --user enable --now` succeeds whether or not lingering is enabled.
// An operator who stops there gets a working agent today and an absent one after
// the next reboot — the exact failure #117 exists to remove, arrived at through a
// successful-looking install. The sudo note is there because polkit has no active
// session to authorise against over SSH, which is how a Linux server is usually
// administered.
func TestInstallServiceNextSteps_SaysWhatTheInstallCannotDoForYou(t *testing.T) {
	got := installServiceNextSteps(
		"/home/alice/.config/systemd/user/hubfuse-agent.service",
		"hubfuse-agent.service",
		"/home/alice/go/bin/hubfuse",
	)
	low := strings.ToLower(got)

	assert.Contains(t, got, "/home/alice/go/bin/hubfuse",
		"the operator must see which binary was wired in")
	assert.Contains(t, got, "systemctl --user enable --now hubfuse-agent.service")
	assert.Contains(t, got, "loginctl enable-linger",
		"without lingering the unit does not survive a reboot, and the enable above still "+
			"reported success — so this cannot be left to the README")
	assert.Contains(t, low, "required",
		"and it has to read as required, not as a suggestion")
	assert.Contains(t, low, "sudo",
		"over SSH polkit has no active session, which is how this is usually administered")
	assert.Contains(t, got, "journalctl --user -u hubfuse-agent.service",
		"the launchd plist logs to a FILE, so someone who read that doc will go looking for "+
			"~/.hubfuse/agent.log unless told otherwise")
}
