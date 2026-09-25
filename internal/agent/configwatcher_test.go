package agent

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	agentconfig "github.com/ykhdr/hubfuse/internal/agent/config"
	pb "github.com/ykhdr/hubfuse/proto"
)

// The config watcher used to start in runServices, which Run reaches only once a
// hub session exists. fsnotify reports only events that occur AFTER watching
// begins, so an edit made before that point was not delayed — it was lost, with
// nothing to replay it, and the daemon went on serving the config it read at
// startup.
//
// Since #74 that window has no bound: a daemon whose hub is unreachable retries
// forever instead of exiting, so the watcher's start was deferred forever with
// it. An operator who noticed a wrong hub.address and fixed it got no reload
// (#103), and the scenario suite went flaky because the harness waits for the
// `ssh server listening` line — emitted before registration — and then writes
// config (#85).

// TestOnConfigChange_SkipsTheHubPublishBeforeTheFirstRegistration pins the one
// thing moving the watcher earlier required.
//
// The local half of a share change must still happen — that is what makes a
// share appear in the SFTP listing, and it needs no hub at all. The hub publish
// must NOT be attempted: on a connection that has never completed a call it is
// certain to fail, and attempting it spends the full deadline and logs an Error
// about a condition that is expected and self-correcting. #73 is the standing
// judgement on that kind of noise.
//
// Skipping costs nothing because onConfigChange swaps d.config first, so the
// first Register that succeeds carries these shares — which is the same
// convergence argument the publish path already documents for a failed RPC.
func TestOnConfigChange_SkipsTheHubPublishBeforeTheFirstRegistration(t *testing.T) {
	d, _ := buildTestDaemon(t)
	require.False(t, d.everRegistered.Load(), "precondition: the hub has accepted nothing yet")

	var publishes atomic.Int32
	d.updateSharesFn = func(context.Context, []*pb.Share) error {
		publishes.Add(1)
		return nil
	}

	oldCfg := &agentconfig.Config{}
	newCfg := &agentconfig.Config{
		Shares: []agentconfig.ShareConfig{
			{Alias: "docs", Path: "/tmp", Permissions: "ro", AllowedDevices: []string{"all"}},
		},
	}
	d.config = oldCfg

	d.onConfigChange(oldCfg, newCfg)

	assert.Zero(t, publishes.Load(),
		"no RPC before the hub has ever accepted a Register — it cannot succeed, and trying "+
			"burns the deadline and logs an Error for an expected, self-correcting state")

	d.mu.RLock()
	got := d.config
	d.mu.RUnlock()
	assert.Same(t, newCfg, got,
		"the config swap must still happen, because that is what makes the first successful "+
			"Register carry these shares")

	assert.Len(t, d.sshServer.aclSnapshot(), 1,
		"the LOCAL half must still run: publishing the ACL to the SSH server is what makes the "+
			"share visible over SFTP, and it needs no hub")
}

// TestOnConfigChange_PublishesOnceRegistered is the positive control for the
// test above. Without it, a version that skipped the publish unconditionally
// would pass — and would silently stop telling the hub about share changes for
// the rest of the daemon's life.
func TestOnConfigChange_PublishesOnceRegistered(t *testing.T) {
	d, _ := buildTestDaemon(t)
	d.everRegistered.Store(true)

	var publishes atomic.Int32
	d.updateSharesFn = func(context.Context, []*pb.Share) error {
		publishes.Add(1)
		return nil
	}

	oldCfg := &agentconfig.Config{}
	newCfg := &agentconfig.Config{
		Shares: []agentconfig.ShareConfig{
			{Alias: "docs", Path: "/tmp", Permissions: "ro", AllowedDevices: []string{"all"}},
		},
	}
	d.config = oldCfg

	d.onConfigChange(oldCfg, newCfg)

	assert.Equal(t, int32(1), publishes.Load(),
		"a registered daemon must still publish share changes exactly once")
}

// TestStartConfigWatcher_SeesAnEditMadeBeforeAnyRegistration drives the real
// fsnotify watcher rather than calling onConfigChange directly.
//
// It tests the METHOD, not the wiring: it calls startConfigWatcher itself, so it
// would keep passing if Run stopped calling it. That gap is covered by
// tests/scenarios/config_watcher_test.go, which asserts the watcher's log line
// appears while the hub is DOWN — impossible if the call moved back into
// runServices. Verified: moving it back fails that scenario at exactly that
// assertion, 0 occurrences in 20s.
//
// It writes the config file with NO hub in the picture at all — no session, no
// registration — and requires the daemon to react. Before this change the
// watcher did not exist yet at this point, so the write produced no event and
// none was ever replayed.
func TestStartConfigWatcher_SeesAnEditMadeBeforeAnyRegistration(t *testing.T) {
	d, _ := buildTestDaemon(t)
	// buildTestDaemon's second return is the DATA DIRECTORY, not the config
	// path — d.configPath is the unambiguous handle, and writing to the
	// directory instead fails silently inside a goroutine.
	cfgPath := d.configPath
	require.False(t, d.everRegistered.Load(), "precondition: no registration has happened")

	reacted := make(chan struct{}, 1)
	d.updateSharesFn = func(context.Context, []*pb.Share) error { return nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d.startConfigWatcher(ctx)

	// The watcher's goroutine needs to be watching before the write, which is the
	// very race this change is about — so poll for the reaction rather than
	// sleeping a guessed interval.
	go func() {
		for i := 0; i < 40; i++ {
			body := `device {
    nickname "test-device"
}
hub {
    address "localhost:9090"
}
agent {
    ssh-port 2222
}
shares {
    share "/tmp" alias="watched" permissions="ro" {
        allowed-devices "all"
    }
}
`
			if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
				return
			}
			if len(d.sshServer.aclSnapshot()) > 0 {
				reacted <- struct{}{}
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()

	select {
	case <-reacted:
	case <-time.After(15 * time.Second):
		t.Fatal("the daemon never reacted to a config edit made before any registration — " +
			"which is exactly the lost-event failure #103 is about")
	}

	assert.Len(t, d.sshServer.aclSnapshot(), 1,
		"the share written before any hub session must have reached the SSH server")
}
