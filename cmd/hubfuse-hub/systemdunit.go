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

// systemdHubUnit is the unit's filename and the handle systemctl needs, so it is
// printed verbatim rather than left for the operator to derive.
const systemdHubUnit = "hubfuse-hub.service"

// hubUnitTemplate is the systemd user unit hubfuse-hub install-service writes.
//
// It is deliberately the agent's unit MINUS two settings, and the subtractions
// matter more than the shape:
//
//   - NO Environment=PATH. The hub shells out to nothing — there is no
//     exec.Command or exec.LookPath anywhere under internal/hub or
//     cmd/hubfuse-hub — so a PATH here would be cargo. The agent needs one
//     because it runs sshfs.
//   - NO KillMode=mixed. The hub spawns no children, so systemd's default
//     control-group kill has nothing extra to signal. The agent needs `mixed`
//     because sshfs self-daemonizes into its cgroup and the default would tear
//     live mounts down instead of letting the agent unmount them in order.
//
// Both absences have their own assertions, so neither gets added later by
// symmetry with the agent's unit.
//
// What it keeps, and why:
//
//   - [Install] WantedBy=default.target — a user manager has no
//     multi-user.target, and `systemctl --user enable --now` succeeds regardless
//     because --now starts it in the current session. Without this the hub does
//     not come back after a reboot and the install still looks successful
//     (#117).
//   - Restart=on-failure, NOT always. `hubfuse-hub stop` exits zero on a healthy
//     shutdown, and the hub goes out of its way to make that true: a cancelled
//     context or a closed store reaching its startup path is filtered so an
//     ordinary stop is not reported as a failed unit (internal/hub/hub.go).
//     Restart=always would undo that and make the stop command a no-op, which is
//     the launchd shape of #98.
//   - RestartSec=30 — #98's ThrottleInterval argument.
//
// Logs go to the journal, systemd's default; install-service prints the
// journalctl line because the macOS plist logs to a file instead.
const hubUnitTemplate = `[Unit]
Description=HubFuse hub
Documentation=https://github.com/ykhdr/hubfuse

[Service]
Type=simple
ExecStart={{.Exec}} start
Restart=on-failure
RestartSec=30

[Install]
WantedBy=default.target
`

// systemdEscapeExec renders path for use in an ExecStart= line.
//
// Duplicated from cmd/hubfuse rather than shared, matching how launchagent.go's
// xmlEscape is not shared either: these are two small main packages that cannot
// import each other, and a common package for eight lines would cost more to
// find than to re-read.
//
// The hazards are systemd's own, not XML's: ExecStart splits on whitespace
// unless quoted, and '%' introduces a specifier so a literal one must be
// doubled. Doubling comes FIRST — after quoting it would also double whatever
// the quoting introduced.
func systemdEscapeExec(path string) string {
	escaped := strings.ReplaceAll(path, "%", "%%")
	if strings.ContainsAny(escaped, " \t") {
		escaped = strings.ReplaceAll(escaped, `\`, `\\`)
		escaped = strings.ReplaceAll(escaped, `"`, `\"`)
		return `"` + escaped + `"`
	}
	return escaped
}

// hubUnitBody renders the unit for execPath.
func hubUnitBody(execPath string) ([]byte, error) {
	tmpl, err := template.New("unit").Parse(hubUnitTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse unit template: %w", err)
	}
	var buf strings.Builder
	if err := tmpl.Execute(&buf, struct{ Exec string }{Exec: systemdEscapeExec(execPath)}); err != nil {
		return nil, fmt.Errorf("render unit: %w", err)
	}
	return []byte(buf.String()), nil
}

// systemdUserUnitPath is where a user manager looks for units it can enable.
func systemdUserUnitPath(home, unit string) string {
	return filepath.Join(home, ".config", "systemd", "user", unit)
}

// installServiceCmd implements: hubfuse-hub install-service
func installServiceCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "install-service",
		Short: "Install a systemd user unit so the hub starts at boot",
		Long: "Writes a systemd user unit to ~/.config/systemd/user so the hub starts with the " +
			"machine and is restarted if it fails.\n\n" +
			"It is a USER unit because the hub keeps its store in ~/.hubfuse-hub. The cost is " +
			"that a user manager stops at logout unless lingering is enabled, so " +
			"\"loginctl enable-linger\" is a required step and not a footnote — without it the " +
			"hub will not come back after a reboot, and every device on it stays offline while " +
			"the install still looks successful.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInstallService(cmd.OutOrStdout(), runtime.GOOS, force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing unit")
	return cmd
}

// runInstallService is the command's body with the OS passed in, so both the
// linux path and the refusal are exercised by tests on any platform — the seam
// runInstallAgent established in cmd/hubfuse, for the same reason.
func runInstallService(out interface{ Write([]byte) (int, error) }, goos string, force bool) error {
	if goos != "linux" {
		return fmt.Errorf("install-service is Linux-only (this is %s); "+
			"it is only meaningful where systemd runs the session", goos)
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

	unitPath := systemdUserUnitPath(home, systemdHubUnit)
	if _, statErr := os.Stat(unitPath); statErr == nil && !force {
		return fmt.Errorf("%s already exists; pass --force to overwrite", unitPath)
	}

	body, err := hubUnitBody(execPath)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(unitPath), err)
	}
	if err := os.WriteFile(unitPath, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", unitPath, err)
	}

	if _, err := fmt.Fprint(out, installServiceNextSteps(unitPath, systemdHubUnit, execPath)); err != nil {
		return fmt.Errorf("write next steps: %w", err)
	}
	return nil
}

// installServiceNextSteps is the text printed after the unit is written.
//
// The lingering line is not advice. `systemctl --user enable --now` succeeds
// whether or not lingering is set, so an operator who stops there gets a working
// hub today and, after the next reboot, every device offline — arrived at through
// an install that reported success. The sudo note is there because polkit has no
// active session to authorise against over SSH, which is how a server is usually
// administered.
func installServiceNextSteps(unitPath, unit, execPath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "wrote %s\n", unitPath)
	fmt.Fprintf(&b, "  program: %s\n\n", execPath)
	b.WriteString("Enable it:\n\n")
	b.WriteString("  systemctl --user daemon-reload\n")
	fmt.Fprintf(&b, "  systemctl --user enable --now %s\n\n", unit)
	b.WriteString("Then allow it to run without you logged in — REQUIRED, or it will not\n")
	b.WriteString("come back after a reboot even though the two commands above succeeded,\n")
	b.WriteString("and every device on this hub will read offline:\n\n")
	b.WriteString("  loginctl enable-linger $USER\n\n")
	b.WriteString("Over SSH that usually needs sudo, because polkit has no active local\n")
	b.WriteString("session to authorise against.\n\n")
	fmt.Fprintf(&b, "Logs go to the journal, not to a file:\n\n  journalctl --user -u %s -f\n", unit)
	return b.String()
}
