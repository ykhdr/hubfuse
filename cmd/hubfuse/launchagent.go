package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"

	"github.com/spf13/cobra"
	"github.com/ykhdr/hubfuse/internal/common"
)

// launchAgentLabel is the LaunchAgent's label and the basename of its plist.
// It is also the handle an operator needs for launchctl, so it is printed
// verbatim by install-agent rather than left for them to derive.
const launchAgentLabel = "com.github.ykhdr.hubfuse"

// launchAgentTemplate is the plist install-agent writes.
//
// Two fields are load-bearing and neither is a style choice:
//
//   - ProgramArguments carries the ABSOLUTE path of the binary, taken from
//     os.Executable rather than from $PATH. macOS grants local-network access
//     per binary identity (issue #74), so a plist pointing at a different copy
//     of hubfuse gets a different — and unapproved — identity, and the daemon
//     is cut off exactly as before. Measured on the test bed: the same bytes,
//     re-signed, behaved the opposite way from the installed path.
//   - RunAtLoad plus KeepAlive is what puts the daemon inside the user's GUI
//     session and keeps it there. That session is the whole point: the
//     local-network prompt is a GUI prompt, and a daemon started over SSH can
//     never be shown one, which is why an SSH-launched agent is cut off with no
//     way to grant it anything.
//
// ProcessType Interactive keeps macOS from throttling it as a background batch
// job; the agent has to answer the hub's heartbeat on a fixed cadence.
const launchAgentTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{.Label}}</string>
	<key>ProgramArguments</key>
	<array>
		<string>{{.Exec}}</string>
		<string>start</string>
	</array>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ProcessType</key>
	<string>Interactive</string>
	<key>StandardOutPath</key>
	<string>{{.LogPath}}</string>
	<key>StandardErrorPath</key>
	<string>{{.LogPath}}</string>
</dict>
</plist>
`

// launchAgentPlist renders the plist for execPath, writing its log to logPath.
//
// Both values are XML-escaped. That is not defensive dressing: a home directory
// containing an ampersand ("Alice & Bob") produces a path that silently
// corrupts the plist, and launchd's failure for a malformed plist is to ignore
// it — the operator would see install-agent report success and the agent never
// run, which is the exact class of unreadable failure issue #74 is about.
func launchAgentPlist(execPath, logPath string) ([]byte, error) {
	tmpl, err := template.New("plist").Parse(launchAgentTemplate)
	if err != nil {
		return nil, fmt.Errorf("parse plist template: %w", err)
	}
	var buf bytes.Buffer
	err = tmpl.Execute(&buf, struct{ Label, Exec, LogPath string }{
		Label:   launchAgentLabel,
		Exec:    xmlEscape(execPath),
		LogPath: xmlEscape(logPath),
	})
	if err != nil {
		return nil, fmt.Errorf("render plist: %w", err)
	}
	return buf.Bytes(), nil
}

func xmlEscape(s string) string {
	var buf bytes.Buffer
	if err := xml.EscapeText(&buf, []byte(s)); err != nil {
		// EscapeText only fails if the writer fails, and bytes.Buffer does not.
		return s
	}
	return buf.String()
}

// launchAgentPath is where launchd looks for per-user agents.
func launchAgentPath(home string) string {
	return filepath.Join(home, "Library", "LaunchAgents", launchAgentLabel+".plist")
}

// installAgentCmd implements: hubfuse install-agent
func installAgentCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "install-agent",
		Short: "Install a macOS LaunchAgent so the daemon runs in your GUI session",
		Long: "Writes a LaunchAgent plist to ~/Library/LaunchAgents so the daemon runs inside " +
			"your logged-in GUI session.\n\n" +
			"On macOS, access to the local network is granted per binary and only through a GUI " +
			"prompt. A daemon started over SSH can never be shown that prompt, so it is cut off " +
			"from the LAN shortly after it starts and every dial to the hub then fails with " +
			"\"no route to host\". Running it as a LaunchAgent is what makes the prompt reachable.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runInstallAgent(cmd.OutOrStdout(), runtime.GOOS, force)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing plist")
	return cmd
}

// runInstallAgent is the command's body with the OS passed in, so both the
// darwin path and the refusal are exercised by tests on any platform — a
// build tag here would leave one of the two branches untested on the Linux CI
// that actually runs the suite.
func runInstallAgent(out interface{ Write([]byte) (int, error) }, goos string, force bool) error {
	if goos != "darwin" {
		return fmt.Errorf("install-agent is macOS-only (this is %s); "+
			"on Linux run the daemon under systemd or start it with \"hubfuse start --daemon\"", goos)
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

	plistPath := launchAgentPath(home)
	if _, statErr := os.Stat(plistPath); statErr == nil && !force {
		return fmt.Errorf("%s already exists; pass --force to overwrite", plistPath)
	}

	dataDir := common.ExpandHome(common.AgentDataDir)
	body, err := launchAgentPlist(execPath, filepath.Join(dataDir, common.AgentLogFile))
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(plistPath), err)
	}
	if err := os.WriteFile(plistPath, body, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", plistPath, err)
	}

	// The plist is already on disk at this point, so a write failure here does
	// not mean the install failed — but it does mean the operator never saw the
	// two steps they cannot guess (bootstrap from a GUI session; approve the
	// prompt), and silently succeeding into a blank terminal would leave them
	// with a plist that never gets loaded.
	if _, err := fmt.Fprint(out, installAgentNextSteps(plistPath, execPath)); err != nil {
		return fmt.Errorf("wrote %s, but could not print the next steps: %w", plistPath, err)
	}
	return nil
}

// installAgentNextSteps is the text printed after a successful write. It is a
// function so a test can assert that the two things an operator cannot guess
// are actually said: that bootstrapping must happen from a GUI session, and
// that upgrading the binary invalidates the approval.
func installAgentNextSteps(plistPath, execPath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "wrote %s\n", plistPath)
	fmt.Fprintf(&b, "  program: %s\n\n", execPath)
	b.WriteString("Next, FROM A TERMINAL ON THE MAC ITSELF (not over SSH):\n\n")
	fmt.Fprintf(&b, "  launchctl bootstrap gui/$(id -u) %s\n\n", plistPath)
	b.WriteString("macOS will ask once whether hubfuse may access devices on your local network.\n")
	b.WriteString("Approve it. Over SSH there is no way to show that prompt, so the agent would be\n")
	b.WriteString("cut off from the LAN a few seconds after it starts, and every dial to the hub\n")
	b.WriteString("would fail with \"no route to host\".\n\n")
	b.WriteString("If you miss the prompt: System Settings > Privacy & Security > Local Network.\n\n")
	b.WriteString("Note: the decision is tied to this PATH, not to the file's contents.\n")
	b.WriteString("Replacing the binary here does not get you a fresh prompt — measured: a path\n")
	b.WriteString("macOS had already refused stayed refused after the binary at it was rebuilt\n")
	b.WriteString("and re-signed, with no prompt and no grace period. If hubfuse was refused\n")
	b.WriteString("once, clear it under System Settings rather than reinstalling.\n")
	return b.String()
}
