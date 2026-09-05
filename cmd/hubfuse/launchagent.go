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
// Three fields are load-bearing and none is a style choice:
//
//   - ProgramArguments carries the ABSOLUTE path of the binary, taken from
//     os.Executable rather than from $PATH. The reason is no longer a claim
//     about how macOS keys its local-network decision — that claim was retracted
//     (#74) — but the ordinary one: a LaunchAgent has no useful $PATH, and a
//     relative name would resolve to whatever launchd happens to find, or to
//     nothing.
//   - KeepAlive is SuccessfulExit=false, NOT true. With `true`, launchd
//     relaunches the daemon whatever it exited for, including a clean SIGTERM —
//     so `hubfuse stop` was a no-op against a LaunchAgent-managed daemon, and an
//     unrecoverable failure became an unbounded restart loop (#98). The agent
//     now retries a hubless start in-process instead of exiting, so the only
//     exits left are a deliberate stop (zero — do not restart) and a real
//     failure (non-zero — do restart).
//   - ThrottleInterval bounds what remains. launchd's default floor is 10s;
//     30 makes a genuine crash loop cost two launches a minute rather than six,
//     which is what kept #98's log growing without bound.
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
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<key>ThrottleInterval</key>
	<integer>30</integer>
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
		Long: "Writes a LaunchAgent plist to ~/Library/LaunchAgents so the daemon starts with " +
			"your login session and is restarted if it fails.\n\n" +
			"On macOS, access to the local network is policed by NECP: a denied connection to a " +
			"LAN address fails with \"no route to host\" while the internet keeps working. Apple's " +
			"TN3179 lists which processes are exempt — a launchd daemon, any process running as " +
			"root, and command-line tools run from Terminal or over SSH including their children — " +
			"and states that the exemption does NOT extend to launchd agents. So this plist does " +
			"not grant anything; it makes the daemon start with your session and survive. If macOS " +
			"shows a Local Network prompt for hubfuse, allow it.",
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

// installAgentNextSteps is the text printed after the plist is written.
//
// It is deliberately short on promises. An earlier version told the operator to
// bootstrap from a terminal rather than over SSH, to approve a System Settings
// entry, and that a rebuild would not produce a fresh prompt. The first was
// beside the point once the daemon started retrying, the second names an entry
// that cannot exist for a binary with no bundle identifier, and the third was
// never isolated from launch context. All three shipped and all three are
// retracted (#74). What is left is what an operator can act on: the command to
// load it, that the daemon now recovers on its own, and Apple's own documented
// machine-wide escape hatch, attributed rather than asserted.
func installAgentNextSteps(plistPath, execPath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "wrote %s\n", plistPath)
	fmt.Fprintf(&b, "  program: %s\n\n", execPath)
	b.WriteString("Load it with:\n\n")
	fmt.Fprintf(&b, "  launchctl bootstrap gui/$(id -u) %s\n\n", plistPath)
	b.WriteString("If macOS shows a prompt asking whether hubfuse may access devices on your\n")
	b.WriteString("local network, allow it.\n\n")
	b.WriteString("If the agent cannot reach the hub it now retries instead of exiting, so a\n")
	b.WriteString("Mac that was asleep, a network that is not up yet, or a hub that is still\n")
	b.WriteString("booting all recover on their own. Watch the log to confirm.\n\n")
	b.WriteString("If dials to a LAN hub keep failing with \"no route to host\" while the\n")
	b.WriteString("internet works, that is macOS local-network policy, not routing. A plain\n")
	b.WriteString("command-line binary has no bundle identifier, so it may never be listed\n")
	b.WriteString("under System Settings > Privacy & Security > Local Network — an absent entry\n")
	b.WriteString("is not a setting you can change. Apple documents a machine-wide alternative\n")
	b.WriteString("in TN3179 (macOS 15.5+): the AllowedWiFiLocalNetworkAddresses and\n")
	b.WriteString("AllowedEthernetLocalNetworkAddresses keys in the com.apple.network.local-network\n")
	b.WriteString("defaults domain exempt a whole CIDR range for every program, and take effect\n")
	b.WriteString("after a restart.\n")
	return b.String()
}
