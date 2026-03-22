package agent

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	agentconfig "github.com/ykhdr/hubfuse/internal/agent/config"
	pb "github.com/ykhdr/hubfuse/proto"
)

// DeviceInfo holds the known state of an online remote device.
type DeviceInfo struct {
	DeviceID string
	Nickname string
	IP       string
	SSHPort  int
	Shares   []string // share aliases
}

// Daemon is the main orchestrator that ties together hub client, mounter,
// SSH server, config watcher, heartbeat, and event handling.
type Daemon struct {
	config       *agentconfig.Config
	configPath   string
	identity     *DeviceIdentity
	hubClient    *HubClient
	connector    *Connector
	mounter      *Mounter
	sshServer    *SSHServer
	watcher      *agentconfig.Watcher
	logger       *slog.Logger
	knownDevices map[string]*DeviceInfo // online devices from hub (device_id -> info)
	mu           sync.RWMutex

	// dataDir is the base data directory (~/.hubfuse by default).
	dataDir string
}

// NewDaemon loads the config and identity, creates the connector, mounter, and
// SSH server, and returns a ready-to-run Daemon.
func NewDaemon(cfgPath string, logger *slog.Logger) (*Daemon, error) {
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

	knownDevicesDir := filepath.Join(dir, "known_devices")

	mounter := NewMounter(keyPath, knownDevicesDir, logger)

	sshPort := cfg.Agent.SSHPort

	// Generate SSH keys if they don't exist yet. We need the key file before
	// creating the SSH server, but the server creation itself will also fail
	// gracefully if the key is missing (it is created in Run if needed).
	if _, statErr := os.Stat(keyPath); os.IsNotExist(statErr) {
		if _, genErr := GenerateSSHKeyPair(keysDir); genErr != nil {
			logger.Warn("could not pre-generate SSH keys", "error", genErr)
		}
	}

	sshServer, err := NewSSHServer(sshPort, keyPath, logger)
	if err != nil {
		return nil, fmt.Errorf("create SSH server: %w", err)
	}

	return &Daemon{
		config:       cfg,
		configPath:   cfgPath,
		identity:     identity,
		connector:    connector,
		mounter:      mounter,
		sshServer:    sshServer,
		logger:       logger,
		knownDevices: make(map[string]*DeviceInfo),
		dataDir:      dir,
	}, nil
}

// Run is the main daemon loop. It connects to the hub, starts all subsystems,
// and blocks until ctx is cancelled.
func (d *Daemon) Run(ctx context.Context) error {
	// 1. Connect to hub with backoff.
	d.logger.Info("connecting to hub", "addr", d.config.Hub.Address)
	hubClient, err := d.connector.Connect(ctx)
	if err != nil {
		return fmt.Errorf("connect to hub: %w", err)
	}
	d.hubClient = hubClient
	d.logger.Info("connected to hub")

	// 2. Generate SSH keys if not present.
	keysDir := filepath.Join(d.dataDir, "keys")
	keyPath := filepath.Join(keysDir, privateKeyFile)
	if _, statErr := os.Stat(keyPath); os.IsNotExist(statErr) {
		d.logger.Info("generating SSH key pair")
		if _, genErr := GenerateSSHKeyPair(keysDir); genErr != nil {
			return fmt.Errorf("generate SSH keys: %w", genErr)
		}
	}

	// 3. Start SSH server in a goroutine.
	go func() {
		if err := d.sshServer.Start(ctx); err != nil {
			d.logger.Error("SSH server stopped", "error", err)
		}
	}()

	// 4. Register with hub and get online devices.
	shares := configSharesToProto(d.config.Shares)
	regResp, err := d.hubClient.Register(ctx, shares, d.config.Agent.SSHPort)
	if err != nil {
		return fmt.Errorf("register with hub: %w", err)
	}
	d.logger.Info("registered with hub",
		"online_devices", len(regResp.DevicesOnline),
	)

	// 5. Process initial online devices.
	d.processInitialDevices(regResp.DevicesOnline)

	// 6. Start Subscribe stream and event processing in a goroutine.
	stream, err := d.hubClient.Subscribe(ctx, d.identity.DeviceID)
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

	// 7. Start heartbeat ticker.
	go d.runHeartbeat(ctx)

	// 8. Start config watcher.
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

	// 9. Wait for context cancellation.
	<-ctx.Done()
	d.logger.Info("daemon shutting down")

	// 10. Shutdown.
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
		info := protoToDeviceInfo(dev)

		d.mu.Lock()
		d.knownDevices[dev.DeviceId] = info
		d.mu.Unlock()

		mc, shouldMount := d.shouldMount(dev.Nickname)
		if !shouldMount {
			continue
		}
		if !d.isPaired(dev.DeviceId) {
			continue
		}
		if err := d.mounter.Mount(context.Background(), mc, info.IP, info.SSHPort); err != nil {
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
// file in the known_devices directory. It checks both by device ID and by
// device nickname (as the mounter uses mc.Device, which is the nickname, for
// its own pairing check).
func (d *Daemon) isPaired(deviceID string) bool {
	knownDevicesDir := filepath.Join(d.dataDir, "known_devices")

	// Primary check: by device ID (set by handlePairingCompleted / SavePeerPublicKey).
	if _, err := os.Stat(filepath.Join(knownDevicesDir, deviceID+".pub")); err == nil {
		return true
	}

	// Secondary check: by device nickname, matching the filename that the
	// Mounter expects when verifying mc.Device.
	d.mu.RLock()
	info, ok := d.knownDevices[deviceID]
	d.mu.RUnlock()
	if ok {
		if _, err := os.Stat(filepath.Join(knownDevicesDir, info.Nickname+".pub")); err == nil {
			return true
		}
	}

	return false
}

// protoToDeviceInfo converts a proto DeviceInfo to our local DeviceInfo type.
func protoToDeviceInfo(dev *pb.DeviceInfo) *DeviceInfo {
	shares := make([]string, 0, len(dev.Shares))
	for _, s := range dev.Shares {
		shares = append(shares, s.Alias)
	}
	return &DeviceInfo{
		DeviceID: dev.DeviceId,
		Nickname: dev.Nickname,
		IP:       dev.Ip,
		SSHPort:  int(dev.SshPort),
		Shares:   shares,
	}
}
