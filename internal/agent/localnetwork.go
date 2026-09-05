package agent

import (
	"net"
	"net/netip"
	"strings"
)

// ─── macOS local-network denial (issue #74) ──────────────────────────────────

// localNetworkFailureStreak is how many consecutive dial failures of this shape
// must accumulate before the daemon names macOS as the cause.
//
// It is not 1 on purpose. A single EHOSTUNREACH to a LAN address is the
// ordinary sound of a laptop changing networks — waking from sleep, moving
// between access points, a router rebooting — and all of those fix themselves
// within a retry or two. macOS revoking local-network access does not fix
// itself at all: every subsequent dial fails identically until a human
// approves the binary. Three in a row separates the two without waiting long,
// since reconnectSession's backoff means the third failure lands well inside a
// minute of the first.
const localNetworkFailureStreak = 3

// isLocalNetworkDenial reports whether err looks like macOS having cut this
// binary off from the local network, rather than an ordinary network failure.
//
// Three conditions must hold together, and each one is there to stop a
// different false accusation:
//
//   - goos is darwin. No other platform has this mechanism, and telling a Linux
//     operator to open System Settings would be worse than saying nothing.
//   - the error is EHOSTUNREACH. That is what a denied process gets for every
//     dial to a local address, and it is what the test bed measured verbatim:
//     `dial tcp 192.168.31.158:9090: connect: no route to host`. The check goes
//     through the error chain (syscall.EHOSTUNREACH) when the chain survives,
//     and falls back to the text when it does not — a dial error that has been
//     through a gRPC status is a formatted string with no wrapped errno left.
//   - the hub address is on the local network. `no route to host` reaching a
//     public address is a routing problem and nothing else; blaming macOS for
//     it would replace one misleading message with another. A literal private
//     or link-local IP qualifies, and so does an mDNS `.local` name, which is
//     local by definition. Anything else — a public IP, a DNS name we cannot
//     resolve to a judgement — deliberately does NOT qualify: staying quiet is
//     the correct behaviour when the evidence is not there.
//
// What the mechanism actually is, from the system's own log rather than from
// inference (#74). Reproduced under a LaunchAgent on macOS 26.4:
//
//	kernel        process: hubfuse  t_state: SYN_SENT  error: 65  reason: NECP
//	nehelper  E   Could not find bundle ID or display name for app:
//	                (bundleID: hubfuse-<hash>, name: (null), teamID: (null))
//
// So the SYN is dropped by NECP — the Network Extension policy engine — with
// errno 65, which Go renders as "no route to host". It is Local Network privacy,
// not routing, and not TCC either: there is no kTCCServiceLocalNetwork row and
// tccutil cannot reset it.
//
// TN3179 documents which processes are exempt: a launchd DAEMON, any process
// running as root, and command-line tools run from Terminal or over SSH
// including their children — but explicitly NOT a launchd AGENT. That is a
// line-by-line explanation of the test bed's results, including why a daemon
// keeps LAN access while its SSH session lives and loses it when the session
// ends.
//
// Two earlier readings of this are retracted rather than refined: that the
// decision is keyed to the CDHash, and that it is keyed to the path. Neither was
// ever isolated — every comparison changed the launch context at the same time,
// and launch context alone explains the outcomes.
//
// Deliberately unreconciled, and left that way rather than papered over: an
// earlier session measured a daemon cut off ~40s after registering whose
// reconnect loop then failed for minutes, while a later session measured the
// same binary under the same LaunchAgent staying online for 6 continuous
// minutes and a bare probe connecting 18/18 over 4 minutes with no approval. No
// OS update separated them. The retry this file's callers now perform is correct
// under either reading, which is why the discrepancy does not block the fix.
func isLocalNetworkDenial(goos, hubAddr string, err error) bool {
	if goos != "darwin" || err == nil {
		return false
	}
	if !isHostUnreachable(err) {
		return false
	}
	return isLocalNetworkAddress(hubAddr)
}

// isHostUnreachable reports whether err is EHOSTUNREACH.
//
// The text fallback is not laziness: by the time a dial failure has been turned
// into a gRPC status and back, it is a plain string —
// `connection error: desc = "transport: Error while dialing: dial tcp
// 10.0.0.1:9090: connect: no route to host"` — and errors.Is has nothing left
// to walk. "no route to host" is the fixed Go rendering of EHOSTUNREACH
// (syscall's errno table), not a message this project or gRPC composes, so it
// is stable in a way an assembled phrase would not be.
func isHostUnreachable(err error) bool {
	if errnoIsHostUnreachable(err) {
		return true
	}
	return strings.Contains(strings.ToLower(err.Error()), "no route to host")
}

// isLocalNetworkAddress reports whether addr ("host:port", or a bare host) is on
// the local network as far as we can tell WITHOUT resolving it. Resolution is
// deliberately avoided: this runs on a failure path where the network is
// already misbehaving, and a DNS lookup there would add a stall to an error
// message.
func isLocalNetworkAddress(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return false
	}

	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLoopback()
	}

	// Not a literal address. An mDNS name is local by definition; anything else
	// we decline to judge.
	return strings.HasSuffix(strings.ToLower(strings.TrimSuffix(host, ".")), ".local")
}

// localNetworkDenialMessage is what the daemon says when it has watched a
// sustained streak of dials fail this way.
//
// It is a DIAGNOSTIC, not a remedy, and that changed with the retry (#74).
// The daemon no longer dies on this — it keeps trying — so the message's job is
// to say what is happening and what is verifiable, not to hand the operator a
// procedure. The previous version handed them two, and both were wrong:
//
//   - "approve hubfuse under System Settings > Privacy & Security > Local
//     Network". A bare ad-hoc Mach-O cannot appear there. The unified log says
//     so directly: `nehelper: Could not find bundle ID or display name for app:
//     (bundleID: hubfuse-<hash>, name: (null), teamID: (null))`. With no name
//     and no team ID the entry cannot be constructed, so the denial is macOS
//     falling back rather than a decision the user made or can revisit. The
//     repo owner went looking for that entry and found nothing, which is how
//     this was caught.
//   - "this decision follows the PATH, not the file". Unproven — every
//     comparison that produced it also changed the launch context, and TN3179's
//     exemption table makes launch context sufficient to explain both outcomes
//     on its own. The earlier reading of the same evidence as CDHash-keyed was
//     the same confound. Acting on either version (reinstall elsewhere) is
//     useless, so neither belongs in operator-facing text.
//
// What IS measured and reachable goes in instead: the mechanism has a name in
// the log, the daemon is retrying rather than giving up, and TN3179 documents a
// machine-wide escape hatch. The CIDR keys are ATTRIBUTED to Apple rather than
// asserted, because this project has not tested them: Apple documents them only
// with 169.254.0.0/16, and whether an RFC1918 range is accepted is unverified.
func localNetworkDenialMessage(evidence string) string {
	return "this binary appears to have been denied local-network access by macOS: " +
		evidence + ". " +
		"The kernel reports these as NECP policy drops, not routing failures, which is why the hub " +
		"looks unreachable while the internet still works. The daemon keeps retrying, so it will " +
		"recover on its own if the block lifts. " +
		"If macOS shows a Local Network prompt for hubfuse, allow it. " +
		"Note that a plain command-line binary has no bundle identifier, so it may not be listed " +
		"under System Settings > Privacy & Security > Local Network at all — an absent entry is not " +
		"a setting you can change. " +
		"Apple documents a machine-wide alternative in TN3179 (macOS 15.5+): the " +
		"AllowedWiFiLocalNetworkAddresses / AllowedEthernetLocalNetworkAddresses keys in the " +
		"com.apple.network.local-network defaults domain exempt an entire CIDR range for every " +
		"program, and take effect after a restart"
}

// localNetworkEvidenceStreak states what was observed. It is a constant rather
// than an inline string because there used to be a second one for the startup
// path, and the difference between "seen once" and "seen repeatedly" was
// load-bearing.
//
// There is only one now. The startup call site is gone: since the daemon retries
// instead of exiting, the streak logic in reconnectSession runs at startup too,
// and a first-failure diagnostic would have been contradicted a second later on
// the measured `fail, ok, ok` shape — then, being once-per-process, it would
// have permanently suppressed this one.
const localNetworkEvidenceStreak = "every dial to the hub is failing with \"no route to host\" " +
	"while the host itself is on the LAN"
