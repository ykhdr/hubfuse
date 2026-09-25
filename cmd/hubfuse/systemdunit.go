package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"

	"github.com/spf13/cobra"
)

// systemdAgentUnit is the unit's filename and the handle systemctl needs, so it
// is printed verbatim rather than left for the operator to derive.
const systemdAgentUnit = "hubfuse-agent.service"

// agentUnitTemplate is the systemd user unit install-service writes.
//
// Every setting below is here for a measured or cited reason, and two ABSENCES
// are as deliberate as the presences — see the comments on each.
//
//   - [Install] WantedBy=default.target is the one whose absence would defeat
//     the whole point silently. A user manager has no multi-user.target, so a
//     unit written with system-unit muscle memory links into a target that never
//     activates and nothing starts at boot — while `systemctl --user enable
//     --now` still succeeds in the interactive session, so the install looks
//     fine and only a real reboot finds it (#117).
//
//   - Restart=on-failure, NOT always. A clean exit means a deliberate stop, and
//     relaunching it makes `hubfuse stop` a no-op — the launchd half of this was
//     #98. Since #74 the daemon retries an unreachable hub in-process instead of
//     exiting, so a zero exit genuinely is "stop" and nothing else.
//
//   - RestartSec=30 is #98's ThrottleInterval argument unchanged: a genuine
//     crash loop costs two launches a minute rather than six.
//
//   - KillMode=mixed, because systemd's default (control-group) signals every
//     process in the cgroup. sshfs is spawned without -f (see mounter.go), so it
//     self-daemonizes but stays in this unit's cgroup, and a plain `systemctl
//     --user restart` would SIGTERM the live mounts out from under the agent
//     instead of letting Shutdown's UnmountAllForce unmount them in order. That
//     would leave "Transport endpoint is not connected" mounts behind on every
//     restart — #67's symptom, reintroduced by the service manager. `mixed`
//     signals the main process only.
//
//   - Environment=PATH is kept, but for a NARROWER reason than it is tempting to
//     give. It is NOT the macOS failure of #115: measured on Linux, systemd's
//     user manager already exports /usr/local/bin and /usr/bin, and sshfs lives
//     at /usr/bin/sshfs. What this covers is (a) a LINGERING manager started at
//     boot, which gets systemd's compiled-in default rather than a login
//     session's imported environment, and (b) a mount tool installed outside
//     those two directories — ~/.local/bin, /snap/bin, /opt/*/bin.
//
//   - NO After=network-online.target. Measured inert here: in a user manager
//     that unit is LoadState=not-found, so the line orders nothing. The agent
//     survives a hubless start anyway (#74), so there is nothing to order
//     against — and shipping a no-op with a comment implying otherwise is how a
//     future reader learns something false.
//
//   - NO StandardOutput=append:. Logs go to the journal, systemd's own default:
//     it needs no path escaping and it is what an operator on Linux will reach
//     for. install-service prints the journalctl line, because the launchd plist
//     puts logs in a FILE and someone who has read that doc would otherwise go
//     looking for ~/.hubfuse/agent.log.
const agentUnitTemplate = `[Unit]
Description=HubFuse agent
Documentation=https://github.com/ykhdr/hubfuse

[Service]
Type=simple
ExecStart={{.Exec}} start
Restart=on-failure
RestartSec=30
KillMode=mixed
Environment=PATH={{.Path}}

[Install]
WantedBy=default.target
`

// agentUnitPath is the PATH the unit exports. Extracted so the test names the
// same value the template does rather than restating a literal.
const agentUnitPath = "%h/.local/bin:/usr/local/bin:/usr/bin:/bin:/usr/local/sbin:/usr/sbin:/sbin:/snap/bin"

// systemdEscapeExec renders path for use in an ExecStart= line.
//
// This is the systemd analogue of xmlEscape, and the hazards are real but
// different from XML's. systemd splits ExecStart on whitespace unless the
// argument is quoted, and '%' introduces a specifier — a literal one must be
// doubled or systemd either substitutes something unintended or fails to parse
// the unit. Either way the operator sees install-service report success and the
// service never run, which is the failure class this project keeps paying for.
//
// The '%' doubling comes FIRST: doing it after quoting would also double any '%'
// this function introduced, and doing it never is how "/opt/100%%pure/bin" turns
// into a unit systemd refuses.
func systemdEscapeExec(path string) string {
	escaped := strings.ReplaceAll(path, "%", "%%")
	if strings.ContainsAny(escaped, " \t") {
		// systemd's own quoting: double quotes with backslash escapes inside.
		escaped = strings.ReplaceAll(escaped, `\`, `\\`)
		escaped = strings.ReplaceAll(escaped, `"`, `\"`)
		return `"` + escaped + `"`
	}
	return escaped
}

// agentUnitBody renders the unit for execPath.
func agentUnitBody(execPath string) ([]byte, error) {
	tmpl, err := template.New("unit").Parse(agentUnitTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse unit template: %w", err)
	}
	var buf strings.Builder
	data := struct {
		Exec string
		Path string
	}{
		Exec: systemdEscapeExec(execPath),
		Path: agentUnitPath,
	}
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("render unit: %w", err)
	}
	return []byte(buf.String()), nil
}

// systemdUserUnitPath is where a user manager looks for units it can enable.
func systemdUserUnitPath(home, unit string) string {
	return filepath.Join(home, ".config", "systemd", "user", unit)
}

// installServiceCmd implements: hubfuse install-service
func installServiceCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "install-service",
		Short: "Install a systemd user unit so the agent starts at boot",
		Long: "Writes a systemd user unit to ~/.config/systemd/user so the agent starts with the " +
			"machine and is restarted if it fails.\n\n" +
			"It is a USER unit rather than a system one because the agent is per-user by " +
			"construction: it mounts into your home directory and holds your SSH keys. The cost " +
			"is that a user manager stops at logout unless lingering is enabled, so " +
			"\"loginctl enable-linger\" is a required step and not a footnote — without it the " +
			"agent will not come back after a reboot, and the install will still look successful.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInstallService(cmd.OutOrStdout(), runtime.GOOS, force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing unit")
	return cmd
}

// runInstallService is the command's body with the OS passed in, so both the
// linux path and the refusal are exercised by tests on any platform — the same
// seam runInstallAgent uses, and for the same reason: a build tag would leave
// one branch untested on whichever OS the CI happens to run.
func runInstallService(out interface{ Write([]byte) (int, error) }, goos string, force bool) error {
	if goos != "linux" {
		hint := "on macOS run \"hubfuse install-agent\" instead"
		if goos != "darwin" {
			hint = "it is only meaningful where systemd runs the session"
		}
		return fmt.Errorf("install-service is Linux-only (this is %s); %s", goos, hint)
	}

	execPath, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locate this binary: %w", err)
	}
	execPath, err = filepath.EvalSymlinks(execPath)
	if err != nil {
		return fmt.Errorf("resolve this binary's path: %w", err)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("locate home directory: %w", err)
	}

	unitPath := systemdUserUnitPath(home, systemdAgentUnit)
	if _, statErr := os.Stat(unitPath); statErr == nil && !force {
		return fmt.Errorf("%s already exists; pass --force to overwrite", unitPath)
	}

	body, err := agentUnitBody(execPath)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(unitPath), err)
	}
	if err := os.WriteFile(unitPath, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", unitPath, err)
	}

	if _, err := fmt.Fprint(out, installServiceNextSteps(unitPath, systemdAgentUnit, execPath)); err != nil {
		return fmt.Errorf("write next steps: %w", err)
	}
	return nil
}

// installServiceNextSteps is the text printed after the unit is written.
//
// The lingering line is not advice, it is the difference between the unit doing
// its job and not: `systemctl --user enable --now` succeeds either way, so an
// operator who skips it sees a working agent today and an absent one after the
// next reboot — exactly the failure #117 exists to remove. The sudo note is
// there because polkit has no active session to authorise against over SSH,
// which is how a Linux server is usually administered.
func installServiceNextSteps(unitPath, unit, execPath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "wrote %s\n", unitPath)
	fmt.Fprintf(&b, "  program: %s\n\n", execPath)
	b.WriteString("Enable it:\n\n")
	b.WriteString("  systemctl --user daemon-reload\n")
	fmt.Fprintf(&b, "  systemctl --user enable --now %s\n\n", unit)
	b.WriteString("Then allow it to run without you logged in — REQUIRED, or it will not\n")
	b.WriteString("come back after a reboot even though the two commands above succeeded:\n\n")
	fmt.Fprintf(&b, "  loginctl enable-linger %s\n\n", "$USER")
	b.WriteString("Over SSH that usually needs sudo, because polkit has no active local\n")
	b.WriteString("session to authorise against.\n\n")
	fmt.Fprintf(&b, "Logs go to the journal, not to a file:\n\n  journalctl --user -u %s -f\n", unit)
	return b.String()
}
