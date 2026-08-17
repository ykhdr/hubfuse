package hub

import (
	"context"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/hub/store"
	pb "github.com/ykhdr/hubfuse/proto"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Config holds configuration for a Hub instance.
type Config struct {
	ListenAddr      string        // e.g. ":9090"
	DataDir         string        // e.g. "~/.hubfuse-hub"
	LogFile         string        // path to JSON log file ("" = no file logging)
	LogLevel        slog.Level    // file log level (default: Debug)
	Verbose         bool          // show debug logs in console
	ExtraSANs       []string      // additional SANs for the server TLS certificate
	DeviceRetention time.Duration // how long to keep offline devices before pruning (0 = never prune)
	JoinTokenTTL    time.Duration // how long issued join tokens remain valid (0 = use default 10m)

	// HeartbeatTimeout is how long a device may go without a heartbeat before
	// the monitor demotes it to offline (0 = use DefaultHeartbeatTimeout). The
	// stale-check cadence is derived from it (timeout/3), so lowering it makes
	// the hub notice a dead device sooner at the cost of more frequent sweeps.
	// Scenario tests shorten it so liveness behaviour is observable in seconds;
	// operators on slow links can raise it. (#69)
	HeartbeatTimeout time.Duration

	// OnReady, if non-nil, is invoked exactly once from Start right
	// after net.Listen returns — the TCP listener is bound and the
	// kernel is already queueing SYNs, and grpcServer.Serve runs
	// immediately after the callback. The cmd layer uses this hook to
	// write the PID file.
	OnReady func()
}

// lifecycleState tracks Hub across its three possible lives: never started,
// currently serving, or stopped. A bare sync.WaitGroup cannot stand in for
// this: wg.Wait() on a WaitGroup nothing has ever Add'd to returns instantly,
// which is exactly what let a Stop-before-Start sequence close the store and
// then have Start bring a server up over it — Stop and Start need a state
// they can both check for "did the other one already run", not just a
// counter of outstanding work. (#75)
type lifecycleState int

const (
	lifecycleNotStarted lifecycleState = iota
	lifecycleStarted
	lifecycleStopped
)

// Hub wires together the store, registry, heartbeat monitor, and gRPC server.
type Hub struct {
	config    Config
	store     store.Store
	registry  *Registry
	heartbeat *HeartbeatMonitor
	tlsCfg    *tls.Config
	logger    *slog.Logger

	// mu guards state, grpcServer, and cancel — the three fields Start writes
	// once and Stop reads to decide what, if anything, it has to tear down.
	mu         sync.Mutex
	state      lifecycleState
	grpcServer *grpc.Server
	// cancel ends the context Start's two background goroutines (the
	// heartbeat monitor and the join-token sweeper) run on, and bgWG is how
	// Stop waits for them to actually exit before it is safe to close the
	// store out from under them. (#75)
	cancel context.CancelFunc
	bgWG   sync.WaitGroup

	// stopOnce makes Stop idempotent, and stopSettled/stopErr are what it
	// caches: a second Stop call must hand back the exact outcome of the
	// first, not run the sequence again (which would close an
	// already-closed store) or return a zero value that looks like success.
	stopOnce    sync.Once
	stopSettled bool
	stopErr     error
}

// NewHub creates a Hub from the given config. It sets up the logger, opens
// (or creates) the SQLite database, and loads (or generates) the CA and
// server TLS certificates.
func NewHub(config Config) (*Hub, error) {
	logger, err := common.SetupLogger(common.LoggerOptions{
		LogFile:   config.LogFile,
		FileLevel: config.LogLevel,
		Verbose:   config.Verbose,
	})
	if err != nil {
		return nil, fmt.Errorf("setup logger: %w", err)
	}

	dataDir := common.ExpandHome(config.DataDir)

	if err := os.MkdirAll(dataDir, 0700); err != nil {
		return nil, fmt.Errorf("create data dir %q: %w", dataDir, err)
	}

	dbPath := filepath.Join(dataDir, common.DBFile)
	s, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}

	caCert, caKey, tlsCfg, err := loadOrGenerateCerts(dataDir, config.ExtraSANs, logger)
	if err != nil {
		s.Close()
		return nil, fmt.Errorf("load/generate certs: %w", err)
	}

	registry := NewRegistry(s, caCert, caKey, logger, config.JoinTokenTTL)
	heartbeat := NewHeartbeatMonitor(registry, s, config.HeartbeatTimeout, config.DeviceRetention, logger)

	return &Hub{
		config:    config,
		store:     s,
		registry:  registry,
		heartbeat: heartbeat,
		tlsCfg:    tlsCfg,
		logger:    logger,
	}, nil
}

// Start begins serving gRPC requests and starts the heartbeat monitor. It
// invokes OnReady (if set) once the listener is up, and blocks until the
// gRPC server stops.
//
// If Stop already ran — the Stop-before-Start ordering — Start returns nil
// without opening a listener, without calling OnReady, and without
// reconciling device statuses against a store that Stop may already have
// closed. The check runs before all three, in that order, because OnReady is
// what the cmd layer uses to write the PID file: a hub that will not serve
// must not leave behind a PID file that makes the next `hubfuse-hub start`
// report "already running". A second call to Start (state already
// lifecycleStarted) is refused the same way — there is nothing to start
// twice. (#75)
func (h *Hub) Start(ctx context.Context) error {
	h.mu.Lock()
	if h.state != lifecycleNotStarted {
		h.mu.Unlock()
		return nil
	}
	h.state = lifecycleStarted
	runCtx, cancel := context.WithCancel(ctx)
	h.cancel = cancel
	h.mu.Unlock()

	// Before anything is served: a hub that has just started has no online
	// devices, whatever the database says. See ReconcileDeviceStatuses. (#75)
	//
	// A Stop landing while this runs fails it, and not with an error worth
	// reporting: cancelling the context makes database/sql return
	// context.Canceled before it even takes the connection, and a Stop that
	// wins the race to store.Close() turns it into "sql: database is closed".
	// Both mean the shutdown got here first, which is a clean stop seen from an
	// unlucky angle — the same case the Serve call below already filters, and
	// the same systemd-records-a-failed-unit outcome if it is not filtered
	// here too. The state check is what separates that from a genuine store
	// failure at startup, which must still be fatal. (#75)
	if err := ReconcileDeviceStatuses(ctx, h.store, h.logger); err != nil {
		if h.stopped() {
			return nil
		}
		return err
	}

	creds := credentials.NewTLS(h.tlsCfg)

	grpcServer := grpc.NewServer(ServerOptions(creds, h.registry)...)

	srv := NewServer(h.registry, h.logger)
	pb.RegisterHubFuseServer(grpcServer, srv)

	lis, err := net.Listen("tcp", h.config.ListenAddr)
	if err != nil {
		return fmt.Errorf("listen on %q: %w", h.config.ListenAddr, err)
	}

	h.mu.Lock()
	if h.state == lifecycleStopped {
		// Stop ran concurrently while we were still setting up: same rule as
		// the top-of-function check, just re-evaluated because a listener now
		// exists that must not leak and an OnReady that must still not fire.
		h.mu.Unlock()
		lis.Close()
		return nil
	}
	h.grpcServer = grpcServer

	// Add and OnReady stay INSIDE the lock, with Stop's own critical section as
	// the other half of the handshake. Released any earlier, a Stop landing in
	// the gap would find a WaitGroup nothing had Add'd to — Wait returns
	// instantly on a zero counter — close the store, and then this function
	// would go on to write the PID file after shutdown had finished and start
	// two background goroutines against a closed store. The PID file is the
	// worst of those: a stale one makes the next `hubfuse-hub start` refuse
	// with "already running". (#75)
	h.bgWG.Add(2)
	if h.config.OnReady != nil {
		h.config.OnReady()
	}
	h.mu.Unlock()

	go func() {
		defer h.bgWG.Done()
		h.heartbeat.Start(runCtx)
	}()

	go func() {
		defer h.bgWG.Done()
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-t.C:
				if err := h.store.DeleteExpiredJoinTokens(runCtx); err != nil {
					logCancelable(h.logger, slog.LevelWarn, "sweep expired join tokens", err)
				}
			}
		}
	}()

	h.logger.Info("hub gRPC server starting", slog.String("addr", h.config.ListenAddr))

	if err := grpcServer.Serve(lis); err != nil {
		// Serve returns nil when a stop interrupts it, and ErrServerStopped only
		// when it is called after the server was already stopped — which is
		// exactly what a Stop landing between the unlock above and this call
		// produces. That is a clean shutdown observed from an unlucky angle, not
		// a startup failure, and reporting it as one would have systemd record a
		// failed unit for an ordinary `hubfuse-hub stop`. (#75)
		if errors.Is(err, grpc.ErrServerStopped) && h.stopped() {
			return nil
		}
		return fmt.Errorf("gRPC serve: %w", err)
	}

	return nil
}

// stopped reports whether Stop has already run. Used to tell a shutdown apart
// from a genuine serve failure. (#75)
func (h *Hub) stopped() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.state == lifecycleStopped
}

// Stop performs a bounded shutdown: broadcasts DeviceOffline for all online
// devices, closes every subscriber stream, stops the gRPC server within a hard
// time budget, and closes the store if the server stopped cleanly.
//
// settled reports whether the whole sequence finished inside
// DefaultShutdownBudget, NOT whether the graceful path was taken. StopForced
// (grace expired, but the forced Stop() returned in time) still settles — it
// is the on-schedule outcome for a wedged peer, the scenario #75 is filed
// about — it just does not get the store closed underneath a handler that
// might still be live (see stopResultFor). settled == false is reserved for
// StopHung (the gRPC server never came down even after being forced) and for
// the background goroutines outliving the budget once waited for; either way
// the caller should treat the process as needing an external kill rather than
// waiting any longer.
//
// Stop is idempotent: a second call returns the exact outcome of the first
// without running the sequence again, which matters because running it twice
// would close the store a second time. (#75)
func (h *Hub) Stop() (settled bool, err error) {
	h.stopOnce.Do(func() {
		h.stopSettled, h.stopErr = h.stop()
	})
	return h.stopSettled, h.stopErr
}

// stop is Stop's actual body, run at most once via stopOnce.
//
// The whole sequence lives under ONE deadline, not one per phase: four
// independent timeouts that each get the full budget can add up to four times
// DefaultShutdownBudget in the worst case, which is the same unbounded-tail
// failure mode #75 is about, just moved one level down. Every phase below
// instead draws on ctx's remaining time, so a slow sweep leaves less for
// StopServer and less again for the goroutine wait, and the caller's promise
// ("done within DefaultShutdownBudget, or told settled == false") holds
// regardless of where the time actually went. (#75)
func (h *Hub) stop() (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultShutdownBudget)
	defer cancel()

	h.mu.Lock()
	cancelBG := h.cancel
	grpcServer := h.grpcServer
	h.state = lifecycleStopped
	h.mu.Unlock()

	// Refuse heartbeat-driven recovery from here on. The sweep below marks every
	// online device offline while the gRPC server is still answering, so without
	// this a heartbeat processed inside that window would flip a device back to
	// online and leave a phantom-online row for the next hub start to serve to
	// its peers. (#69)
	h.registry.Drain()

	// Cancelled before the sweep, not after: the heartbeat monitor and the
	// join-token sweeper have no more useful work once shutdown has started
	// (their own writes on an about-to-close store would just be more of the
	// noise logCancelable exists to quiet), and cancelling first is what
	// bounds how long step 6 below has to wait for them. (#75)
	if cancelBG != nil {
		cancelBG()
	}

	h.sweepOnlineDevices(ctx)

	// Queued AFTER the sweep's broadcasts, not before: a receive from a closed
	// buffered channel drains the buffer first and only then reports !ok, so
	// every DeviceOffline the sweep just queued still reaches its subscriber
	// before that subscriber's stream ends. (#75)
	if closed := h.registry.CloseAllSubscribers(); closed > 0 {
		h.logger.Info("closed subscriber streams for shutdown", slog.Int("count", closed))
	}

	// A hub that never started (Stop before Start, or Start failed before
	// grpcServer was assigned) has nothing to bound and no live handlers, so it
	// is safe to close the store as if the graceful path had run.
	outcome := StopGraceful
	if grpcServer != nil {
		grace, hardLimit := splitRemaining(ctx, shutdownGrace)
		outcome = StopServer(grpcServer, grace, hardLimit, h.logger)
	}

	settled, closeStore := stopResultFor(outcome)
	if !closeStore {
		// StopForced still reports settled: Stop() returned inside the
		// budget, so the sequence finished on schedule — which is exactly
		// what a wedged peer (the scenario #75 is filed about) is supposed to
		// produce. What it does not give us is server.go:1985-1986's
		// handler-drain guarantee, which applies to the graceful path only,
		// so a still-live handler would see the database close out from under
		// it as "sql: database is closed" instead of the clean process exit
		// it would otherwise get with its WAL already committed — hence
		// closeStore is what gates store.Close(), not settled. (#75)
		return settled, nil
	}

	// The store's writer connection is serialized (SetMaxOpenConns(1)) with a
	// busy_timeout, so a heartbeat mid-write when cancelBG fired can still be
	// holding it; store.Close() underneath that write would race it. Waiting
	// here, rather than trusting cancellation alone, is what keeps that race
	// from ever happening, and running out of budget mid-wait means the store
	// is left open — the same live-writer hazard the closeStore gate above
	// exists to prevent.
	//
	// It still reports SETTLED. This branch is reachable only on StopGraceful,
	// so the gRPC server came down cleanly and grpc-go has already waited out
	// every handler; what is late here is one of the hub's OWN background
	// goroutines, which no client is waiting on. Reporting unsettled would exit
	// non-zero for a shutdown strictly healthier than the StopForced case that
	// exits zero a few lines up — and the trigger is mundane: a sweep that sat
	// out the store's 5s busy_timeout behind a concurrent `hubfuse-hub
	// issue-join` leaves splitRemaining almost nothing. (#75)
	bgDone := make(chan struct{})
	go func() {
		h.bgWG.Wait()
		close(bgDone)
	}()

	if !waitForBackground(ctx, bgDone) {
		h.logger.Warn("stop: background goroutines did not exit within the shutdown budget; leaving the store open")
		return true, nil
	}

	if err := h.store.Close(); err != nil {
		h.logger.Warn("stop: close store", slog.Any("error", err))
	}

	return true, nil
}

// waitForBackground reports whether done closed before ctx ran out. A false
// result gates store.Close() only — the caller still reports the shutdown
// settled, because this runs on the graceful path where the server is already
// down.
//
// The two-stage select is the point of the function. Callers reach it with a
// budget that is routinely already spent, so a single select over both channels
// would choose at random whenever the goroutines had finished AND the deadline
// had passed — Go picks uniformly among ready cases — and the store would be
// closed or left open by coin toss on runs that differ in nothing. Checking
// done first makes "finished, if only just" win deterministically.
//
// It takes the channel rather than the WaitGroup so the choice above is what
// gets tested. Given a WaitGroup it would have to start the waiting goroutine
// itself, and whether that goroutine had reached its close by the first check
// would be down to the scheduler — the test would race the code instead of
// pinning it. (#75)
func waitForBackground(ctx context.Context, done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
	}

	select {
	case <-done:
		return true
	case <-ctx.Done():
		return false
	}
}

// stopResultFor translates StopServer's outcome into the two separate
// questions Hub.stop needs answered.
//
// settled and closeStore are not the same question. settled tells the caller
// (main.go, eventually) whether the shutdown finished inside
// DefaultShutdownBudget at all — StopHung is the only outcome where it did
// not, since even the forced Stop() never returned. StopForced DID return
// inside the budget: forcing the server down and having Stop() come back is
// the expected, on-schedule outcome for a wedged peer, which is the exact
// scenario #75 is filed about, so settled is true there too. closeStore is
// narrower: only StopGraceful carries server.go:1985-1986's guarantee that
// every handler has already returned before it does, so it is the only
// outcome safe to close the store under — StopForced settled on time, but a
// handler forced-stop merely disconnected, rather than one whose own return
// path completed, could still be mid-call against the store. (#75)
func stopResultFor(outcome StopOutcome) (settled, closeStore bool) {
	switch outcome {
	case StopGraceful:
		return true, true
	case StopForced:
		return true, false
	default: // StopHung
		return false, false
	}
}

// splitRemaining divides ctx's remaining time into a grace window (capped at
// want) and whatever is left over for the hard limit that follows it,
// so a phase that starts late still fits inside ctx's deadline instead of
// re-measuring want and the hard limit from a fresh clock. A ctx already at
// or past its deadline yields (0, 0), which StopServer treats the same as any
// other exhausted budget — it still tries, it just does not wait.
func splitRemaining(ctx context.Context, want time.Duration) (grace, hardLimit time.Duration) {
	remaining := time.Until(deadlineOf(ctx))
	if remaining < 0 {
		remaining = 0
	}
	grace = want
	if grace > remaining {
		grace = remaining
	}
	return grace, remaining - grace
}

// deadlineOf returns ctx's deadline, or the zero time's far future stand-in
// (now, so remaining computes to 0) if ctx carries none — stop always builds
// ctx with context.WithTimeout, so the fallback is defensive only.
func deadlineOf(ctx context.Context) time.Time {
	if dl, ok := ctx.Deadline(); ok {
		return dl
	}
	return time.Now()
}

// sweepOnlineDevices marks every online device offline and tells the remaining
// peers about it, in that order.
//
// The order is the whole point. The durable write goes first, so that a sweep
// cut short by its deadline leaves "row offline, event undelivered" — the peer
// finds out on its own liveness timeout, and the next hub start serves a correct
// list. The reverse leaves "event delivered, row still online", which is the
// phantom #69 was about: a device the hub announces as up, forever, because the
// only writer that would have corrected it is gone.
//
// The write is one statement rather than one per device. The store is opened
// with SetMaxOpenConns(1) and busy_timeout=5000, so a single UPDATE can wait
// five seconds behind a concurrent `hubfuse-hub issue-join`; N of them can
// outlast the SIGTERM window on their own, and the whole shutdown runs under one
// budget. (#75)
func (h *Hub) sweepOnlineDevices(ctx context.Context) {
	online, err := h.store.ListOnlineDevices(ctx)
	if err != nil {
		h.logger.Warn("stop: list online devices", slog.Any("error", err))
		return
	}
	if len(online) == 0 {
		return
	}

	if _, err := h.store.MarkAllOffline(ctx); err != nil {
		// Nothing is broadcast on this path: peers told a device is offline by a
		// hub whose database still says otherwise would be corrected on the next
		// start, back to a device that is not there.
		h.logger.Warn("stop: mark devices offline", slog.Any("error", err))
		return
	}

	for _, d := range online {
		h.registry.BroadcastAll(&pb.Event{
			Payload: &pb.Event_DeviceOffline{
				DeviceOffline: &pb.DeviceOfflineEvent{
					DeviceId: d.DeviceID,
					Nickname: d.Nickname,
				},
			},
		})
	}
}

// loadOrGenerateCerts loads existing CA and server TLS certificates from
// dataDir/tls/, or generates and saves them if they do not exist. When
// generating, it auto-detects local IPs/hostnames and merges extraSANs.
func loadOrGenerateCerts(dataDir string, extraSANs []string, logger *slog.Logger) (*x509.Certificate, *rsa.PrivateKey, *tls.Config, error) {
	tlsDir := filepath.Join(dataDir, common.TLSDir)

	caCertPath := filepath.Join(tlsDir, common.CACertFile)
	caKeyPath := filepath.Join(tlsDir, common.CAKeyFile)
	serverCertPath := filepath.Join(tlsDir, common.ServerCertFile)
	serverKeyPath := filepath.Join(tlsDir, common.ServerKeyFile)

	if fileExists(caCertPath) && fileExists(caKeyPath) && fileExists(serverCertPath) && fileExists(serverKeyPath) {
		logger.Info("loading existing TLS certificates", slog.String("tls_dir", tlsDir))
		caCert, caKey, err := loadCACertAndKey(caCertPath, caKeyPath)
		if err != nil {
			return nil, nil, nil, err
		}
		tlsCfg, err := common.LoadTLSServerConfig(caCertPath, serverCertPath, serverKeyPath)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("load TLS config: %w", err)
		}
		return caCert, caKey, tlsCfg, nil
	}

	logger.Info("generating new TLS certificates", slog.String("tls_dir", tlsDir))

	if err := os.MkdirAll(tlsDir, 0700); err != nil {
		return nil, nil, nil, fmt.Errorf("create tls dir %q: %w", tlsDir, err)
	}

	caCert, caKey, err := common.GenerateCA()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generate CA: %w", err)
	}

	caCertPEM := common.EncodeCACertPEM(caCert)
	caKeyPEM := common.EncodeCAKeyPEM(caKey)

	if err := os.WriteFile(caCertPath, caCertPEM, 0644); err != nil {
		return nil, nil, nil, fmt.Errorf("write CA cert: %w", err)
	}
	if err := os.WriteFile(caKeyPath, caKeyPEM, 0600); err != nil {
		return nil, nil, nil, fmt.Errorf("write CA key: %w", err)
	}

	hosts := common.LocalHosts()
	hosts = append(hosts, extraSANs...)
	hosts = dedup(hosts)

	logger.Info("generating server TLS certificate", slog.Any("sans", hosts))

	serverCertPEM, serverKeyPEM, err := common.GenerateServerCert(caCert, caKey, hosts)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("generate server cert: %w", err)
	}

	if err := common.SaveCertAndKey(serverCertPath, serverKeyPath, serverCertPEM, serverKeyPEM); err != nil {
		return nil, nil, nil, fmt.Errorf("save server cert/key: %w", err)
	}

	tlsCfg, err := common.LoadTLSServerConfig(caCertPath, serverCertPath, serverKeyPath)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load TLS config: %w", err)
	}

	return caCert, caKey, tlsCfg, nil
}

// loadCACertAndKey reads the CA certificate and private key from disk.
func loadCACertAndKey(caCertPath, caKeyPath string) (*x509.Certificate, *rsa.PrivateKey, error) {
	certDER, err := common.LoadPEM(caCertPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load CA cert: %w", err)
	}

	caCert, err := x509.ParseCertificate(certDER)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA cert: %w", err)
	}

	keyDER, err := common.LoadPEM(caKeyPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load CA key: %w", err)
	}

	caKey, err := x509.ParsePKCS1PrivateKey(keyDER)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA key: %w", err)
	}

	return caCert, caKey, nil
}

// fileExists reports whether a file exists and is not a directory.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// dedup returns a sorted, deduplicated copy of ss.
func dedup(ss []string) []string {
	seen := make(map[string]struct{}, len(ss))
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
