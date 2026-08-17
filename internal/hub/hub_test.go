package hub

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/hub/store"
)

func TestLoadOrGenerateCerts_AutoSANs(t *testing.T) {
	dataDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	_, _, _, err := loadOrGenerateCerts(dataDir, nil, logger)
	require.NoError(t, err)

	cert := parseServerCert(t, dataDir)

	// Baseline: must contain localhost in DNSNames and 127.0.0.1 in IPAddresses.
	assert.True(t, containsString(cert.DNSNames, "localhost"),
		`server cert DNSNames missing "localhost": %v`, cert.DNSNames)
	assert.True(t, containsIP(cert.IPAddresses, net.ParseIP("127.0.0.1")),
		"server cert IPAddresses missing 127.0.0.1: %v", cert.IPAddresses)

	// Hostname should be present (as DNS name if not an IP).
	hostname, err := os.Hostname()
	if err == nil && hostname != "" && net.ParseIP(hostname) == nil {
		assert.True(t, containsString(cert.DNSNames, hostname),
			"server cert DNSNames missing hostname %q: %v", hostname, cert.DNSNames)
	}
}

func TestLoadOrGenerateCerts_ExtraSANs(t *testing.T) {
	dataDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	extra := []string{"10.99.99.1", "custom.example.com"}
	_, _, _, err := loadOrGenerateCerts(dataDir, extra, logger)
	require.NoError(t, err)

	cert := parseServerCert(t, dataDir)

	assert.True(t, containsIP(cert.IPAddresses, net.ParseIP("10.99.99.1")),
		"server cert IPAddresses missing 10.99.99.1: %v", cert.IPAddresses)
	assert.True(t, containsString(cert.DNSNames, "custom.example.com"),
		`server cert DNSNames missing "custom.example.com": %v`, cert.DNSNames)
}

func TestLoadOrGenerateCerts_ExistingCertsNotRegenerated(t *testing.T) {
	dataDir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))

	// First generation.
	_, _, _, err := loadOrGenerateCerts(dataDir, nil, logger)
	require.NoError(t, err, "first loadOrGenerateCerts")

	cert1 := parseServerCert(t, dataDir)

	// Second call with different extra SANs — should load existing, not regenerate.
	_, _, _, err = loadOrGenerateCerts(dataDir, []string{"10.0.0.1"}, logger)
	require.NoError(t, err, "second loadOrGenerateCerts")

	cert2 := parseServerCert(t, dataDir)

	assert.Zero(t, cert1.SerialNumber.Cmp(cert2.SerialNumber),
		"server cert was regenerated: serial %v != %v", cert1.SerialNumber, cert2.SerialNumber)
}

// TestStart_ReconcilesOnlineDevicesFromAPreviousLife covers the invariant the
// bounded shutdown leans on.
//
// The hub trusts its stored statuses across restarts — Register answers a
// joining device with ListOnlineDevices — so an online row that outlived the
// previous process is served to peers as a live endpoint to mount. Today nothing
// clears those rows: the shutdown sweep is the only writer that ever did, and it
// does not run after SIGKILL, OOM or a power cut, nor necessarily in full once
// shutdown is bounded by a budget. The heartbeat monitor only notices a
// timeout/3 later.
//
// A hub that has just started has no online devices by definition — none of them
// has heartbeated yet — so this is settled before the first RPC is served. (#75)
func TestStart_ReconcilesOnlineDevicesFromAPreviousLife(t *testing.T) {
	dataDir := t.TempDir()

	// Leave behind exactly what a SIGKILLed hub leaves: an online row.
	seed, err := store.NewSQLiteStore(filepath.Join(dataDir, common.DBFile))
	require.NoError(t, err, "seed store")
	require.NoError(t, seed.CreateDevice(context.Background(), &store.Device{
		DeviceID:      "dev-ghost",
		Nickname:      "ghost",
		LastIP:        "10.0.0.9",
		SSHPort:       2222,
		Status:        store.StatusOnline,
		LastHeartbeat: time.Now().UTC(),
	}), "seed device")
	require.NoError(t, seed.Close(), "close seed store")

	ready := make(chan struct{})
	h, err := NewHub(Config{
		ListenAddr: "127.0.0.1:0",
		DataDir:    dataDir,
		LogLevel:   slog.LevelError,
		OnReady:    func() { close(ready) },
	})
	require.NoError(t, err, "NewHub")

	serveErr := make(chan error, 1)
	go func() { serveErr <- h.Start(context.Background()) }()

	select {
	case <-ready:
	case err := <-serveErr:
		t.Fatalf("hub exited before it was ready: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("hub never became ready")
	}

	// Reconciliation happens before the listener is bound, so by the time the
	// hub is ready no peer can ever have been told about the ghost.
	online, err := h.store.ListOnlineDevices(context.Background())
	require.NoError(t, err, "ListOnlineDevices")
	assert.Empty(t, online, "a freshly started hub must have no online devices")

	d, err := h.store.GetDevice(context.Background(), "dev-ghost")
	require.NoError(t, err, "GetDevice")
	assert.Equal(t, store.StatusOffline, d.Status)
	assert.Equal(t, "10.0.0.9", d.LastIP, "the endpoint must survive so a heartbeat can recover the device")
	assert.Equal(t, 2222, d.SSHPort)

	settled, err := h.Stop()
	require.NoError(t, err, "Stop")
	assert.True(t, settled, "an idle hub's shutdown must settle inside the budget")
	require.NoError(t, <-serveErr, "Start must return cleanly after Stop")
}

// newUnstartedTestHub builds a Hub against a fresh temp data dir without
// calling Start. onReady, if non-nil, is invoked from Config.OnReady.
func newUnstartedTestHub(t *testing.T, onReady func()) *Hub {
	t.Helper()
	h, err := NewHub(Config{
		ListenAddr: "127.0.0.1:0",
		DataDir:    t.TempDir(),
		LogLevel:   slog.LevelError,
		OnReady:    onReady,
	})
	require.NoError(t, err, "NewHub")
	return h
}

// TestStop_BeforeStart_StartReturnsCleanlyWithoutServingOrWritingPIDFile
// covers the hole Stop-before-Start used to leave open: Stop would close the
// store, and Start — reading no state that told it Stop had already run —
// would go on to net.Listen, invoke OnReady (the cmd layer's PID-file hook),
// and call Serve over a closed store. If this regresses, a hub that will
// never actually serve leaves a PID file behind that makes the next
// `hubfuse-hub start` refuse with "hub already running". (#75)
func TestStop_BeforeStart_StartReturnsCleanlyWithoutServingOrWritingPIDFile(t *testing.T) {
	var onReadyCalled atomic.Bool
	h := newUnstartedTestHub(t, func() { onReadyCalled.Store(true) })

	settled, err := h.Stop()
	require.NoError(t, err, "Stop before Start")
	assert.True(t, settled, "a hub that never started has nothing to bound, so shutdown settles trivially")

	startErr := make(chan error, 1)
	go func() { startErr <- h.Start(context.Background()) }()

	select {
	case err := <-startErr:
		assert.NoError(t, err, "Start after Stop must return cleanly, not fail")
	case <-time.After(2 * time.Second):
		t.Fatal("Start after Stop must return promptly instead of trying to serve over a closed store")
	}

	assert.False(t, onReadyCalled.Load(), "a hub that refuses to serve must never invoke OnReady")
}

// TestStop_CalledTwice_ReturnsSameOutcomeAndDoesNotRunTwice covers Stop's
// idempotency contract: a second call — including one racing the first —
// must observe the exact cached outcome of the one real run, not execute the
// sequence again. Running it twice would call store.Close() a second time and
// re-drive StopServer against a *grpc.Server already stopped. If this
// regresses under -race, two concurrent Stop calls each running the sequence
// is exactly the kind of double-close/double-broadcast race sync.Once exists
// to rule out. (#75)
func TestStop_CalledTwice_ReturnsSameOutcomeAndDoesNotRunTwice(t *testing.T) {
	ready := make(chan struct{})
	h := newUnstartedTestHub(t, func() { close(ready) })

	serveErr := make(chan error, 1)
	go func() { serveErr <- h.Start(context.Background()) }()

	select {
	case <-ready:
	case err := <-serveErr:
		t.Fatalf("hub exited before it was ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("hub never became ready")
	}

	type result struct {
		settled bool
		err     error
	}
	results := make([]result, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	for i := range results {
		go func(i int) {
			defer wg.Done()
			results[i].settled, results[i].err = h.Stop()
		}(i)
	}
	wg.Wait()

	assert.Equal(t, results[0], results[1], "concurrent Stop calls must observe the same cached outcome")

	settled3, err3 := h.Stop()
	assert.Equal(t, results[0].settled, settled3, "a later Stop call must still return the first outcome")
	assert.Equal(t, results[0].err, err3, "a later Stop call must still return the first outcome")

	require.NoError(t, <-serveErr, "Start must return cleanly after Stop")
}

// TestStop_AwaitsBackgroundGoroutinesBeforeClosingStore covers the ordering
// Start's sync.WaitGroup exists to enforce: Stop cancels the heartbeat
// monitor's context and then waits for it to actually exit before closing
// the store. If Stop instead closed the store right after cancelling — trusting
// cancellation alone rather than waiting for bgWG — a heartbeat check already
// past its ctx.Done() select and into a store call would still be live when
// Close() runs, and the next 1s-cadence tick (HeartbeatTimeout floors the
// check interval to 1s) would hit a closed *sql.DB and log "sql: database is
// closed" — which is not context.Canceled, so logCancelable would not quiet
// it. Asserting silence past that point is what makes this regression
// visible. (#75)
func TestStop_AwaitsBackgroundGoroutinesBeforeClosingStore(t *testing.T) {
	dataDir := t.TempDir()
	logPath := filepath.Join(dataDir, "hub.log")

	ready := make(chan struct{})
	h, err := NewHub(Config{
		ListenAddr:       "127.0.0.1:0",
		DataDir:          dataDir,
		LogFile:          logPath,
		LogLevel:         slog.LevelDebug,
		HeartbeatTimeout: 3 * time.Second, // floors to a 1s stale-check cadence
		OnReady:          func() { close(ready) },
	})
	require.NoError(t, err, "NewHub")

	serveErr := make(chan error, 1)
	go func() { serveErr <- h.Start(context.Background()) }()

	select {
	case <-ready:
	case err := <-serveErr:
		t.Fatalf("hub exited before it was ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("hub never became ready")
	}

	settled, err := h.Stop()
	require.NoError(t, err, "Stop")
	assert.True(t, settled, "an idle hub's shutdown must settle inside the budget")
	require.NoError(t, <-serveErr, "Start must return cleanly after Stop")

	// Give the heartbeat monitor at least two more would-be ticks to prove it
	// actually exited rather than merely being about to.
	time.Sleep(2500 * time.Millisecond)

	data, err := os.ReadFile(logPath)
	require.NoError(t, err, "read log file")
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		var entry map[string]any
		require.NoError(t, json.Unmarshal(line, &entry), "log line: %s", line)
		assert.NotEqual(t, "ERROR", entry["level"], "unexpected error logged after Stop returned: %s", line)
	}
}

// TestStopResultFor_SettledAndCloseStoreAreSeparateQuestions pins a
// distinction that is easy to collapse and expensive when collapsed: "did the
// shutdown finish inside its budget" and "may the store be closed" have
// different answers for StopForced.
//
// StopForced settles. The server came down within the budget — it just had to
// be pushed, because a handler blocked in stream.Send is freed by closing the
// transport and by nothing else. That is the ORDINARY outcome for the wedged
// subscriber this issue was filed about. The caller turns "not settled" into a
// non-zero exit, so answering false here would make the issue's own
// reproduction report a failed shutdown and exit 1 on a hub that did exactly
// what was asked of it.
//
// It still must not close the store: grpc-go waits for handlers on the
// graceful path only (server.go:1985-1986), so a live handler would meet
// "sql: database is closed" instead of the clean exit it gets from the process
// going away with its WAL committed. (#75)
func TestStopResultFor_SettledAndCloseStoreAreSeparateQuestions(t *testing.T) {
	tests := []struct {
		name            string
		outcome         StopOutcome
		wantSettled     bool
		wantCloseStore  bool
		whyItMattersMsg string
	}{
		{
			name: "graceful", outcome: StopGraceful,
			wantSettled: true, wantCloseStore: true,
			whyItMattersMsg: "every handler has returned, so the store is safe to close",
		},
		{
			name: "forced", outcome: StopForced,
			wantSettled: true, wantCloseStore: false,
			whyItMattersMsg: "the wedged-peer case: bounded stop worked, but handlers are still live",
		},
		{
			name: "hung", outcome: StopHung,
			wantSettled: false, wantCloseStore: false,
			whyItMattersMsg: "only this one needs the caller to force the process out",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			settled, closeStore := stopResultFor(tc.outcome)
			assert.Equal(t, tc.wantSettled, settled, "settled: %s", tc.whyItMattersMsg)
			assert.Equal(t, tc.wantCloseStore, closeStore, "closeStore: %s", tc.whyItMattersMsg)
		})
	}
}

// TestSplitRemaining_DividesCtxDeadlineNotFullWant covers the fix at the
// heart of the "one budget" requirement: StopServer's grace and hardLimit
// must come from what is left of the shutdown deadline, not be re-measured
// from a full grace+hardLimit on top of whatever earlier phases already
// spent. If this regresses to want and a hardcoded hardLimit added
// independently of ctx, a slow sweep or CloseAllSubscribers no longer
// shortens what StopServer gets — the total sequence can run past
// DefaultShutdownBudget by as much as those earlier phases took.
func TestSplitRemaining_DividesCtxDeadlineNotFullWant(t *testing.T) {
	t.Run("plenty of budget left: grace gets exactly what it asked for", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		grace, hardLimit := splitRemaining(ctx, 3*time.Second)

		assert.InDelta(t, 3*time.Second, grace, float64(200*time.Millisecond))
		assert.InDelta(t, 2*time.Second, hardLimit, float64(200*time.Millisecond))
	})

	t.Run("less remaining than want: grace is capped to what is left, hardLimit gets nothing", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		grace, hardLimit := splitRemaining(ctx, 3*time.Second)

		assert.InDelta(t, 2*time.Second, grace, float64(200*time.Millisecond))
		assert.Equal(t, time.Duration(0), hardLimit)
	})

	t.Run("deadline already passed: both windows are zero, not negative", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), -1*time.Second)
		defer cancel()

		grace, hardLimit := splitRemaining(ctx, 3*time.Second)

		assert.Equal(t, time.Duration(0), grace)
		assert.Equal(t, time.Duration(0), hardLimit)
	})
}

func TestDedup(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{"empty", nil, []string{}},
		{"no_duplicates", []string{"b", "a", "c"}, []string{"a", "b", "c"}},
		{"with_duplicates", []string{"a", "b", "a", "c", "b"}, []string{"a", "b", "c"}},
		{"single", []string{"x"}, []string{"x"}},
		{"all_same", []string{"a", "a", "a"}, []string{"a"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := dedup(tc.in)
			assert.Equal(t, tc.want, got, "dedup(%v)", tc.in)
			assert.True(t, sort.StringsAreSorted(got), "dedup result not sorted: %v", got)
		})
	}
}

// parseServerCert reads and parses the server certificate from dataDir/tls/server.crt.
func parseServerCert(t *testing.T, dataDir string) *x509.Certificate {
	t.Helper()
	certPEM, err := os.ReadFile(filepath.Join(dataDir, "tls", "server.crt"))
	require.NoError(t, err, "read server.crt")
	block, _ := pem.Decode(certPEM)
	require.NotNil(t, block, "no PEM block in server.crt")
	cert, err := x509.ParseCertificate(block.Bytes)
	require.NoError(t, err, "parse server.crt")
	return cert
}

func containsString(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

func containsIP(ips []net.IP, target net.IP) bool {
	for _, ip := range ips {
		if ip.Equal(target) {
			return true
		}
	}
	return false
}
