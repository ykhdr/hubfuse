package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	agentconfig "github.com/ykhdr/hubfuse/internal/agent/config"
	"github.com/ykhdr/hubfuse/internal/common"
	pb "github.com/ykhdr/hubfuse/proto"
	gossh "golang.org/x/crypto/ssh"
)

// OnlineDevice holds the known state of a currently-online remote device.
type OnlineDevice struct {
	DeviceID string
	Nickname string
	IP       string
	SSHPort  int
	Shares   []string // share aliases
}

// DaemonOptions configures a Daemon at construction time.
type DaemonOptions struct {
	// OnReady, if non-nil, is invoked exactly once by Run, immediately
	// after successful Register with the hub. The cmd layer uses this
	// hook to write the PID file.
	OnReady func()
}

// Daemon is the main orchestrator that ties together hub client, mounter,
// SSH server, config watcher, heartbeat, and event handling.
type Daemon struct {
	config        *agentconfig.Config
	configPath    string
	identity      *DeviceIdentity
	hubClient     *HubClient
	connector     *Connector
	mounter       *Mounter
	sshServer     *SSHServer
	watcher       *agentconfig.Watcher
	logger        *slog.Logger
	onlineDevices map[string]*OnlineDevice // online devices from hub (device_id -> info)
	// nicknames is a persisted device_id → nickname fallback so paired peers
	// resolve even before their DeviceOnline event arrives (e.g. right after a
	// daemon restart).  It is guarded by mu; disk I/O happens outside the lock.
	nicknames map[string]string
	mu        sync.RWMutex

	// dataDir is the base data directory (~/.hubfuse by default).
	dataDir string

	// sshPort is the port the embedded SSH server bound at startup. It is
	// captured once in NewDaemon and never changes: the SSH server binds its port
	// once and is not restarted on config hot-reload, so every (re)Register must
	// advertise THIS port — not the live config's ssh-port — or a roaming peer
	// would remount to an endpoint the daemon isn't actually listening on. Read
	// without a lock: it is written once, before any goroutine starts. (#61)
	sshPort int

	onReady func()

	// readyOnce guards onReady so it fires exactly once for the daemon's
	// lifetime. The supervisor re-runs sessionOnce on every reconnect, but the
	// PID-file hook (onReady) must run only on the first successful Register.
	readyOnce sync.Once

	// minReconnectInterval is the floor on how frequently the supervisor starts a
	// new hub session. It serves two roles, both keyed off the same minimum so a
	// flapping hub is never hammered: (1) supervise waits out this interval when a
	// session died sooner than it — a Subscribe stream that re-establishes then
	// dies almost immediately must not let supervise spin with zero delay (CPU
	// peg, back-to-back Register+Subscribe, log flood); (2) reconnectSession uses
	// it as the initial retry backoff, doubling up to backoffMax on repeated
	// Register failures. Defaults to backoffInitial (set in NewDaemon);
	// buildTestDaemon leaves it 0 so unit tests neither sleep on the floor nor on
	// retries. (#61)
	minReconnectInterval time.Duration

	// mountMonitorInterval is the cadence of the background mount-health monitor
	// (runMountMonitor): every tick it probes the active mounts and heals the
	// ones CONFIRMED dead (see healDeadMounts). The monitor exists for the
	// event-less windows the Subscribe stream cannot cover — an sshfs that dies
	// while its peer stays registered at the same endpoint never produces a hub
	// event at all. NewDaemon sets defaultMountMonitorInterval, overridable via
	// the HUBFUSE_MOUNT_MONITOR_INTERVAL env var (a test handle for scenario
	// tests, like HUBFUSE_STUB_MOUNT_DIR); <= 0 disables the monitor.
	// buildTestDaemon bypasses NewDaemon and leaves this 0, so unit tests never
	// run the monitor unless they set it explicitly. (#67)
	mountMonitorInterval time.Duration

	// registerFn and subscribeFn are injectable seams over the concrete
	// HubClient (client.go has no interface, so it cannot be stubbed without a
	// live gRPC connection). NewDaemon wires them to delegate to
	// d.hubClient.Register / d.hubClient.Subscribe; unit tests override them with
	// fakes to drive sessionOnce / reconnectSession / supervise without a live hub.
	registerFn  func(ctx context.Context, shares []*pb.Share, sshPort int) (*pb.RegisterResponse, error)
	subscribeFn func(ctx context.Context) (pb.HubFuse_SubscribeClient, error)

	// updateSharesFn is the same kind of seam over HubClient.UpdateShares so the
	// config-watcher's share-publish path (onConfigChange) is testable without a
	// live gRPC connection. NewDaemon wires it to d.hubClient.UpdateShares; unit
	// tests override it to observe the publish (e.g. assert d.config is already
	// swapped to the new pointer by the time shares are pushed to the hub). (#61)
	updateSharesFn func(ctx context.Context, shares []*pb.Share) error
}

// NewDaemon loads the config and identity, creates the connector, mounter, and
// SSH server, and returns a ready-to-run Daemon.
func NewDaemon(cfgPath string, logger *slog.Logger, opts DaemonOptions) (*Daemon, error) {
	cfg, err := agentconfig.Load(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("load config %q: %w", cfgPath, err)
	}

	dir := filepath.Dir(cfgPath)

	identityPath := filepath.Join(dir, "device.json")
	identity, err := LoadIdentity(identityPath)
	if err != nil {
		return nil, fmt.Errorf("load identity: %w", err)
	}

	tlsDir := filepath.Join(dir, "tls")
	caCertPath := filepath.Join(tlsDir, "ca.crt")
	clientCertPath := filepath.Join(tlsDir, "client.crt")
	clientKeyPath := filepath.Join(tlsDir, "client.key")

	connector := NewConnector(cfg.Hub.Address, caCertPath, clientCertPath, clientKeyPath, logger)

	keysDir := filepath.Join(dir, "keys")
	keyPath := filepath.Join(keysDir, privateKeyFile)

	// Fail fast if the configured mount tool is not supported on this OS
	// (e.g. "fuse-t" on Linux). Value validation happened at config load;
	// this adds the platform-specific gating the config layer deliberately omits.
	if err := validateMountTool(cfg.Agent.MountTool, runtime.GOOS); err != nil {
		return nil, fmt.Errorf("validate mount tool: %w", err)
	}

	knownDevicesDir := filepath.Join(dir, common.KnownDevicesDir)
	knownHostsDir := filepath.Join(dir, common.KnownHostsDir)
	mounter := NewMounter(keyPath, knownDevicesDir, knownHostsDir, cfg.Agent.MountTool, logger)

	// Warn (but do not abort) if the mount binary is missing or the FUSE-T
	// runtime is absent while mounts are configured — sharing must still work
	// even without a mount tool installed.
	fileExists := func(path string) bool {
		_, err := os.Stat(path)
		return err == nil
	}
	preflightMountBinary(cfg.Agent.MountTool, resolveBackend(cfg.Agent.MountTool), len(cfg.Mounts) > 0, runtime.GOOS, exec.LookPath, fileExists, logger)

	sshPort := cfg.Agent.SSHPort

	if _, statErr := os.Stat(keyPath); os.IsNotExist(statErr) {
		if _, genErr := GenerateSSHKeyPair(keysDir); genErr != nil {
			logger.Warn("could not pre-generate SSH keys", "error", genErr)
		}
	}

	sshServer, err := NewSSHServer(sshPort, keyPath, logger)
	if err != nil {
		return nil, fmt.Errorf("create SSH server: %w", err)
	}

	// Load the persisted device_id → nickname map before the SSH server starts
	// serving so the DeviceResolver is warm on the very first SFTP request.
	// This is a best-effort load: a missing file is normal (first run, never
	// paired), and any I/O or parse error is logged but does not abort startup.
	cachedNicknames := make(map[string]string)
	if nn, loadErr := LoadNicknames(filepath.Join(dir, common.KnownDevicesDir)); loadErr != nil {
		logger.Warn("could not load persisted nicknames; starting with empty cache", "error", loadErr)
	} else {
		cachedNicknames = nn
	}

	d := &Daemon{
		config:        cfg,
		configPath:    cfgPath,
		identity:      identity,
		connector:     connector,
		mounter:       mounter,
		sshServer:     sshServer,
		logger:        logger,
		onlineDevices: make(map[string]*OnlineDevice),
		nicknames:     cachedNicknames,
		dataDir:       dir,
		sshPort:       sshPort,
		onReady:       opts.OnReady,

		minReconnectInterval: backoffInitial,
		mountMonitorInterval: mountMonitorIntervalFromEnv(
			os.Getenv("HUBFUSE_MOUNT_MONITOR_INTERVAL"),
			defaultMountMonitorInterval, logger),
	}

	// Wire the hub-session seams to the live client. The closures read d.hubClient
	// at call time (it is set later, by connect), so eager assignment here is safe
	// — the codebase idiom (see NewMounter's execCommand/unmount defaults). Unit
	// tests replace these with fakes before driving the session functions.
	d.registerFn = func(ctx context.Context, shares []*pb.Share, sshPort int) (*pb.RegisterResponse, error) {
		return d.hubClient.Register(ctx, shares, sshPort)
	}
	d.subscribeFn = func(ctx context.Context) (pb.HubFuse_SubscribeClient, error) {
		return d.hubClient.Subscribe(ctx)
	}
	d.updateSharesFn = func(ctx context.Context, shares []*pb.Share) error {
		return d.hubClient.UpdateShares(ctx, shares)
	}

	// Install the initial ACL snapshot so pre-existing shares are enforced
	// immediately — the config watcher only fires on later file-change events.
	initialACLs := sharesToACL(cfg.Shares)
	warnInaccessibleShares(logger, initialACLs)
	sshServer.UpdateShares(initialACLs)

	// Daemon satisfies DeviceResolver via its onlineDevices map.
	sshServer.SetDeviceResolver(d)

	return d, nil
}

// fuseTRuntimeMarkers lists the filesystem paths that indicate a FUSE-T
// runtime installation. At least one of these must exist for FUSE-T to work.
var fuseTRuntimeMarkers = []string{
	"/usr/local/lib/libfuse-t.dylib",
	"/opt/homebrew/lib/libfuse-t.dylib",
	"/Library/Application Support/fuse-t",
}

// preflightMountBinary checks that the mount backend's binary is present on
// PATH when at least one mount is configured. On a miss it logs an actionable
// warning and returns — it never aborts startup, because a device with no
// mount tool can still export (share) its own directories. When no mounts are
// configured, the check is skipped entirely (lookPath is not invoked).
//
// Additionally, when tool is "fuse-t" on darwin, it checks for the FUSE-T
// runtime libraries. If none of the known markers exist, it logs a WARN that
// the sshfs on PATH may be macFUSE's drop-in rather than fuse-t-sshfs.
//
// tool is the configured mount-tool value (used only for the message and the
// install hint). It is a pure helper: goos, lookPath, and fileExists are
// injected (runtime.GOOS, exec.LookPath, and an os.Stat wrapper in
// production) so the platform, PATH, and filesystem dependencies can all be
// stubbed in tests without a Daemon instance.
func preflightMountBinary(tool string, backend mountBackend, hasMounts bool, goos string, lookPath func(string) (string, error), fileExists func(string) bool, logger *slog.Logger) {
	if !hasMounts {
		return
	}
	if _, err := lookPath(backend.binary); err != nil {
		logger.Warn(
			fmt.Sprintf("mount-tool %q selected but %q not found on PATH — %s",
				tool, backend.binary, mountInstallHint(tool, goos)),
			"error", err,
		)
	}

	// FUSE-T runtime check: if fuse-t is selected on macOS but none of the
	// known runtime markers are present, the "sshfs" on PATH is likely the
	// macFUSE version and mounts will fail at runtime without an obvious error.
	if tool == "fuse-t" && goos == "darwin" {
		runtimeFound := false
		for _, marker := range fuseTRuntimeMarkers {
			if fileExists(marker) {
				runtimeFound = true
				break
			}
		}
		if !runtimeFound {
			logger.Warn(fmt.Sprintf(
				"mount-tool %q selected but FUSE-T runtime not found; "+
					"the sshfs on PATH may be macFUSE's (checked: %s) — "+
					"install with: brew tap macos-fuse-t/homebrew-cask && brew install --cask fuse-t fuse-t-sshfs",
				tool, strings.Join(fuseTRuntimeMarkers, ", ")),
			)
		}
	}
}

// mountInstallHint returns an OS- and tool-appropriate install suggestion for a
// missing mount binary. FUSE-T is macOS-only and its casks live in a third-party
// tap, so the hint taps it first. When sshfs is selected on macOS we point at
// macFUSE (no fuse-t claim); on Linux we point at the distribution's sshfs
// package.
func mountInstallHint(tool, goos string) string {
	if tool == "fuse-t" {
		return "install with: brew tap macos-fuse-t/homebrew-cask && brew install --cask fuse-t fuse-t-sshfs"
	}
	if goos == "darwin" {
		return "install an sshfs binary (e.g. brew install --cask macfuse, then a macFUSE sshfs build)"
	}
	return "install your distribution's sshfs package (e.g. apt install sshfs)"
}

// NicknameForDeviceID implements DeviceResolver. Used by the SFTP handler to
// match ACL tokens that reference human-readable nicknames.
//
// Resolution is two-tier:
//  1. Live onlineDevices (authoritative when the peer is currently online).
//  2. Persisted nicknames map (covers the restart / pre-online window for
//     already-paired peers — this is the fix for the ACL race in issue #48).
func (d *Daemon) NicknameForDeviceID(id string) (string, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if dev, ok := d.onlineDevices[id]; ok && dev.Nickname != "" {
		return dev.Nickname, true
	}
	if n, ok := d.nicknames[id]; ok && n != "" {
		return n, true
	}
	return "", false
}

// guardConfiguredTargets restricts every configured mount target to guardMode
// so that targets which are not yet mounted (offline peer, unpaired device) do
// not silently absorb local writes. Called early in Run, before registerAndSubscribe,
// to cover the startup window. (#49 guard-target)
func (d *Daemon) guardConfiguredTargets() {
	d.mu.RLock()
	cfg := d.config
	d.mu.RUnlock()

	for _, mc := range cfg.Mounts {
		if err := d.mounter.guardTarget(mc.To); err != nil {
			d.logger.Warn("guard configured mount target", "to", mc.To, "error", err)
		}
	}
}

// rememberNickname records the device_id → nickname mapping in the in-memory
// cache under the lock, then flushes the whole map to disk outside the lock
// (atomic temp+rename).  It is a no-op when nickname is empty or unchanged.
// Disk errors are logged but never fatal — the in-memory cache is the primary
// authority for the running process.
func (d *Daemon) rememberNickname(deviceID, nickname string) {
	if nickname == "" {
		return
	}

	d.mu.Lock()
	if d.nicknames == nil {
		d.nicknames = make(map[string]string)
	}
	if d.nicknames[deviceID] == nickname {
		d.mu.Unlock()
		return
	}
	d.nicknames[deviceID] = nickname
	// Snapshot under the lock so no concurrent update is missed.
	snapshot := make(map[string]string, len(d.nicknames))
	for k, v := range d.nicknames {
		snapshot[k] = v
	}
	d.mu.Unlock()

	// Disk I/O happens outside the lock.
	knownDevicesDir := filepath.Join(d.dataDir, common.KnownDevicesDir)
	if err := SaveNicknames(knownDevicesDir, snapshot); err != nil {
		d.logger.Warn("could not persist nickname", "device_id", deviceID, "error", err)
	}
}

// seedNicknamesFromHub calls ListDevices once after connecting and merges
// the hub-reported nicknames for every device this agent has paired with.
// This covers peers paired by the other side (where our handlePairingCompleted
// received no nickname) and self-heals stale nicknames after a peer is renamed.
// It is best-effort: hub unreachability is logged but not fatal.
func (d *Daemon) seedNicknamesFromHub(ctx context.Context) {
	if d.hubClient == nil {
		return
	}
	resp, err := d.hubClient.ListDevices(ctx)
	if err != nil {
		d.logger.Warn("seedNicknamesFromHub: ListDevices failed; relying on persisted cache", "error", err)
		return
	}

	for _, dev := range resp.Devices {
		if dev.Nickname == "" {
			continue
		}
		if !d.isPaired(dev.DeviceId) {
			continue
		}
		d.rememberNickname(dev.DeviceId, dev.Nickname)
	}
}

// Run is the main daemon loop. It connects to the hub, starts all subsystems,
// and blocks until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	if err := d.connect(ctx); err != nil {
		return err
	}

	// The persisted nickname cache was loaded in NewDaemon (before startSSH),
	// so the DeviceResolver is already warm.  The hub seed below is additive:
	// it merges any updated or newly-paired nicknames after we have a live
	// connection, covering peers whose nickname changed since last run.
	d.seedNicknamesFromHub(ctx)

	if err := d.startSSH(ctx); err != nil {
		return err
	}

	// Load any peer keys that were paired before this run (e.g. the daemon
	// was restarted after previous pairings). Keep this after startSSH so the
	// running SSH service has the persisted authorized keys loaded immediately.
	d.reloadSSHAllowedKeys()

	// Guard all configured mount targets before connecting to the hub so that
	// any target not yet mounted (offline peer, unpaired device) is restricted
	// immediately. (#49 guard-target)
	d.guardConfiguredTargets()

	if err := d.registerAndSubscribe(ctx); err != nil {
		return err
	}

	return d.runServices(ctx)
}

// connect establishes the hub connection with backoff retry.
func (d *Daemon) connect(ctx context.Context) error {
	d.logger.Info("connecting to hub", "addr", d.config.Hub.Address)
	hubClient, err := d.connector.Connect(ctx)
	if err != nil {
		return fmt.Errorf("connect to hub: %w", err)
	}
	d.hubClient = hubClient
	d.logger.Info("connected to hub")
	return nil
}

// startSSH generates SSH keys if absent and starts the embedded SSH server.
func (d *Daemon) startSSH(ctx context.Context) error {
	keysDir := filepath.Join(d.dataDir, "keys")
	keyPath := filepath.Join(keysDir, privateKeyFile)
	if _, statErr := os.Stat(keyPath); os.IsNotExist(statErr) {
		d.logger.Info("generating SSH key pair")
		if _, genErr := GenerateSSHKeyPair(keysDir); genErr != nil {
			return fmt.Errorf("generate SSH keys: %w", genErr)
		}
	}

	go func() {
		if err := d.sshServer.Start(ctx); err != nil {
			d.logger.Error("SSH server stopped", "error", err)
		}
	}()

	return nil
}

// registerAndSubscribe runs the first hub session synchronously (so a hub that
// is down at startup still surfaces as a startup error and onReady timing for
// the PID file is preserved), then hands the live stream to a long-running
// supervisor goroutine that reconnects whenever the session dies. (#61)
func (d *Daemon) registerAndSubscribe(ctx context.Context) error {
	stream, err := d.sessionOnce(ctx)
	if err != nil {
		return err
	}

	go d.supervise(ctx, stream)

	return nil
}

// sessionOnce runs one hub session setup: Register, signal readiness (exactly
// once across the daemon's lifetime), process the initial online-device
// snapshot, then open the event Subscribe stream and return it. The
// Register → processInitialDevices → Subscribe order is preserved so the full
// online-device state comes from the RegisterResponse snapshot rather than being
// reconstructed from events; processInitialDevices runs again on every reconnect,
// which is how a roaming device refreshes its own mounts. A partial success
// (Register ok, Subscribe failed) returns an error and the caller retries the
// whole sessionOnce — a repeat Register is idempotent on the hub.
func (d *Daemon) sessionOnce(ctx context.Context) (pb.HubFuse_SubscribeClient, error) {
	// Snapshot the config pointer under the lock before reading shares: the
	// supervisor re-runs sessionOnce on every reconnect, concurrently with the
	// config watcher's onConfigChange, which swaps d.config under d.mu. Read it the
	// same way every other supervisor-path access does (mountsForOnlineDevice,
	// guardConfiguredTargets) — onConfigChange only ever replaces the pointer, so a
	// snapshotted *Config is immutable. The SSH port comes from the immutable
	// startup value (d.sshPort), never the live config, so a re-register always
	// advertises the port the embedded SSH server is actually listening on. (#61)
	d.mu.RLock()
	cfg := d.config
	d.mu.RUnlock()

	shares := configSharesToProto(cfg.Shares)
	regResp, err := d.registerFn(ctx, shares, d.sshPort)
	if err != nil {
		return nil, fmt.Errorf("register with hub: %w", err)
	}
	d.logger.Info("registered with hub",
		"online_devices", len(regResp.DevicesOnline),
	)

	// onReady is optional (nil in tests and when the cmd layer wants no PID
	// hook), so guard the nil case inside the Once — a bare Do(d.onReady) would
	// panic on a nil func.
	d.readyOnce.Do(func() {
		if d.onReady != nil {
			d.onReady()
		}
	})

	d.processInitialDevices(regResp.DevicesOnline)

	stream, err := d.subscribeFn(ctx)
	if err != nil {
		return nil, fmt.Errorf("subscribe to hub events: %w", err)
	}

	return stream, nil
}

// readStream consumes events from the hub subscription until Recv returns an
// error or ctx is cancelled. A Recv error after ctx cancellation is the normal
// shutdown path (no warning); otherwise the hub session has died and the caller
// (supervise) reconnects.
func (d *Daemon) readStream(ctx context.Context, stream pb.HubFuse_SubscribeClient) {
	for {
		event, err := stream.Recv()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
				d.logger.Warn("event stream error", "error", err)
				return
			}
		}
		d.handleEvent(event)
	}
}

// reconnectSession retries sessionOnce with exponential backoff
// (minReconnectInterval → backoffMax) until it succeeds or ctx is cancelled. It
// returns the live stream on success, or nil when ctx is cancelled (signalling
// supervise to exit). The same hubClient is reused throughout: gRPC repairs the
// transport under the hood, so a fresh dial is unnecessary.
func (d *Daemon) reconnectSession(ctx context.Context) pb.HubFuse_SubscribeClient {
	delay := d.minReconnectInterval
	if delay <= 0 {
		// Floor the FAILURE backoff so a persistent Register failure cannot
		// busy-spin if minReconnectInterval was left at 0 (the ">0" invariant is
		// unenforced; NewDaemon sets backoffInitial, but this guards callers that
		// don't). Only the backoff delay is floored — the success path returns
		// before this is read, and the supervise floor is independent. (#61)
		delay = backoffInitial
	}
	for {
		stream, err := d.sessionOnce(ctx)
		if err == nil {
			d.logger.Info("hub session re-established")
			return stream
		}

		d.logger.Warn("hub session reconnect failed, retrying",
			"error", err,
			"backoff", delay,
		)

		select {
		case <-ctx.Done():
			return nil
		case <-time.After(delay):
		}

		delay *= 2
		if delay > backoffMax {
			delay = backoffMax
		}
	}
}

// supervise runs the hub session for the daemon's lifetime: it reads the event
// stream until the session dies, then reconnects (Register → Subscribe again)
// and resumes. It returns only when ctx is cancelled — either readStream
// observes the cancellation or reconnectSession returns a nil stream. A floor
// (minReconnectInterval) is enforced between successive session starts so a
// session that dies the instant it is established cannot make supervise spin.
func (d *Daemon) supervise(ctx context.Context, stream pb.HubFuse_SubscribeClient) {
	for {
		sessionStart := time.Now()
		d.readStream(ctx, stream)
		if ctx.Err() != nil {
			return
		}
		d.logger.Warn("hub session lost; reconnecting")

		// Floor the reconnect cadence. reconnectSession only backs off when
		// sessionOnce FAILS; a session that re-establishes and then dies almost
		// immediately (flaky proxy, half-open conn, or two daemons sharing one
		// device identity each closing the other's Subscribe channel) would
		// otherwise loop readStream → reconnectSession (instant success) →
		// readStream with zero delay — pegging CPU and hammering the hub. If the
		// session lived less than minReconnectInterval, wait out the remainder so
		// successive session starts are spaced at least that far apart. (#61)
		if dwell := time.Since(sessionStart); dwell < d.minReconnectInterval {
			select {
			case <-ctx.Done():
				return
			case <-time.After(d.minReconnectInterval - dwell):
			}
		}

		stream = d.reconnectSession(ctx)
		if stream == nil {
			return
		}
	}
}

// runServices starts the heartbeat ticker, the mount-health monitor (#67), and
// the config watcher, then blocks until ctx is cancelled before shutting down.
func (d *Daemon) runServices(ctx context.Context) error {
	go d.runHeartbeat(ctx)
	go d.runMountMonitor(ctx)

	watcher, err := agentconfig.NewWatcher(d.configPath, d.onConfigChange)
	if err != nil {
		d.logger.Warn("could not start config watcher", "error", err)
	} else {
		d.watcher = watcher
		go func() {
			if err := watcher.Start(ctx); err != nil {
				d.logger.Warn("config watcher stopped", "error", err)
			}
		}()
	}

	<-ctx.Done()
	d.logger.Info("daemon shutting down")

	return d.Shutdown()
}

// defaultMountMonitorInterval is the default cadence of the mount-health
// monitor. It sits between the hub's 10s heartbeat interval and its 30s
// offline timeout: fast enough that a confirmed-dead mount is usually healed
// within one heartbeat cycle, slow enough that liveness probes (a stat per
// active mount per tick) stay negligible. (#67)
const defaultMountMonitorInterval = 15 * time.Second

// mountMonitorIntervalFromEnv resolves the mount-monitor interval from the raw
// HUBFUSE_MOUNT_MONITOR_INTERVAL env value. An empty value keeps def; a value
// time.ParseDuration accepts is used as-is — including zero or negative, which
// disable the monitor (see runMountMonitor); a malformed value logs a WARN and
// falls back to def so a typo can never silently disable dead-mount healing.
// It is a pure helper so the parsing rules are unit-testable without driving
// NewDaemon (which needs a full on-disk fixture). (#67)
func mountMonitorIntervalFromEnv(raw string, def time.Duration, logger *slog.Logger) time.Duration {
	if raw == "" {
		return def
	}
	interval, err := time.ParseDuration(raw)
	if err != nil {
		logger.Warn("invalid HUBFUSE_MOUNT_MONITOR_INTERVAL; using default",
			"value", raw,
			"default", def,
			"error", err,
		)
		return def
	}
	return interval
}

// runMountMonitor periodically heals confirmed-dead mounts. It exists for the
// event-less windows the Subscribe stream cannot cover: an sshfs process that
// dies while its peer stays registered (same endpoint — the hub never emits
// any event, so ls on the target serves "Transport endpoint is not connected"
// forever), or a DeviceOffline missed because our own event stream was down at
// that moment. Each tick delegates to healDeadMounts synchronously, so a slow
// heal simply delays the next sweep (time.Ticker drops missed ticks) rather
// than piling up concurrent sweeps. Returns immediately when
// mountMonitorInterval <= 0 (monitor disabled); exits when ctx is cancelled.
// (#67 dead-mount healing)
func (d *Daemon) runMountMonitor(ctx context.Context) {
	if d.mountMonitorInterval <= 0 {
		d.logger.Info("mount monitor disabled", "interval", d.mountMonitorInterval)
		return
	}

	ticker := time.NewTicker(d.mountMonitorInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.healDeadMounts(ctx)
		}
	}
}

// healDeadMounts is one monitor sweep: collect the mounts CONFIRMED dead (see
// Mounter.DeadMounts — a probe timeout is NOT confirmation, so a
// wedged-but-possibly-live mount is never touched), then apply the heal policy
// to each dead mount:
//
//   - peer online + mount still configured + share still exported + peer
//     paired → d.mounter.Mount: its same-endpoint dead branch (or the roam
//     branch, if the peer also moved during the event-less window) tears the
//     zombie down and re-attaches atomically under m.mu (#67, #61);
//   - peer offline → d.mounter.UnmountDevice, mirroring handleDeviceOffline
//     (force + reguard) for the DeviceOffline we evidently missed; the next
//     DeviceOnline then mounts cleanly from scratch;
//   - mount no longer in config → the interactive d.mounter.Unmount (its
//     recheck-reap collects the zombie), mirroring onConfigChange's
//     MountsRemoved path;
//   - peer online but share no longer exported, or peer not paired → skip with
//     a WARN: re-mounting would target a dead share (or an unauthorized peer),
//     and unmounting is not this monitor's call — SharesUpdated / config
//     changes own that cleanup (the documented add-only-no-prune tradeoff, see
//     mountsForOnlineDevice).
//
// Heal errors are logged and retried on the next tick — no dedicated backoff
// in v1. The natural retry works because a failed TEARDOWN retains the
// activeMounts entry (unmountKey), so the next sweep re-detects the same dead
// mount. (A teardown that succeeds but whose fresh mount then fails leaves no
// entry behind — recovery for that case comes from the next
// DeviceOnline/reconnect, the same as any other failed mount.)
//
// Lock discipline (mirrors handleDeviceOnline): everything needed from daemon
// state — the config pointer and a DEEP snapshot of onlineDevices — is taken
// under d.mu and the lock is RELEASED before any mounter call. Mount and the
// Unmount* family take m.mu and can block up to mountVerifyTimeout (10s) in
// the verify poll; holding d.mu across a sweep would stall every event handler
// for the whole accumulated budget. The mounter never takes d.mu, so there is
// no lock-order deadlock either way — only latency — but the snapshot keeps a
// heal sweep from freezing the daemon. OnlineDevice values are copied
// (including the Shares slice) rather than aliased: handleSharesUpdated
// mutates info.Shares in place under d.mu, and this sweep reads them after
// unlocking. The snapshotted cfg pointer is immutable — onConfigChange only
// ever swaps the pointer. (#67)
func (d *Daemon) healDeadMounts(ctx context.Context) {
	dead := d.mounter.DeadMounts(ctx)
	if len(dead) == 0 {
		return
	}

	d.mu.RLock()
	cfg := d.config
	peers := make(map[string]OnlineDevice, len(d.onlineDevices))
	for _, dev := range d.onlineDevices {
		cp := *dev
		cp.Shares = append([]string(nil), dev.Shares...)
		peers[dev.Nickname] = cp
	}
	d.mu.RUnlock()

	// UnmountDevice sweeps ALL of a peer's mounts at once, so when an offline
	// peer left several dead mounts behind one call suffices — dedupe to avoid
	// redundant sweeps (and duplicate log noise) within a single tick.
	offlineHandled := make(map[string]bool)

	for _, mnt := range dead {
		if ctx.Err() != nil {
			return // daemon shutting down — abandon the sweep
		}

		peer, online := peers[mnt.Device]
		if !online {
			if offlineHandled[mnt.Device] {
				continue
			}
			offlineHandled[mnt.Device] = true
			d.logger.Info("mount monitor: peer offline, removing its dead mount(s)",
				"device", mnt.Device,
				"share", mnt.Share,
			)
			if err := d.mounter.UnmountDevice(mnt.Device); err != nil {
				d.logger.Warn("mount monitor: unmount dead mounts of offline peer",
					"device", mnt.Device,
					"error", err,
				)
			}
			continue
		}

		mc, configured := mountConfigFor(cfg, mnt.Device, mnt.Share)
		if !configured {
			d.logger.Info("mount monitor: dead mount no longer in config, unmounting",
				"device", mnt.Device,
				"share", mnt.Share,
			)
			if err := d.mounter.Unmount(mnt.Device, mnt.Share); err != nil {
				d.logger.Warn("mount monitor: unmount dead unconfigured mount",
					"device", mnt.Device,
					"share", mnt.Share,
					"error", err,
				)
			}
			continue
		}

		if !slices.Contains(peer.Shares, mnt.Share) {
			d.logger.Warn("mount monitor: peer no longer exports the dead mount's share; skipping heal",
				"device", mnt.Device,
				"share", mnt.Share,
			)
			continue
		}

		if !d.isPaired(peer.DeviceID) {
			d.logger.Warn("mount monitor: peer is not paired; skipping heal of its dead mount",
				"device", mnt.Device,
				"share", mnt.Share,
				"device_id", peer.DeviceID,
			)
			continue
		}

		d.logger.Info("mount monitor: healing dead mount",
			"device", mnt.Device,
			"share", mnt.Share,
			"ip", peer.IP,
			"port", peer.SSHPort,
		)
		if err := d.mounter.Mount(ctx, mc, peer.DeviceID, peer.IP, peer.SSHPort); err != nil {
			d.logger.Error("mount monitor: re-mount of dead mount failed",
				"device", mnt.Device,
				"share", mnt.Share,
				"error", err,
			)
		}
	}
}

// mountConfigFor returns the configured mount entry matching (device, share),
// if any. cfg must be a snapshotted config pointer (immutable once taken).
func mountConfigFor(cfg *agentconfig.Config, device, share string) (agentconfig.MountConfig, bool) {
	for _, mc := range cfg.Mounts {
		if mc.Device == device && mc.Share == share {
			return mc, true
		}
	}
	return agentconfig.MountConfig{}, false
}

// Shutdown unmounts all shares, deregisters from the hub, stops the SSH
// server, stops the config watcher, and closes the hub client.
// UnmountAllForce runs under a 5s timeout so a wedged mount cannot prevent
// clean shutdown — comfortably under the daemonize 10s SIGKILL deadline. (#50 bounded/force)
func (d *Daemon) Shutdown() error {
	var errs []string

	uctx, ucancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ucancel()
	if err := d.mounter.UnmountAllForce(uctx); err != nil {
		errs = append(errs, fmt.Sprintf("unmount all: %v", err))
	}

	if d.hubClient != nil {
		if err := d.hubClient.Deregister(context.Background()); err != nil {
			errs = append(errs, fmt.Sprintf("deregister: %v", err))
		}
	}

	if err := d.sshServer.Stop(); err != nil {
		errs = append(errs, fmt.Sprintf("stop SSH server: %v", err))
	}

	if d.watcher != nil {
		if err := d.watcher.Stop(); err != nil {
			errs = append(errs, fmt.Sprintf("stop config watcher: %v", err))
		}
	}

	if d.hubClient != nil {
		if err := d.hubClient.Close(); err != nil {
			errs = append(errs, fmt.Sprintf("close hub client: %v", err))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("shutdown errors: %v", errs)
	}
	return nil
}

// processInitialDevices handles the list of online devices received on Register.
// The snapshot is AUTHORITATIVE for who is online right now, in both directions:
//
//   - Devices present in the snapshot are added/refreshed in onlineDevices, and
//     each paired one has every configured share it exports (re)mounted.
//     processInitialDevices runs again on every reconnect, so a roaming peer
//     whose endpoint changed has each of its shares re-pointed at the new
//     IP/port by Mount's remount branch. (#61)
//   - Devices ABSENT from the snapshot are pruned from onlineDevices: a peer
//     that went offline while our event stream was dead never delivered its
//     DeviceOffline, and an add/update-only reconciliation would leave the
//     stale entry lying forever — misleading every consumer of the map
//     (healDeadMounts would keep re-mounting a gone peer's dead mounts at its
//     dead endpoint instead of removing them). (#67)
//
// Nothing is unmounted in this path, deliberately. A missing snapshot entry
// proves only that the HUB lost the peer — not that the SSH data path did — so
// tearing down a possibly-live mount here would be exactly the
// touch-unconfirmed-mounts mistake the monitor is designed to avoid. Instead the
// prune feeds the conservative division of labour: once the entry is gone, the
// mount monitor's next sweep sees the peer as offline and removes its mounts
// IF AND ONLY IF their probes confirm them dead (healDeadMounts); a mount that
// is still alive is left untouched. (#67)
func (d *Daemon) processInitialDevices(devices []*pb.DeviceInfo) {
	present := make(map[string]struct{}, len(devices))
	for _, dev := range devices {
		present[dev.DeviceId] = struct{}{}
	}

	d.mu.Lock()
	var pruned []*OnlineDevice
	for id, info := range d.onlineDevices {
		if _, ok := present[id]; !ok {
			pruned = append(pruned, info)
			delete(d.onlineDevices, id)
		}
	}
	d.mu.Unlock()

	// Log outside the lock — no daemon state is read here, and the entries were
	// exclusively ours the moment they left the map.
	for _, info := range pruned {
		d.logger.Info("peer absent from register snapshot, dropping stale online entry",
			"device_id", info.DeviceID,
			"nickname", info.Nickname,
		)
	}

	for _, dev := range devices {
		info := protoToOnlineDevice(dev)

		d.mu.Lock()
		d.onlineDevices[dev.DeviceId] = info
		d.mu.Unlock()

		// Opportunistically persist the freshest nickname so it survives the
		// next restart (closes the online-gap window for this peer).
		d.rememberNickname(dev.DeviceId, dev.Nickname)

		mounts := d.mountsForOnlineDevice(info)
		if len(mounts) == 0 {
			continue
		}
		if !d.isPaired(dev.DeviceId) {
			continue
		}
		// Mount/remount EVERY configured share the peer still exports — a
		// multi-share peer must recover all of its shares on (re)connect, not just
		// the first, but a share it stopped exporting must not be re-mounted. (#61)
		for _, mc := range mounts {
			if err := d.mounter.Mount(context.Background(), mc, info.DeviceID, info.IP, info.SSHPort); err != nil {
				d.logger.Error("auto-mount failed",
					"device", dev.Nickname,
					"share", mc.Share,
					"error", err,
				)
			}
		}
	}
}

// mountsForOnlineDevice returns the configured mounts that should be (re)mounted
// for an online peer: those whose device nickname matches info.Nickname AND whose
// share the peer is currently exporting (info.Shares). Both halves matter:
//
//   - A single peer can export multiple shares — each its own mount entry — so the
//     auto-mount/remount paths must act on ALL of them, not just the first, or a
//     multi-share peer's other shares stay unmounted and, after a roam, stranded
//     on the dead old endpoint.
//   - Intersecting with the peer's exported set (from the Register snapshot or the
//     DeviceOnline event) means a share the peer stopped exporting — a
//     SharesUpdated removal missed while the event stream was down — is never
//     (re)mounted to a now-dead share. (We do not unmount an already-stale mount
//     here; that add-only-no-prune reconnect behaviour is a documented tradeoff.)
//
// The config pointer is snapshotted under the lock (onConfigChange swaps it under
// d.mu). Returns nil when nothing matches. (#61)
func (d *Daemon) mountsForOnlineDevice(info *OnlineDevice) []agentconfig.MountConfig {
	d.mu.RLock()
	cfg := d.config
	d.mu.RUnlock()

	exported := make(map[string]struct{}, len(info.Shares))
	for _, s := range info.Shares {
		exported[s] = struct{}{}
	}

	var mounts []agentconfig.MountConfig
	for _, mc := range cfg.Mounts {
		if mc.Device != info.Nickname {
			continue
		}
		if _, ok := exported[mc.Share]; !ok {
			continue
		}
		mounts = append(mounts, mc)
	}
	return mounts
}

// isPaired reports whether a device is paired by checking for a public key
// file keyed on device_id in the known_devices directory. Returns false for
// any deviceID that fails path-safety validation.
func (d *Daemon) isPaired(deviceID string) bool {
	if err := validateDeviceID(deviceID); err != nil {
		return false
	}
	knownDevicesDir := filepath.Join(d.dataDir, common.KnownDevicesDir)
	_, err := os.Stat(filepath.Join(knownDevicesDir, deviceID+".pub"))
	return err == nil
}

// reloadSSHAllowedKeys reads all *.pub files from the known-devices directory
// and updates the SSH server's allowed-key set. This must be called after a
// new peer key is saved (e.g. from handlePairingCompleted) so that inbound
// SSHFS connections from the newly paired peer are immediately authenticated.
func (d *Daemon) reloadSSHAllowedKeys() {
	knownDevicesDir := filepath.Join(d.dataDir, common.KnownDevicesDir)
	deviceIDs, err := ListPairedDevices(knownDevicesDir)
	if err != nil {
		d.logger.Warn("reload ssh allowed keys: list paired devices", "error", err)
		return
	}

	keys := make(map[string]gossh.PublicKey, len(deviceIDs))
	for _, id := range deviceIDs {
		raw, loadErr := LoadPeerPublicKey(knownDevicesDir, id)
		if loadErr != nil {
			d.logger.Warn("reload ssh allowed keys: load peer key", "device_id", id, "error", loadErr)
			continue
		}
		parsed, _, _, _, parseErr := gossh.ParseAuthorizedKey([]byte(raw))
		if parseErr != nil {
			d.logger.Warn("reload ssh allowed keys: parse peer key", "device_id", id, "error", parseErr)
			continue
		}
		keys[id] = parsed
	}

	d.sshServer.UpdateAllowedKeys(keys)
}

// protoToOnlineDevice converts a proto DeviceInfo to our local OnlineDevice type.
func protoToOnlineDevice(dev *pb.DeviceInfo) *OnlineDevice {
	shares := make([]string, 0, len(dev.Shares))
	for _, s := range dev.Shares {
		shares = append(shares, s.Alias)
	}
	return &OnlineDevice{
		DeviceID: dev.DeviceId,
		Nickname: dev.Nickname,
		IP:       dev.Ip,
		SSHPort:  int(dev.SshPort),
		Shares:   shares,
	}
}
