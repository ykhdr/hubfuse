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
// The macOS behaviour itself was measured rather than assumed (issue #74): a
// freshly signed daemon registered with the hub, was cut off ~40s later, and
// then failed every dial with this error indefinitely, while the same bytes
// under a different signing identity kept working. Permission is granted per
// binary and only through a GUI prompt, which a daemon started over SSH can
// never be shown.
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

// localNetworkDenialMessage is the one thing the daemon can usefully say when
// it detects this. evidence states what was actually observed, because the two
// call sites have genuinely different amounts of it and the message must not
// overstate either: the reconnect loop has watched a streak of failures, while
// the startup path has seen exactly one and is about to exit on it.
//
// It is a function so the test can assert that the parts an operator cannot
// work out for themselves are present regardless of which site called it: that
// the block is per binary, that an SSH-started daemon can never be approved,
// and where to go.
func localNetworkDenialMessage(evidence string) string {
	return "this binary appears to have been denied local-network access by macOS: " +
		evidence + ". " +
		"macOS grants that access per binary and only through a GUI prompt, so a daemon started over SSH " +
		"can never be approved and is cut off shortly after it starts. " +
		"Fix: run \"hubfuse install-agent\" and bootstrap it from a terminal on the Mac itself, " +
		"or approve hubfuse under System Settings > Privacy & Security > Local Network. " +
		"Note that this decision follows the PATH, not the file: rebuilding hubfuse in place does " +
		"not produce a fresh prompt, so clear it under Local Network rather than reinstalling"
}

// Evidence clauses for the two places this is detected. They are separate
// constants rather than inline strings so the difference between them stays
// visible: one is a sustained observation, the other a single one.
const (
	// localNetworkEvidenceStreak is the reconnect loop's case: several dials in
	// a row, over at least the length of its backoff.
	localNetworkEvidenceStreak = "every dial to the hub is failing with \"no route to host\" " +
		"while the host itself is on the LAN"

	// localNetworkEvidenceStartup is the FIRST session's case, and the one a
	// user hits most: a binary that has already been refused is refused
	// instantly, so the daemon never reaches the reconnect loop at all — it
	// fails its initial registration and exits. Measured on the test bed: the
	// installed path failed at t=0 and the process was gone, while a
	// freshly-signed copy of the same bytes registered and ran.
	//
	// The wording hedges deliberately. One EHOSTUNREACH at startup is also what
	// a LaunchAgent launched before the network is up would see, and that case
	// resolves itself on the next launch; saying "every dial" here would be a
	// claim this call site has not earned.
	localNetworkEvidenceStartup = "the hub address is on the LAN, but the very first dial to it " +
		"failed with \"no route to host\""
)
