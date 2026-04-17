package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/ykhdr/hubfuse/cmd/internal/clierrors"
	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/common/daemonize"
	"github.com/ykhdr/hubfuse/internal/hub"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, clierrors.Format(err, nil))
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hubfuse-hub",
		Short: "HubFuse hub server",
		// main prints errors itself via clierrors.Format, so silence Cobra's
		// default "Error: ..." prefix. Usage is only suppressed once we've
		// passed arg/flag validation (see PersistentPreRunE below) so that
		// malformed-command errors still print the usage block.
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			cmd.SilenceUsage = true
			return nil
		},
	}
	cmd.AddCommand(startCmd(), stopCmd(), statusCmd())
	return cmd
}

func startCmd() *cobra.Command {
	var (
		listen    string
		dataDir   string
		logFile   string
		logLevel  string
		verbose   bool
		extraSANs []string
		daemon    bool
		deviceRet string
	)

	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the hub server",
		RunE: func(cmd *cobra.Command, args []string) error {
			expandedData := common.ExpandHome(dataDir)
			pidPath := filepath.Join(expandedData, common.HubPIDFile)
			defaultLog := filepath.Join(expandedData, common.HubLogFile)
			configPath := filepath.Join(expandedData, common.ConfigFile)

			if pid, alive, err := daemonize.CheckRunning(pidPath); err != nil {
				return fmt.Errorf("check existing hub: %w", err)
			} else if alive {
				return fmt.Errorf("hub already running (pid %d)", pid)
			}

			if daemon && !daemonize.IsChild() {
				if err := os.MkdirAll(expandedData, 0o700); err != nil {
					return fmt.Errorf("create data dir: %w", err)
				}
				return daemonize.Spawn(daemonize.SpawnOpts{
					LogPath:     defaultLog,
					PIDFilePath: pidPath,
				})
			}

			retention, err := resolveDeviceRetention(deviceRet, cmd.Flags().Changed("device-retention"), configPath)
			if err != nil {
				return err
			}

			cfg := hub.Config{
				ListenAddr:      listen,
				DataDir:         dataDir,
				LogFile:         logFile,
				LogLevel:        common.ParseLogLevel(logLevel),
				Verbose:         verbose,
				ExtraSANs:       extraSANs,
				DeviceRetention: retention,
				OnReady: func() {
					if err := daemonize.WritePIDFile(pidPath); err != nil {
						fmt.Fprintf(os.Stderr, "warning: write pid file: %v\n", err)
					}
				},
			}

			h, err := hub.NewHub(cfg)
			if err != nil {
				return fmt.Errorf("create hub: %w", err)
			}
			defer func() {
				if err := os.Remove(pidPath); err != nil && !os.IsNotExist(err) {
					fmt.Fprintf(os.Stderr, "warning: remove pid file: %v\n", err)
				}
			}()

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

			go func() {
				<-sigCh
				cancel()
				if err := h.Stop(); err != nil {
					fmt.Fprintf(os.Stderr, "hub stop: %v\n", err)
				}
			}()

			if err := h.Start(ctx); err != nil {
				return fmt.Errorf("hub start: %w", err)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&listen, "listen", ":9090", "address to listen on")
	cmd.Flags().StringVar(&dataDir, "data-dir", common.HubDataDir, "data directory")
	cmd.Flags().StringVar(&logFile, "log-file", "", "write JSON logs to file (disabled by default)")
	cmd.Flags().StringVar(&logLevel, "log-level", "debug", "log file level (debug, info, warn, error)")
	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "show debug logs in console")
	cmd.Flags().StringSliceVar(&extraSANs, "san", nil, "additional SANs for TLS certificate (IPs or hostnames)")
	cmd.Flags().BoolVarP(&daemon, "daemon", "d", false, "detach from terminal and run in the background")
	cmd.Flags().StringVar(&deviceRet, "device-retention", hub.DefaultDeviceRetention.String(), "prune offline devices older than this duration (0 = never prune)")

	return cmd
}

func stopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop a running hub server",
		RunE: func(cmd *cobra.Command, args []string) error {
			pidPath := common.ExpandHome(filepath.Join(common.HubDataDir, common.HubPIDFile))
			return daemonize.SignalStop(pidPath, "hub")
		},
	}
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show hub server status",
		RunE: func(cmd *cobra.Command, args []string) error {
			pidPath := common.ExpandHome(filepath.Join(common.HubDataDir, common.HubPIDFile))
			return daemonize.ReportStatus(pidPath, "hub")
		},
	}
}

