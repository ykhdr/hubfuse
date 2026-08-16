package hub

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/credentials/insecure"
)

// agentKeepaliveTime mirrors agent.hubKeepaliveTime. The two packages cannot
// import each other, and the constant that matters here is the agent's, so it
// is restated with the check that keeps them honest below. (#72)
const agentKeepaliveTime = 10 * time.Second

// TestServerOptions_EnforcementPolicyToleratesAgentPings pins the invariant the
// whole fix rests on: grpc-go answers a client that pings more often than
// EnforcementPolicy.MinTime with GOAWAY too_many_pings. If MinTime ever creeps
// above the agent's keepalive interval, every agent would be disconnected on a
// schedule — the opposite of what #72 set out to fix. The margin also has to
// survive ordinary jitter, hence "comfortably below", not merely "below".
func TestServerOptions_EnforcementPolicyToleratesAgentPings(t *testing.T) {
	require.Less(t, keepaliveMinTime, agentKeepaliveTime,
		"the hub must tolerate the agent's ping cadence")
	assert.LessOrEqual(t, keepaliveMinTime, agentKeepaliveTime/2,
		"leave room for jitter between the agent's ping and the hub's tolerance")
	assert.Positive(t, serverKeepaliveTime)
	assert.Positive(t, serverKeepaliveTimeout)
}

// TestServerOptions_AreComplete guards the reason ServerOptions exists: both
// the real hub and hubtest build their server from it, so an option added in
// one place cannot go missing in the other.
func TestServerOptions_AreComplete(t *testing.T) {
	opts := ServerOptions(insecure.NewCredentials())
	assert.Len(t, opts, 5,
		"expected creds, unary and stream interceptors, keepalive params and enforcement policy")
}
