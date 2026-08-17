package agent

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	agentconfig "github.com/ykhdr/hubfuse/internal/agent/config"
	"github.com/ykhdr/hubfuse/internal/common"
	pb "github.com/ykhdr/hubfuse/proto"
)

// shortenBudget swaps one of the session budgets for the duration of a test.
// The budgets are vars for exactly this reason: at 20s apiece, every test of
// what happens when one expires would cost 20 seconds.
func shortenBudget(t *testing.T, budget *time.Duration, d time.Duration) {
	t.Helper()
	prev := *budget
	*budget = d
	t.Cleanup(func() { *budget = prev })
}

// ─── The invariant that keeps ordinary disconnects off the swap path ──────────

// Keepalive must notice a dead transport before the daemon's own register budget
// expires. That ordering is what makes an ordinary disconnect come back
// Unavailable — the hub is gone, our budget never fired — instead of looking
// like the daemon's deadline expiring on a live-but-useless connection. The
// classifier no longer depends on it for correctness (it reads whose deadline
// fired, not which code came back), but the ordering is what keeps the common
// case off the replacement path entirely.
func TestRegisterBudgetOutlastsKeepaliveDetection(t *testing.T) {
	t.Parallel()

	detection := hubKeepaliveTime + hubKeepaliveTimeout
	assert.Less(t, detection, defaultRegisterTimeout,
		"a dead transport must be detected (%s) before the register budget expires (%s), "+
			"or every ordinary disconnect starts looking like a wedged connection",
		detection, defaultRegisterTimeout)
	assert.Less(t, detection, defaultSubscribeSetupTimeout,
		"the same ordering must hold for the subscribe-establishment budget")
}

// ─── Subscribe establishment ──────────────────────────────────────────────────

// An unbounded Subscribe establishment parks the session attempt forever: no
// error, no backoff, no log line, and nothing for the classifier to see, because
// sessionOnce simply never returns. The budget turns that into an expiry the
// daemon can act on.
func TestSubscribeEstablishmentIsBounded(t *testing.T) {
	// No t.Parallel(): this test depends on a package-level budget var.
	shortenBudget(t, &subscribeSetupTimeout, 50*time.Millisecond)

	var (
		mu     sync.Mutex
		gotCtx context.Context
	)
	d := &Daemon{logger: discardLogger()}
	d.subscribeFn = func(ctx context.Context) (pb.HubFuse_SubscribeClient, error) {
		mu.Lock()
		gotCtx = ctx
		mu.Unlock()
		// A wedged establishment: it returns only because the budget cancels
		// the context underneath it.
		<-ctx.Done()
		return nil, ctx.Err()
	}

	done := make(chan error, 1)
	go func() {
		_, err := d.subscribeWithBudget(context.Background())
		done <- err
	}()

	select {
	case err := <-done:
		require.Error(t, err)
		assert.ErrorIs(t, err, errSessionTimedOut,
			"an establishment killed by our own budget is the sentinel, not an ordinary failure")
	case <-time.After(5 * time.Second):
		t.Fatal("subscribeWithBudget never returned; the establishment is unbounded")
	}

	// No orphan: the abandoned call's context is cancelled, so a subscription
	// cannot stay open on the hub for a session the daemon considers dead —
	// which the hub's #69 recovery gate would read as "this daemon is still here".
	mu.Lock()
	ctx := gotCtx
	mu.Unlock()
	require.NotNil(t, ctx)
	assert.Error(t, ctx.Err(), "the abandoned establishment must not be left running")
}

// On the success path the stream keeps the session's lifetime, not the budget's:
// the budget bounds establishment only.
func TestSubscribeStreamOutlivesItsEstablishmentBudget(t *testing.T) {
	// No t.Parallel(): this test depends on a package-level budget var.
	shortenBudget(t, &subscribeSetupTimeout, 50*time.Millisecond)

	var (
		mu     sync.Mutex
		gotCtx context.Context
	)
	d := &Daemon{logger: discardLogger()}
	d.subscribeFn = func(ctx context.Context) (pb.HubFuse_SubscribeClient, error) {
		mu.Lock()
		gotCtx = ctx
		mu.Unlock()
		return nil, nil
	}

	sessionCtx, cancelSession := context.WithCancel(context.Background())
	defer cancelSession()

	_, err := d.subscribeWithBudget(sessionCtx)
	require.NoError(t, err)

	// Well past the budget, the stream's context is still alive.
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	streamCtx := gotCtx
	mu.Unlock()
	require.NotNil(t, streamCtx)
	assert.NoError(t, streamCtx.Err(), "the established stream must not die with its setup budget")

	// And it still ends with the session.
	cancelSession()
	assert.Error(t, streamCtx.Err(), "the stream must die with the session")
}

// A genuine Subscribe failure keeps its own error: only an expiry of the
// daemon's budget is the sentinel.
func TestSubscribeEstablishmentFailurePreservesItsError(t *testing.T) {
	// No t.Parallel(): this test depends on a package-level budget var.
	sentinel := errors.New("hub said no")
	d := &Daemon{logger: discardLogger()}
	d.subscribeFn = func(context.Context) (pb.HubFuse_SubscribeClient, error) {
		return nil, sentinel
	}

	_, err := d.subscribeWithBudget(context.Background())
	assert.ErrorIs(t, err, sentinel)
	assert.NotErrorIs(t, err, errSessionTimedOut,
		"a hub that answered must never be reported as our budget expiring")
}

// ─── Deadlines on the remaining unbounded calls ───────────────────────────────

// deadlineRecordingHub answers ListDevices while reporting the deadline it saw,
// so a test can assert the call was bounded without waiting the budget out.
type deadlineRecordingHub struct {
	pb.UnimplementedHubFuseServer

	mu       sync.Mutex
	deadline time.Time
	hadOne   bool
}

func (s *deadlineRecordingHub) ListDevices(ctx context.Context, _ *pb.ListDevicesRequest) (*pb.ListDevicesResponse, error) {
	dl, ok := ctx.Deadline()
	s.mu.Lock()
	s.deadline, s.hadOne = dl, ok
	s.mu.Unlock()
	return &pb.ListDevicesResponse{}, nil
}

// seedNicknamesFromHub runs on the startup path, before the SSH server and the
// first Register, on a connection that has never carried a call — and its caller
// passes the daemon's own context, which is cancelled only at shutdown. An
// unanswered ListDevices there hangs Run itself.
func TestSeedNicknamesFromHubIsBounded(t *testing.T) {
	t.Parallel()

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err, "listen")
	hub := &deadlineRecordingHub{}
	srv := grpc.NewServer()
	pb.RegisterHubFuseServer(srv, hub)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	client := dialStubHub(t, lis.Addr().String())
	t.Cleanup(func() { _ = client.Close() })

	d := &Daemon{logger: discardLogger(), nicknames: map[string]string{}}
	d.setHubClient(client)

	before := time.Now()
	d.seedNicknamesFromHub(context.Background())

	hub.mu.Lock()
	deadline, hadOne := hub.deadline, hub.hadOne
	hub.mu.Unlock()

	require.True(t, hadOne, "the hub must see a deadline on ListDevices")
	assert.WithinDuration(t, before.Add(listDevicesTimeout), deadline, time.Second,
		"the deadline must be the ListDevices budget")
}

// The share publish that follows a config reload runs on the fsnotify watcher's
// goroutine, which has no context and no supervisor: an RPC that never returned
// would stop the daemon reacting to any further config change for good.
func TestConfigReloadPublishIsBounded(t *testing.T) {
	t.Parallel()

	d, cfgPath := buildTestDaemon(t)

	got := make(chan context.Context, 1)
	d.updateSharesFn = func(ctx context.Context, _ []*pb.Share) error {
		got <- ctx
		return nil
	}

	dir := filepath.Dir(cfgPath)
	shareDir := filepath.Join(dir, "docs")
	require.NoError(t, os.MkdirAll(shareDir, 0o755), "mkdir share")

	oldCfg := d.config
	newCfg := &agentconfig.Config{}
	*newCfg = *oldCfg
	newCfg.Shares = []agentconfig.ShareConfig{{Alias: "docs", Path: shareDir, Permissions: "ro"}}

	before := time.Now()
	d.onConfigChange(oldCfg, newCfg)

	select {
	case ctx := <-got:
		deadline, ok := ctx.Deadline()
		require.True(t, ok, "the share publish must carry a deadline")
		assert.WithinDuration(t, before.Add(updateSharesTimeout), deadline, time.Second,
			"the deadline must be the UpdateShares budget")
	case <-time.After(5 * time.Second):
		t.Fatal("the config reload never published shares")
	}
}

// ─── The one-shot dial ────────────────────────────────────────────────────────

// writeConnectorCerts writes a usable CA/client certificate set and returns the
// three paths, plus the directory holding them.
func writeConnectorCerts(t *testing.T) (dir, caPath, certPath, keyPath string) {
	t.Helper()

	dir = t.TempDir()
	caCert, caKey, err := common.GenerateCA()
	require.NoError(t, err, "GenerateCA")
	caPEM := common.EncodeCACertPEM(caCert)
	certPEM, keyPEM, err := common.SignClientCert(caCert, caKey, "connector-test")
	require.NoError(t, err, "SignClientCert")

	caPath = filepath.Join(dir, "ca.crt")
	certPath = filepath.Join(dir, "client.crt")
	keyPath = filepath.Join(dir, "client.key")
	require.NoError(t, os.WriteFile(caPath, caPEM, 0o600), "write ca")
	require.NoError(t, os.WriteFile(certPath, certPEM, 0o600), "write cert")
	require.NoError(t, os.WriteFile(keyPath, keyPEM, 0o600), "write key")

	return dir, caPath, certPath, keyPath
}

// Dial is the seam the replacement path uses, and it must return once. Its only
// caller already runs inside reconnectSession's backoff loop; a second retry
// loop nested inside would make the outer one's timing a fiction — which is the
// nested-backoff objection that sank design idea B.
func TestConnectorDialDoesNotRetry(t *testing.T) {
	t.Parallel()

	_, caPath, certPath, keyPath := writeConnectorCerts(t)
	c := NewConnector("127.0.0.1:1", caPath, certPath, filepath.Join(filepath.Dir(keyPath), "missing.key"), discardLogger())

	start := time.Now()
	client, err := c.Dial()
	elapsed := time.Since(start)

	require.Error(t, err, "a missing key must fail the dial")
	assert.Nil(t, client)
	assert.Less(t, elapsed, backoffInitial,
		"Dial must return immediately, not sleep through a backoff")
}

// The credentials are read once and cached. Re-reading them on every dial is not
// merely wasteful: `hubfuse join` writes the three files with three separate
// os.WriteFile calls, so a dial landing between them loads a mismatched pair,
// and a repeat join can mint a different device_id — silently changing the CN,
// which IS the daemon's identity to the hub.
func TestConnectorCachesCredentialsAfterFirstDial(t *testing.T) {
	t.Parallel()

	_, caPath, certPath, keyPath := writeConnectorCerts(t)
	c := NewConnector("127.0.0.1:1", caPath, certPath, keyPath, discardLogger())

	first, err := c.Dial()
	require.NoError(t, err, "first dial")
	t.Cleanup(func() { _ = first.Close() })

	// Removing the files must not affect a later dial: nothing reads them again.
	require.NoError(t, os.Remove(certPath), "remove cert")
	require.NoError(t, os.Remove(keyPath), "remove key")

	second, err := c.Dial()
	require.NoError(t, err, "a cached credential set must survive the files going away")
	t.Cleanup(func() { _ = second.Close() })
	assert.NotSame(t, first, second, "each dial must produce its own connection")
}

// A failed load must NOT be cached: today a daemon started with a missing or
// half-written client.crt retries until the files appear, and caching the error
// would make that retry loop permanently hopeless.
func TestConnectorDoesNotCacheACredentialLoadFailure(t *testing.T) {
	t.Parallel()

	dir, caPath, certPath, keyPath := writeConnectorCerts(t)
	certPEM, err := os.ReadFile(certPath)
	require.NoError(t, err)
	keyPEM, err := os.ReadFile(keyPath)
	require.NoError(t, err)

	// Start with the pair missing, as a daemon racing `hubfuse join` would.
	require.NoError(t, os.Remove(certPath))
	require.NoError(t, os.Remove(keyPath))

	c := NewConnector("127.0.0.1:1", caPath, certPath, keyPath, discardLogger())
	_, err = c.Dial()
	require.Error(t, err, "a dial without certificates must fail")

	require.NoError(t, os.WriteFile(filepath.Join(dir, "client.crt"), certPEM, 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "client.key"), keyPEM, 0o600))

	client, err := c.Dial()
	require.NoError(t, err, "the retry must succeed once the certificates appear")
	t.Cleanup(func() { _ = client.Close() })
}

// Connect is the bootstrap path; it must share the same cache, so every
// connection a daemon ever holds carries one identity.
func TestConnectSharesTheDialCredentialCache(t *testing.T) {
	t.Parallel()

	_, caPath, certPath, keyPath := writeConnectorCerts(t)
	c := NewConnector("127.0.0.1:1", caPath, certPath, keyPath, discardLogger())

	client, err := c.Connect(context.Background())
	require.NoError(t, err, "Connect")
	t.Cleanup(func() { _ = client.Close() })

	require.NoError(t, os.Remove(certPath), "remove cert")
	require.NoError(t, os.Remove(keyPath), "remove key")

	replacement, err := c.Dial()
	require.NoError(t, err, "a replacement dial must reuse what Connect loaded")
	t.Cleanup(func() { _ = replacement.Close() })
}
