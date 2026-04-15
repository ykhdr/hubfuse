package main

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/ykhdr/hubfuse/internal/agent"
	"github.com/ykhdr/hubfuse/internal/agent/config"
	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/common/daemonize"
)

const (
	defaultDataDir = "~/.hubfuse"
	configFile     = "config.kdl"
	pidFile        = "hubfuse-agent.pid"
	identityFile   = "device.json"
	tlsDir         = "tls"
	keysDir        = "keys"
	privateKeyFile = "id_ed25519"
	publicKeyFile  = "id_ed25519.pub"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hubfuse-agent",
		Short: "HubFuse agent daemon",
	}

	shareCmd := &cobra.Command{
		Use:   "share",
		Short: "Manage shared directories",
	}
	shareCmd.AddCommand(shareAddCmd(), shareRemoveCmd(), shareListCmd())

	mountCmd := &cobra.Command{
		Use:   "mount",
		Short: "Manage remote mounts",
	}
	mountCmd.AddCommand(mountAddCmd(), mountRemoveCmd(), mountListCmd())

	cmd.AddCommand(
		joinCmd(),
		startCmd(),
		stopCmd(),
		statusCmd(),
		pairCmd(),
		devicesCmd(),
		renameCmd(),
		shareCmd,
		mountCmd,
	)
	return cmd
}

// joinCmd implements: hubfuse-agent join <hub-address>
func joinCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "join <hub-address>",
		Short: "Join a hub and register this device",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			hubAddr := args[0]

			// Prompt for nickname.
			fmt.Print("Enter nickname for this device: ")
			reader := bufio.NewReader(os.Stdin)
			nickname, err := reader.ReadString('\n')
			if err != nil {
				return fmt.Errorf("read nickname: %w", err)
			}
			nickname = strings.TrimSpace(nickname)
			if nickname == "" {
				return fmt.Errorf("nickname cannot be empty")
			}

			// Generate device ID.
			deviceID := uuid.New().String()

			// Dial hub insecurely (no client cert yet).
			logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
			hubClient, err := agent.DialInsecure(hubAddr, logger)
			if err != nil {
				return fmt.Errorf("dial hub: %w", err)
			}
			defer hubClient.Close()

			// Call Join.
			resp, err := hubClient.Join(context.Background(), deviceID, nickname)
			if err != nil {
				return fmt.Errorf("join hub: %w", err)
			}
			if !resp.Success {
				return fmt.Errorf("join failed: %s", resp.Error)
			}

			// Save certs to ~/.hubfuse/tls/.
			dataDir := expandHome(defaultDataDir)
			tlsDirPath := filepath.Join(dataDir, tlsDir)
			if err := os.MkdirAll(tlsDirPath, 0700); err != nil {
				return fmt.Errorf("create tls dir: %w", err)
			}

			if err := os.WriteFile(filepath.Join(tlsDirPath, "ca.crt"), resp.CaCert, 0644); err != nil {
				return fmt.Errorf("write ca.crt: %w", err)
			}
			if err := os.WriteFile(filepath.Join(tlsDirPath, "client.crt"), resp.ClientCert, 0644); err != nil {
				return fmt.Errorf("write client.crt: %w", err)
			}
			if err := os.WriteFile(filepath.Join(tlsDirPath, "client.key"), resp.ClientKey, 0600); err != nil {
				return fmt.Errorf("write client.key: %w", err)
			}

			// Save identity.
			identity := &agent.DeviceIdentity{
				DeviceID: deviceID,
				Nickname: nickname,
			}
			identityPath := filepath.Join(dataDir, identityFile)
			if err := agent.SaveIdentity(identityPath, identity); err != nil {
				return fmt.Errorf("save identity: %w", err)
			}

			// Write initial config.kdl.
			cfgPath := filepath.Join(dataDir, configFile)
			cfg := config.DefaultConfig()
			cfg.Device.Nickname = nickname
			cfg.Hub.Address = hubAddr
			if err := writeConfig(cfgPath, cfg); err != nil {
				return fmt.Errorf("write config: %w", err)
			}

			fmt.Printf("joined hub %s as %q (device_id: %s)\n", hubAddr, nickname, deviceID)
			return nil
		},
	}
}

// startCmd implements: hubfuse-agent start
func startCmd() *cobra.Command {
	var (
		logLevel  string
		logOutput string
		daemon    bool
	)

	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the agent daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			dataDir := expandHome(defaultDataDir)
			cfgPath := filepath.Join(dataDir, configFile)
			pidPath := filepath.Join(dataDir, pidFile)
			defaultLog := filepath.Join(dataDir, "agent.log")

			if pid, alive, err := daemonize.CheckRunning(pidPath); err != nil {
				return fmt.Errorf("check existing agent: %w", err)
			} else if alive {
				return fmt.Errorf("agent already running (pid %d)", pid)
			}

			if daemon && !daemonize.IsChild() {
				if err := os.MkdirAll(dataDir, 0o700); err != nil {
					return fmt.Errorf("create data dir: %w", err)
				}
				return daemonize.Spawn(daemonize.SpawnOpts{
					LogPath:     daemonize.ResolveLogOutput(logOutput, true, defaultLog),
					PIDFilePath: pidPath,
				})
			}

			effectiveLog := daemonize.ResolveLogOutput(logOutput, daemon || daemonize.IsChild(), defaultLog)

			logger, err := common.SetupLogger(logLevel, effectiveLog)
			if err != nil {
				return fmt.Errorf("setup logger: %w", err)
			}

			d, err := agent.NewDaemon(cfgPath, logger)
			if err != nil {
				return fmt.Errorf("create daemon: %w", err)
			}
			d.OnReady = func() {
				if err := daemonize.WritePIDFile(pidPath); err != nil {
					logger.Warn("write pid file", "path", pidPath, "error", err)
				}
			}
			defer func() {
				if err := os.Remove(pidPath); err != nil && !os.IsNotExist(err) {
					logger.Warn("remove pid file", "path", pidPath, "error", err)
				}
			}()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

			go func() {
				<-sigCh
				cancel()
			}()

			if err := d.Run(ctx); err != nil {
				return fmt.Errorf("daemon run: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&logLevel, "log-level", "info", "log level (debug, info, warn, error)")
	cmd.Flags().StringVar(&logOutput, "log-output", "stderr", "log output (stderr or file path)")
	cmd.Flags().BoolVarP(&daemon, "daemon", "d", false, "detach from terminal and run in the background")

	return cmd
}

// stopCmd implements: hubfuse-agent stop
func stopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the running agent daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			pidPath := expandHome(filepath.Join(defaultDataDir, pidFile))
			pid, err := readPID(pidPath)
			if err != nil {
				return fmt.Errorf("read PID file %q: %w", pidPath, err)
			}
			proc, err := os.FindProcess(pid)
			if err != nil {
				return fmt.Errorf("find process %d: %w", pid, err)
			}
			if err := proc.Signal(syscall.SIGTERM); err != nil {
				return fmt.Errorf("send SIGTERM to %d: %w", pid, err)
			}
			fmt.Printf("sent SIGTERM to agent (pid %d)\n", pid)
			return nil
		},
	}
}

// statusCmd implements: hubfuse-agent status
func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show agent daemon status",
		RunE: func(cmd *cobra.Command, args []string) error {
			pidPath := expandHome(filepath.Join(defaultDataDir, pidFile))
			pid, err := readPID(pidPath)
			if err != nil {
				fmt.Println("agent is not running (no PID file)")
				return nil
			}
			proc, err := os.FindProcess(pid)
			if err != nil {
				fmt.Printf("agent is not running (pid %d not found)\n", pid)
				return nil
			}
			if err := proc.Signal(syscall.Signal(0)); err != nil {
				fmt.Printf("agent is not running (pid %d, signal error: %v)\n", pid, err)
				return nil
			}
			fmt.Printf("agent is running (pid %d)\n", pid)
			return nil
		},
	}
}

// pairCmd implements: hubfuse-agent pair <device>
func pairCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pair <device>",
		Short: "Request pairing with a remote device",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			toDevice := args[0]

			dataDir := expandHome(defaultDataDir)
			keysDirPath := filepath.Join(dataDir, keysDir)
			pubKeyPath := filepath.Join(keysDirPath, publicKeyFile)

			// Load or generate SSH key pair.
			var publicKey string
			if _, err := os.Stat(pubKeyPath); os.IsNotExist(err) {
				pk, err := agent.GenerateSSHKeyPair(keysDirPath)
				if err != nil {
					return fmt.Errorf("generate SSH key pair: %w", err)
				}
				publicKey = pk
			} else {
				pk, err := agent.LoadPublicKey(pubKeyPath)
				if err != nil {
					return fmt.Errorf("load public key: %w", err)
				}
				publicKey = pk
			}

			logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
			hubClient, _, err := dialHub(dataDir, logger)
			if err != nil {
				return fmt.Errorf("connect to hub: %w", err)
			}
			defer hubClient.Close()

			inviteCode, err := hubClient.RequestPairing(context.Background(), toDevice, publicKey)
			if err != nil {
				return fmt.Errorf("request pairing: %w", err)
			}

			fmt.Printf("pairing invite code: %s\n", inviteCode)
			fmt.Printf("share this code with %q to complete pairing\n", toDevice)
			return nil
		},
	}
}

// devicesCmd implements: hubfuse-agent devices
func devicesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "devices",
		Short: "List online devices",
		RunE: func(cmd *cobra.Command, args []string) error {
			dataDir := expandHome(defaultDataDir)
			logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

			hubClient, _, err := dialHub(dataDir, logger)
			if err != nil {
				return fmt.Errorf("connect to hub: %w", err)
			}
			defer hubClient.Close()

			resp, err := hubClient.Register(context.Background(), nil, 0)
			if err != nil {
				return fmt.Errorf("get device list: %w", err)
			}

			if len(resp.DevicesOnline) == 0 {
				fmt.Println("no devices online")
				return nil
			}

			fmt.Printf("%-40s  %-20s  %s\n", "DEVICE ID", "NICKNAME", "IP")
			fmt.Printf("%-40s  %-20s  %s\n", strings.Repeat("-", 40), strings.Repeat("-", 20), strings.Repeat("-", 15))
			for _, d := range resp.DevicesOnline {
				fmt.Printf("%-40s  %-20s  %s\n", d.DeviceId, d.Nickname, d.Ip)
			}
			return nil
		},
	}
}

// renameCmd implements: hubfuse-agent rename <new-nickname>
func renameCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rename <new-nickname>",
		Short: "Rename this device",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			newNickname := args[0]

			dataDir := expandHome(defaultDataDir)
			logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

			hubClient, identity, err := dialHub(dataDir, logger)
			if err != nil {
				return fmt.Errorf("connect to hub: %w", err)
			}
			defer hubClient.Close()

			if _, err := hubClient.Rename(context.Background(), newNickname); err != nil {
				return fmt.Errorf("rename: %w", err)
			}

			// Update local identity.
			identity.Nickname = newNickname
			identityPath := filepath.Join(dataDir, identityFile)
			if err := agent.SaveIdentity(identityPath, identity); err != nil {
				return fmt.Errorf("save identity: %w", err)
			}

			// Update config.kdl.
			cfgPath := filepath.Join(dataDir, configFile)
			cfg, err := loadConfig(cfgPath)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			cfg.Device.Nickname = newNickname
			if err := writeConfig(cfgPath, cfg); err != nil {
				return fmt.Errorf("write config: %w", err)
			}

			fmt.Printf("renamed device to %q\n", newNickname)
			return nil
		},
	}
}

// shareAddCmd implements: hubfuse-agent share add <path> --alias <name> --permissions <ro|rw> --allow <devices>
func shareAddCmd() *cobra.Command {
	var (
		alias       string
		permissions string
		allow       []string
	)

	cmd := &cobra.Command{
		Use:   "add <path>",
		Short: "Add a shared directory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sharePath := args[0]

			dataDir := expandHome(defaultDataDir)
			cfgPath := filepath.Join(dataDir, configFile)

			cfg, err := loadConfig(cfgPath)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			// Check for duplicate alias.
			for _, s := range cfg.Shares {
				if s.Alias == alias {
					return fmt.Errorf("share with alias %q already exists", alias)
				}
			}

			perm := config.NormalizePermissions(permissions)
			if perm == "" {
				perm = "ro"
			}

			cfg.Shares = append(cfg.Shares, config.ShareConfig{
				Path:           sharePath,
				Alias:          alias,
				Permissions:    perm,
				AllowedDevices: allow,
			})

			if err := writeConfig(cfgPath, cfg); err != nil {
				return fmt.Errorf("write config: %w", err)
			}

			fmt.Printf("added share %q -> %s\n", alias, sharePath)
			return nil
		},
	}

	cmd.Flags().StringVar(&alias, "alias", "", "alias name for the share (required)")
	cmd.Flags().StringVar(&permissions, "permissions", "ro", "permissions: ro or rw")
	cmd.Flags().StringSliceVar(&allow, "allow", nil, "allowed device nicknames")
	_ = cmd.MarkFlagRequired("alias")

	return cmd
}

// shareRemoveCmd implements: hubfuse-agent share remove <alias>
func shareRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <alias>",
		Short: "Remove a shared directory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			alias := args[0]

			dataDir := expandHome(defaultDataDir)
			cfgPath := filepath.Join(dataDir, configFile)

			cfg, err := loadConfig(cfgPath)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			found := false
			newShares := cfg.Shares[:0]
			for _, s := range cfg.Shares {
				if s.Alias == alias {
					found = true
					continue
				}
				newShares = append(newShares, s)
			}
			if !found {
				return fmt.Errorf("share %q not found", alias)
			}
			cfg.Shares = newShares

			if err := writeConfig(cfgPath, cfg); err != nil {
				return fmt.Errorf("write config: %w", err)
			}

			fmt.Printf("removed share %q\n", alias)
			return nil
		},
	}
}

// shareListCmd implements: hubfuse-agent share list
func shareListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List shared directories",
		RunE: func(cmd *cobra.Command, args []string) error {
			dataDir := expandHome(defaultDataDir)
			cfgPath := filepath.Join(dataDir, configFile)

			cfg, err := loadConfig(cfgPath)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			if len(cfg.Shares) == 0 {
				fmt.Println("no shares configured")
				return nil
			}

			fmt.Printf("%-20s  %-4s  %-30s  %s\n", "ALIAS", "PERM", "PATH", "ALLOWED")
			fmt.Printf("%-20s  %-4s  %-30s  %s\n", strings.Repeat("-", 20), "----", strings.Repeat("-", 30), strings.Repeat("-", 20))
			for _, s := range cfg.Shares {
				allowed := strings.Join(s.AllowedDevices, ",")
				if allowed == "" {
					allowed = "none"
				}
				fmt.Printf("%-20s  %-4s  %-30s  %s\n", s.Alias, s.Permissions, s.Path, allowed)
			}
			return nil
		},
	}
}

// mountAddCmd implements: hubfuse-agent mount add <device>:<share> --to <local-path>
func mountAddCmd() *cobra.Command {
	var to string

	cmd := &cobra.Command{
		Use:   "add <device>:<share>",
		Short: "Add a remote mount",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			parts := strings.SplitN(args[0], ":", 2)
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				return fmt.Errorf("argument must be in <device>:<share> format")
			}
			deviceName := parts[0]
			shareName := parts[1]

			dataDir := expandHome(defaultDataDir)
			cfgPath := filepath.Join(dataDir, configFile)

			cfg, err := loadConfig(cfgPath)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			// Check for duplicate.
			for _, m := range cfg.Mounts {
				if m.Device == deviceName && m.Share == shareName {
					return fmt.Errorf("mount %s:%s already exists", deviceName, shareName)
				}
			}

			cfg.Mounts = append(cfg.Mounts, config.MountConfig{
				Device: deviceName,
				Share:  shareName,
				To:     to,
			})

			if err := writeConfig(cfgPath, cfg); err != nil {
				return fmt.Errorf("write config: %w", err)
			}

			fmt.Printf("added mount %s:%s -> %s\n", deviceName, shareName, to)
			return nil
		},
	}

	cmd.Flags().StringVar(&to, "to", "", "local mount point path (required)")
	_ = cmd.MarkFlagRequired("to")

	return cmd
}

// mountRemoveCmd implements: hubfuse-agent mount remove <device>:<share>
func mountRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <device>:<share>",
		Short: "Remove a remote mount",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			parts := strings.SplitN(args[0], ":", 2)
			if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
				return fmt.Errorf("argument must be in <device>:<share> format")
			}
			deviceName := parts[0]
			shareName := parts[1]

			dataDir := expandHome(defaultDataDir)
			cfgPath := filepath.Join(dataDir, configFile)

			cfg, err := loadConfig(cfgPath)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			found := false
			newMounts := cfg.Mounts[:0]
			for _, m := range cfg.Mounts {
				if m.Device == deviceName && m.Share == shareName {
					found = true
					continue
				}
				newMounts = append(newMounts, m)
			}
			if !found {
				return fmt.Errorf("mount %s:%s not found", deviceName, shareName)
			}
			cfg.Mounts = newMounts

			if err := writeConfig(cfgPath, cfg); err != nil {
				return fmt.Errorf("write config: %w", err)
			}

			fmt.Printf("removed mount %s:%s\n", deviceName, shareName)
			return nil
		},
	}
}

// mountListCmd implements: hubfuse-agent mount list
func mountListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List remote mounts",
		RunE: func(cmd *cobra.Command, args []string) error {
			dataDir := expandHome(defaultDataDir)
			cfgPath := filepath.Join(dataDir, configFile)

			cfg, err := loadConfig(cfgPath)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}

			if len(cfg.Mounts) == 0 {
				fmt.Println("no mounts configured")
				return nil
			}

			fmt.Printf("%-20s  %-20s  %s\n", "DEVICE", "SHARE", "LOCAL PATH")
			fmt.Printf("%-20s  %-20s  %s\n", strings.Repeat("-", 20), strings.Repeat("-", 20), strings.Repeat("-", 30))
			for _, m := range cfg.Mounts {
				fmt.Printf("%-20s  %-20s  %s\n", m.Device, m.Share, m.To)
			}
			return nil
		},
	}
}

// expandHome replaces a leading "~" with the user's home directory.
func expandHome(path string) string {
	if len(path) == 0 || path[0] != '~' {
		return path
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	return filepath.Join(home, path[1:])
}

// dialHub loads the device identity and connects to the hub with mTLS.
func dialHub(dataDir string, logger *slog.Logger) (*agent.HubClient, *agent.DeviceIdentity, error) {
	identityPath := filepath.Join(dataDir, identityFile)
	identity, err := agent.LoadIdentity(identityPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load identity: %w", err)
	}

	tlsDirPath := filepath.Join(dataDir, tlsDir)
	caCertPath := filepath.Join(tlsDirPath, "ca.crt")
	clientCertPath := filepath.Join(tlsDirPath, "client.crt")
	clientKeyPath := filepath.Join(tlsDirPath, "client.key")

	cfgPath := filepath.Join(dataDir, configFile)
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return nil, nil, fmt.Errorf("load config: %w", err)
	}

	hubClient, err := agent.DialWithMTLS(cfg.Hub.Address, caCertPath, clientCertPath, clientKeyPath, logger)
	if err != nil {
		return nil, nil, fmt.Errorf("dial hub: %w", err)
	}

	_ = common.ProtocolVersion // suppress unused import warning
	return hubClient, identity, nil
}

// loadConfig loads the config from path, returning a default config if the
// file does not exist.
func loadConfig(path string) (*config.Config, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return config.DefaultConfig(), nil
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, fmt.Errorf("load config %q: %w", path, err)
	}
	return cfg, nil
}

// writeConfig serialises cfg to a KDL file at path, creating parent
// directories as needed. The format matches what config.Load expects.
func writeConfig(path string, cfg *config.Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	var sb strings.Builder

	// device block.
	fmt.Fprintf(&sb, "device {\n")
	fmt.Fprintf(&sb, "    nickname %q\n", cfg.Device.Nickname)
	fmt.Fprintf(&sb, "}\n\n")

	// hub block.
	fmt.Fprintf(&sb, "hub {\n")
	fmt.Fprintf(&sb, "    address %q\n", cfg.Hub.Address)
	fmt.Fprintf(&sb, "}\n\n")

	// agent block.
	fmt.Fprintf(&sb, "agent {\n")
	fmt.Fprintf(&sb, "    ssh-port %d\n", cfg.Agent.SSHPort)
	fmt.Fprintf(&sb, "}\n\n")

	// shares block.
	if len(cfg.Shares) > 0 {
		fmt.Fprintf(&sb, "shares {\n")
		for _, s := range cfg.Shares {
			fmt.Fprintf(&sb, "    share %q alias=%q permissions=%q", s.Path, s.Alias, s.Permissions)
			if len(s.AllowedDevices) > 0 {
				fmt.Fprintf(&sb, " {\n")
				fmt.Fprintf(&sb, "        allowed-devices")
				for _, d := range s.AllowedDevices {
					fmt.Fprintf(&sb, " %q", d)
				}
				fmt.Fprintf(&sb, "\n")
				fmt.Fprintf(&sb, "    }\n")
			} else {
				fmt.Fprintf(&sb, "\n")
			}
		}
		fmt.Fprintf(&sb, "}\n\n")
	}

	// mounts block.
	if len(cfg.Mounts) > 0 {
		fmt.Fprintf(&sb, "mounts {\n")
		for _, m := range cfg.Mounts {
			fmt.Fprintf(&sb, "    mount device=%q share=%q to=%q\n", m.Device, m.Share, m.To)
		}
		fmt.Fprintf(&sb, "}\n")
	}

	if err := os.WriteFile(path, []byte(sb.String()), 0644); err != nil {
		return fmt.Errorf("write config %q: %w", path, err)
	}
	return nil
}

// readPID reads an integer PID from the file at path.
func readPID(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0, fmt.Errorf("parse PID: %w", err)
	}
	return pid, nil
}
