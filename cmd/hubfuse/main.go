package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/ykhdr/hubfuse/cmd/internal/clierrors"
	"github.com/ykhdr/hubfuse/internal/agent"
	"github.com/ykhdr/hubfuse/internal/agent/config"
	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/common/daemonize"
	pb "github.com/ykhdr/hubfuse/proto"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, clierrors.Format(err, nil))
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hubfuse",
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
	silenceAll(cmd)
	return cmd
}

// joinCmd implements: hubfuse join <hub-address>
func joinCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "join <hub-address>",
		Short: "Join a hub and register this device",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			hubAddr := args[0]

			deviceID := agent.GenerateDeviceID()
			ctx := &clierrors.Context{HubAddr: hubAddr}

			// Dial hub insecurely (no client cert yet).
			logger := slog.New(common.NewConsoleHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
			hubClient, err := agent.DialInsecure(hubAddr, logger)
			if err != nil {
				return clierrors.Wrap(fmt.Errorf("dial hub: %w", err), ctx)
			}
			defer hubClient.Close()

			reader := bufio.NewReader(os.Stdin)
			var (
				nickname string
				resp     *pb.JoinResponse
			)
			printNicknameTaken := func(err error) {
				ctx.Nickname = nickname
				fmt.Fprintln(os.Stderr, clierrors.Format(err, ctx))
			}

			for {
				n, err := promptNickname(reader)
				if err != nil {
					return err
				}
				nickname = n
				ctx.Nickname = nickname
				if nickname == "" {
					fmt.Fprintln(os.Stderr, "error: nickname cannot be empty")
					continue
				}

				// Call Join.
				resp, err = hubClient.Join(context.Background(), deviceID, nickname)
				if err != nil {
					if clierrors.IsNicknameTaken(err) {
						printNicknameTaken(err)
						continue
					}
					return clierrors.Wrap(fmt.Errorf("join hub: %w", err), ctx)
				}
				if !resp.Success {
					respErr := errors.New(resp.Error)
					if clierrors.IsNicknameTaken(respErr) {
						printNicknameTaken(respErr)
						continue
					}
					return clierrors.Wrap(fmt.Errorf("join failed: %w", respErr), ctx)
				}

				break
			}

			// Save certs to ~/.hubfuse/tls/.
			dataDir := common.ExpandHome(common.AgentDataDir)
			tlsDirPath := filepath.Join(dataDir, common.TLSDir)
			if err := os.MkdirAll(tlsDirPath, 0700); err != nil {
				return fmt.Errorf("create tls dir: %w", err)
			}

			if err := os.WriteFile(filepath.Join(tlsDirPath, common.CACertFile), resp.CaCert, 0644); err != nil {
				return fmt.Errorf("write ca.crt: %w", err)
			}
			if err := os.WriteFile(filepath.Join(tlsDirPath, common.ClientCertFile), resp.ClientCert, 0644); err != nil {
				return fmt.Errorf("write client.crt: %w", err)
			}
			if err := os.WriteFile(filepath.Join(tlsDirPath, common.ClientKeyFile), resp.ClientKey, 0600); err != nil {
				return fmt.Errorf("write client.key: %w", err)
			}

			// Save identity.
			identity := &agent.DeviceIdentity{
				DeviceID: deviceID,
				Nickname: nickname,
			}
			identityPath := filepath.Join(dataDir, common.IdentityFile)
			if err := agent.SaveIdentity(identityPath, identity); err != nil {
				return fmt.Errorf("save identity: %w", err)
			}

			cfgPath := filepath.Join(dataDir, common.ConfigFile)
			cfg := config.DefaultConfig()
			cfg.Device.Nickname = nickname
			cfg.Hub.Address = hubAddr
			if err := config.Save(cfgPath, cfg); err != nil {
				return fmt.Errorf("write config: %w", err)
			}

			fmt.Printf("joined hub %s as %q (device_id: %s)\n", hubAddr, nickname, deviceID)
			fmt.Println("run `hubfuse start` to connect this device and appear online")
			return nil
		},
	}
}

// startCmd implements: hubfuse start
func startCmd() *cobra.Command {
	var (
		logFile  string
		logLevel string
		verbose  bool
		daemon   bool
	)

	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the agent daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			dataDir := common.ExpandHome(common.AgentDataDir)
			cfgPath := filepath.Join(dataDir, common.ConfigFile)
			pidPath := filepath.Join(dataDir, common.AgentPIDFile)
			defaultLog := filepath.Join(dataDir, common.AgentLogFile)

			// Reject second concurrent start regardless of daemon flag.
			if pid, alive, err := daemonize.CheckRunning(pidPath); err != nil {
				return fmt.Errorf("check existing agent: %w", err)
			} else if alive {
				return fmt.Errorf("agent already running (pid %d)", pid)
			}

			// If we're the parent and --daemon was requested, re-exec.
			// The detached child's stdout/stderr (which is where the
			// console-handler logs land) gets redirected into defaultLog.
			if daemon && !daemonize.IsChild() {
				if err := os.MkdirAll(dataDir, 0o700); err != nil {
					return fmt.Errorf("create data dir: %w", err)
				}
				return daemonize.Spawn(daemonize.SpawnOpts{
					LogPath:     defaultLog,
					PIDFilePath: pidPath,
				})
			}

			logger, err := common.SetupLogger(common.LoggerOptions{
				LogFile:   logFile,
				FileLevel: common.ParseLogLevel(logLevel),
				Verbose:   verbose,
			})
			if err != nil {
				return fmt.Errorf("setup logger: %w", err)
			}

			d, err := agent.NewDaemon(cfgPath, logger, agent.DaemonOptions{
				OnReady: func() {
					if err := daemonize.WritePIDFile(pidPath); err != nil {
						logger.Warn("write pid file", "path", pidPath, "error", err)
					}
				},
			})
			if err != nil {
				return fmt.Errorf("create daemon: %w", err)
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

	cmd.Flags().StringVar(&logFile, "log-file", "", "write JSON logs to file (disabled by default)")
	cmd.Flags().StringVar(&logLevel, "log-level", "debug", "log file level (debug, info, warn, error)")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "show debug logs in console")
	cmd.Flags().BoolVarP(&daemon, "daemon", "d", false, "detach from terminal and run in the background")

	return cmd
}

// stopCmd implements: hubfuse stop
func stopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the running agent daemon",
		RunE: func(cmd *cobra.Command, args []string) error {
			pidPath := common.ExpandHome(filepath.Join(common.AgentDataDir, common.AgentPIDFile))
			return daemonize.SignalStop(pidPath, "agent")
		},
	}
}

// statusCmd implements: hubfuse status
func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show agent daemon status",
		RunE: func(cmd *cobra.Command, args []string) error {
			pidPath := common.ExpandHome(filepath.Join(common.AgentDataDir, common.AgentPIDFile))
			return daemonize.ReportStatus(pidPath, "agent")
		},
	}
}

// pairCmd implements: hubfuse pair <device>
func pairCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pair <device>",
		Short: "Request pairing with a remote device",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			toDevice := args[0]

			dataDir := common.ExpandHome(common.AgentDataDir)
			keysDirPath := filepath.Join(dataDir, common.KeysDir)
			pubKeyPath := filepath.Join(keysDirPath, common.PublicKeyFile)

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

			logger := slog.New(common.NewConsoleHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
			hubClient, _, hubAddr, err := dialHub(dataDir, logger)
			if err != nil {
				return clierrors.Wrap(fmt.Errorf("connect to hub: %w", err), &clierrors.Context{HubAddr: hubAddr})
			}
			defer hubClient.Close()

			ctx := &clierrors.Context{Nickname: toDevice, HubAddr: hubAddr}
			inviteCode, err := hubClient.RequestPairing(context.Background(), toDevice, publicKey)
			if err != nil {
				return clierrors.Wrap(fmt.Errorf("request pairing: %w", err), ctx)
			}

			fmt.Printf("pairing invite code: %s\n", inviteCode)
			fmt.Printf("share this code with %q to complete pairing\n", toDevice)
			return nil
		},
	}
}

// devicesCmd implements: hubfuse devices
func devicesCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "devices",
		Short: "List all devices",
		RunE: func(cmd *cobra.Command, args []string) error {
			dataDir := common.ExpandHome(common.AgentDataDir)
			logger := slog.New(common.NewConsoleHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

			hubClient, _, hubAddr, err := dialHub(dataDir, logger)
			if err != nil {
				return clierrors.Wrap(fmt.Errorf("connect to hub: %w", err), &clierrors.Context{HubAddr: hubAddr})
			}
			defer hubClient.Close()

			resp, err := hubClient.ListDevices(context.Background())
			if err != nil {
				return clierrors.Wrap(fmt.Errorf("list devices: %w", err), &clierrors.Context{HubAddr: hubAddr})
			}

			if len(resp.Devices) == 0 {
				fmt.Println("no devices registered")
				return nil
			}

			fmt.Printf("%-40s  %-20s  %-10s  %s\n", "DEVICE ID", "NICKNAME", "STATUS", "IP")
			fmt.Printf("%-40s  %-20s  %-10s  %s\n",
				strings.Repeat("-", 40), strings.Repeat("-", 20),
				strings.Repeat("-", 10), strings.Repeat("-", 15))
			for _, d := range resp.Devices {
				fmt.Printf("%-40s  %-20s  %-10s  %s\n", d.DeviceId, d.Nickname, d.Status, d.Ip)
			}
			return nil
		},
	}
}

// renameCmd implements: hubfuse rename <new-nickname>
func renameCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "rename <new-nickname>",
		Short: "Rename this device",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			newNickname := args[0]

			dataDir := common.ExpandHome(common.AgentDataDir)
			logger := slog.New(common.NewConsoleHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

			hubClient, identity, hubAddr, err := dialHub(dataDir, logger)
			if err != nil {
				return clierrors.Wrap(fmt.Errorf("connect to hub: %w", err), &clierrors.Context{HubAddr: hubAddr})
			}
			defer hubClient.Close()

			ctx := &clierrors.Context{Nickname: newNickname, HubAddr: hubAddr}
			resp, err := hubClient.Rename(context.Background(), newNickname)
			if err != nil {
				return clierrors.Wrap(fmt.Errorf("rename: %w", err), ctx)
			}
			if !resp.Success {
				return clierrors.Wrap(errors.New(resp.Error), ctx)
			}

			// Update local identity.
			identity.Nickname = newNickname
			identityPath := filepath.Join(dataDir, common.IdentityFile)
			if err := agent.SaveIdentity(identityPath, identity); err != nil {
				return fmt.Errorf("save identity: %w", err)
			}

			// Update config.kdl.
			cfgPath := filepath.Join(dataDir, common.ConfigFile)
			cfg, err := loadConfig(cfgPath)
			if err != nil {
				return fmt.Errorf("load config: %w", err)
			}
			cfg.Device.Nickname = newNickname
			if err := config.Save(cfgPath, cfg); err != nil {
				return fmt.Errorf("write config: %w", err)
			}

			fmt.Printf("renamed device to %q\n", newNickname)
			return nil
		},
	}
}

// shareAddCmd implements: hubfuse share add <path> --alias <name> --permissions <ro|rw> --allow <devices>
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

			dataDir := common.ExpandHome(common.AgentDataDir)
			cfgPath := filepath.Join(dataDir, common.ConfigFile)

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

			if err := config.Save(cfgPath, cfg); err != nil {
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

// shareRemoveCmd implements: hubfuse share remove <alias>
func shareRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "remove <alias>",
		Short: "Remove a shared directory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			alias := args[0]

			dataDir := common.ExpandHome(common.AgentDataDir)
			cfgPath := filepath.Join(dataDir, common.ConfigFile)

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

			if err := config.Save(cfgPath, cfg); err != nil {
				return fmt.Errorf("write config: %w", err)
			}

			fmt.Printf("removed share %q\n", alias)
			return nil
		},
	}
}

// shareListCmd implements: hubfuse share list
func shareListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List shared directories",
		RunE: func(cmd *cobra.Command, args []string) error {
			dataDir := common.ExpandHome(common.AgentDataDir)
			cfgPath := filepath.Join(dataDir, common.ConfigFile)

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

// mountAddCmd implements: hubfuse mount add <device>:<share> --to <local-path>
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

			dataDir := common.ExpandHome(common.AgentDataDir)
			cfgPath := filepath.Join(dataDir, common.ConfigFile)

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

			if err := config.Save(cfgPath, cfg); err != nil {
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

// mountRemoveCmd implements: hubfuse mount remove <device>:<share>
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

			dataDir := common.ExpandHome(common.AgentDataDir)
			cfgPath := filepath.Join(dataDir, common.ConfigFile)

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

			if err := config.Save(cfgPath, cfg); err != nil {
				return fmt.Errorf("write config: %w", err)
			}

			fmt.Printf("removed mount %s:%s\n", deviceName, shareName)
			return nil
		},
	}
}

// mountListCmd implements: hubfuse mount list
func mountListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List remote mounts",
		RunE: func(cmd *cobra.Command, args []string) error {
			dataDir := common.ExpandHome(common.AgentDataDir)
			cfgPath := filepath.Join(dataDir, common.ConfigFile)

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

func silenceAll(cmd *cobra.Command) {
	cmd.SilenceUsage = true
	cmd.SilenceErrors = true
	for _, child := range cmd.Commands() {
		silenceAll(child)
	}
}

func promptNickname(reader *bufio.Reader) (string, error) {
	fmt.Print("Enter nickname for this device: ")
	nickname, err := reader.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read nickname: %w", err)
	}
	return strings.TrimSpace(nickname), nil
}

// dialHub loads the device identity and connects to the hub with mTLS.
func dialHub(dataDir string, logger *slog.Logger) (*agent.HubClient, *agent.DeviceIdentity, string, error) {
	identityPath := filepath.Join(dataDir, common.IdentityFile)
	identity, err := agent.LoadIdentity(identityPath)
	if err != nil {
		return nil, nil, "", fmt.Errorf("load identity: %w", err)
	}

	tlsDirPath := filepath.Join(dataDir, common.TLSDir)
	caCertPath := filepath.Join(tlsDirPath, common.CACertFile)
	clientCertPath := filepath.Join(tlsDirPath, common.ClientCertFile)
	clientKeyPath := filepath.Join(tlsDirPath, common.ClientKeyFile)

	cfgPath := filepath.Join(dataDir, common.ConfigFile)
	cfg, err := loadConfig(cfgPath)
	if err != nil {
		return nil, nil, "", fmt.Errorf("load config: %w", err)
	}

	hubAddr := cfg.Hub.Address
	hubClient, err := agent.DialWithMTLS(hubAddr, caCertPath, clientCertPath, clientKeyPath, logger)
	if err != nil {
		return nil, nil, hubAddr, fmt.Errorf("dial hub: %w", err)
	}

	_ = common.ProtocolVersion // suppress unused import warning
	return hubClient, identity, hubAddr, nil
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
