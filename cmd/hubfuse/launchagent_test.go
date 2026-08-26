package main

import (
	"bytes"
	"encoding/xml"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLaunchAgentPlist_IsWellFormedAndCarriesTheAbsolutePath pins the two
// properties launchd silently punishes. A malformed plist is IGNORED by
// launchd — no error, no agent — so "it parses" has to be asserted, not
// assumed; and the program path has to be the exact binary, because macOS
// grants local-network access per binary identity (issue #74).
func TestLaunchAgentPlist_IsWellFormedAndCarriesTheAbsolutePath(t *testing.T) {
	body, err := launchAgentPlist("/Users/alice/go/bin/hubfuse", "/Users/alice/.hubfuse/agent.log")
	require.NoError(t, err)

	// Well-formedness, checked by an actual XML parser rather than by eye.
	dec := xml.NewDecoder(bytes.NewReader(body))
	for {
		_, tokErr := dec.Token()
		if tokErr != nil {
			require.ErrorContains(t, tokErr, "EOF", "the plist must be well-formed XML")
			break
		}
	}

	s := string(body)
	assert.Contains(t, s, "<string>/Users/alice/go/bin/hubfuse</string>",
		"the plist must name the exact binary: the local-network approval is tied to it")
	assert.Contains(t, s, "<string>start</string>", "the agent must be launched with the start subcommand")
	assert.Contains(t, s, launchAgentLabel)
	assert.Contains(t, s, "<key>RunAtLoad</key>")
	assert.Contains(t, s, "<key>KeepAlive</key>")
	assert.Contains(t, s, "/Users/alice/.hubfuse/agent.log")
}

// TestLaunchAgentPlist_EscapesPathsThatWouldCorruptTheXML is the case that
// justifies escaping at all. A home directory named "Alice & Bob" is legal on
// macOS and produces a path that turns the plist into invalid XML — whereupon
// launchd ignores the file entirely and the operator is left with a command
// that reported success and an agent that never runs.
func TestLaunchAgentPlist_EscapesPathsThatWouldCorruptTheXML(t *testing.T) {
	body, err := launchAgentPlist(`/Users/Alice & Bob/bin/hubfuse`, `/Users/Alice & Bob/.hubfuse/agent.log`)
	require.NoError(t, err)

	assert.NotContains(t, string(body), "Alice & Bob",
		"a raw ampersand must not survive into the plist — it is what makes the XML invalid")
	assert.Contains(t, string(body), "Alice &amp; Bob", "the ampersand must be escaped in place")

	dec := xml.NewDecoder(bytes.NewReader(body))
	for {
		_, tokErr := dec.Token()
		if tokErr != nil {
			require.ErrorContains(t, tokErr, "EOF",
				"an escaped path must still leave the plist parseable — this is the whole point")
			break
		}
	}
}

func TestLaunchAgentPath(t *testing.T) {
	assert.Equal(t,
		"/Users/alice/Library/LaunchAgents/"+launchAgentLabel+".plist",
		launchAgentPath("/Users/alice"),
		"launchd only looks in ~/Library/LaunchAgents for per-user agents")
}

// TestRunInstallAgent_RefusesOffDarwin covers the branch the CI that runs this
// suite is actually on. The refusal must name the platform and point somewhere
// useful rather than just failing — an operator on Linux who types this command
// has a real need (running the daemon at boot) that this command cannot serve.
func TestRunInstallAgent_RefusesOffDarwin(t *testing.T) {
	var out bytes.Buffer
	err := runInstallAgent(&out, "linux", false)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "macOS-only")
	assert.Contains(t, err.Error(), "linux", "the refusal must name the platform it is refusing on")
	assert.Contains(t, err.Error(), "systemd", "and point at what does work here")
	assert.Empty(t, out.String(), "a refused install must not print next steps")
}

// TestInstallAgentNextSteps_SaysWhatCannotBeGuessed pins the two facts that
// cost days to discover on the test bed and that no amount of reading the
// plist would reveal (issue #74):
//
//  1. bootstrapping must happen from a terminal ON the Mac — over SSH the
//     local-network prompt cannot be shown, so the agent is approved by nobody
//     and cut off a few seconds in;
//  2. the approval is bound to that exact binary, so upgrading hubfuse means
//     approving it again.
//
// Both are the difference between a working install and a silent failure, so
// they are asserted rather than left to whoever edits this text next.
func TestInstallAgentNextSteps_SaysWhatCannotBeGuessed(t *testing.T) {
	got := installAgentNextSteps("/Users/alice/Library/LaunchAgents/x.plist", "/Users/alice/go/bin/hubfuse")

	assert.Contains(t, got, "/Users/alice/go/bin/hubfuse", "the operator must see which binary was wired in")
	assert.Contains(t, got, "launchctl bootstrap", "the next command must be spelled out")
	assert.Contains(t, strings.ToLower(got), "not over ssh",
		"the SSH trap is the whole reason this command exists")
	assert.Contains(t, got, "Local Network",
		"the settings pane must be named for anyone who missed the prompt")
	assert.Contains(t, strings.ToLower(got), "approve it again",
		"an upgrade changes the binary's identity and voids the approval")
}
