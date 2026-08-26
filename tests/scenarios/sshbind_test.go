package scenarios_test

import (
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ykhdr/hubfuse/tests/scenarios/helpers"
)

// TestSSHBindFailureAbortsStartup is issue #90.
//
// A daemon whose SSH server cannot take its port has nothing to serve, but it
// used to register anyway: startSSH launched the server in a goroutine and
// returned nil unconditionally, so the bind error was one log line and the very
// next line was "registered with hub". The hub then handed d.sshPort to every
// peer — a port owned by whoever squatted it. If that squatter speaks at all,
// the peer's SSHFS does not fail; it reaches the wrong process.
//
// The assertion that carries the issue is the last one: the hub must never have
// seen this device online. Checking only the process's exit would pass for a
// daemon that registered first and died afterwards, which is the same bug with
// better manners.
func TestSSHBindFailureAbortsStartup(t *testing.T) {
	hub := helpers.StartHub(t)

	// alice is the observer. The hub's view is only reachable through a device
	// that can talk to it, and bob is by construction never going to be one.
	alice := helpers.StartAgent(t, hub, "alice")
	alice.Join(t)
	alice.StartDaemon(t)

	port := helpers.FreePort(t)
	squatter, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	require.NoError(t, err, "squat bob's SSH port")
	defer squatter.Close()

	bob := helpers.StartAgent(t, hub, "bob", helpers.WithSSHPort(port))
	bob.Join(t)

	out := bob.StartDaemonExpectFailure(t, 30*time.Second)

	assert.Contains(t, out, "address already in use",
		"the failure must name the cause: the SSH port could not be taken")
	assert.Contains(t, out, fmt.Sprintf("%d", port),
		"the failure must name the port an operator has to free")
	assert.NotContains(t, out, "registered with hub",
		"a daemon that cannot serve anything must never reach the hub")

	// The hub's own view. bob has joined, so a row exists — it just must never
	// have been online, because nothing was ever registered against it.
	row, ok := alice.PeerStatus(t, "bob")
	if ok {
		assert.NotEqual(t, "online", row.Status,
			"the hub must never advertise a device whose SSH port belongs to someone else")
	}

	// Nothing about the squatter changed: it still holds the port, which is why
	// a port probe alone could never have caught this.
	assert.NoError(t, squatter.Close(), "the squatter held the port throughout")
}

// TestSSHBindSucceedsWithoutSquatter is the negative control for the test above.
// Without it, a fix that broke `hubfuse start` outright — or a harness that
// failed every daemon for an unrelated reason — would leave that test green and
// the regression invisible. Same hub, same helper, same port handling; the only
// difference is that nobody is holding the port. (#90)
func TestSSHBindSucceedsWithoutSquatter(t *testing.T) {
	hub := helpers.StartHub(t)

	alice := helpers.StartAgent(t, hub, "alice")
	alice.Join(t)
	alice.StartDaemon(t)

	port := helpers.FreePort(t)
	bob := helpers.StartAgent(t, hub, "bob", helpers.WithSSHPort(port))
	bob.Join(t)
	bob.StartDaemon(t)

	require.Eventually(t, func() bool {
		row, ok := alice.PeerStatus(t, "bob")
		return ok && row.Status == "online"
	}, 15*time.Second, 200*time.Millisecond,
		"a daemon that DID take its port must register and come online")
}
