package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

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
	mu            sync.RWMutex

	// dataDir is the base data directory (~/.hubfuse by default).
	dataDir string

	onReady func()
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

	d := &Daemon{
		config:        cfg,
		configPath:    cfgPath,
		identity:      identity,
		connector:     connector,
		mounter:       mounter,
		sshServer:     sshServer,
		logger:        logger,
		onlineDevices: make(map[string]*OnlineDevice),
		dataDir:       dir,
		onReady:       opts.OnReady,
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
func (d *Daemon) NicknameForDeviceID(id string) (string, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	if dev, ok := d.onlineDevices[id]; ok && dev.Nickname != "" {
		return dev.Nickname, true
	}
	return "", false
}

// Run is the main daemon loop. It connects to the hub, starts all subsystems,
// and blocks until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	if err := d.connect(ctx); err != nil {
		return err
	}

	if err := d.startSSH(ctx); err != nil {
		return err
	}

	// Load any peer keys that were paired before this run (e.g. the daemon
	// was restarted after previous pairings). Keep this after startSSH so the
	// running SSH service has the persisted authorized keys loaded immediately.
	d.reloadSSHAllowedKeys()

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

// registerAndSubscribe registers with the hub, signals readiness, processes
// the initial device list, and starts the event stream goroutine.
func (d *Daemon) registerAndSubscribe(ctx context.Context) error {
	shares := configSharesToProto(d.config.Shares)
	regResp, err := d.hubClient.Register(ctx, shares, d.config.Agent.SSHPort)
	if err != nil {
		return fmt.Errorf("register with hub: %w", err)
	}
	d.logger.Info("registered with hub",
		"online_devices", len(regResp.DevicesOnline),
	)

	if d.onReady != nil {
		d.onReady()
	}

	d.processInitialDevices(regResp.DevicesOnline)

	stream, err := d.hubClient.Subscribe(ctx)
	if err != nil {
		return fmt.Errorf("subscribe to hub events: %w", err)
	}

	go func() {
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
	}()

	return nil
}

// runServices starts the heartbeat ticker and config watcher, then blocks
// until ctx is cancelled before shutting down.
func (d *Daemon) runServices(ctx context.Context) error {
	go d.runHeartbeat(ctx)

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

// Shutdown unmounts all shares, deregisters from the hub, stops the SSH
// server, stops the config watcher, and closes the hub client.
func (d *Daemon) Shutdown() error {
	var errs []string

	if err := d.mounter.UnmountAll(); err != nil {
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
// For each device that is paired and has a mount configured, it auto-mounts.
func (d *Daemon) processInitialDevices(devices []*pb.DeviceInfo) {
	for _, dev := range devices {
		info := protoToOnlineDevice(dev)

		d.mu.Lock()
		d.onlineDevices[dev.DeviceId] = info
		d.mu.Unlock()

		mc, shouldMount := d.shouldMount(dev.Nickname)
		if !shouldMount {
			continue
		}
		if !d.isPaired(dev.DeviceId) {
			continue
		}
		if err := d.mounter.Mount(context.Background(), mc, info.DeviceID, info.IP, info.SSHPort); err != nil {
			d.logger.Error("auto-mount failed",
				"device", dev.Nickname,
				"share", mc.Share,
				"error", err,
			)
		}
	}
}

// shouldMount checks whether the config has a mount entry for a device with the
// given nickname. Returns the MountConfig and true if found.
func (d *Daemon) shouldMount(deviceNickname string) (agentconfig.MountConfig, bool) {
	d.mu.RLock()
	cfg := d.config
	d.mu.RUnlock()

	for _, mc := range cfg.Mounts {
		if mc.Device == deviceNickname {
			return mc, true
		}
	}
	return agentconfig.MountConfig{}, false
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
