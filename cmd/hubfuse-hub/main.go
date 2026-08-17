package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/ykhdr/hubfuse/cmd/internal/clierrors"
	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/common/daemonize"
	"github.com/ykhdr/hubfuse/internal/hub"
	"github.com/ykhdr/hubfuse/internal/version"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, clierrors.Format(err, nil))
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "hubfuse-hub",
		Short:   "HubFuse hub server",
		Version: version.Short(),
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
	cmd.AddCommand(startCmd(), stopCmd(), statusCmd(), issueJoinCmd(), versionCmd())
	return cmd
}

// versionCmd implements: hubfuse-hub version
func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), version.Full())
			return err
		},
	}
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
		hbTimeout string
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

			joinTokenTTL, err := resolveJoinTokenTTL(configPath)
			if err != nil {
				return err
			}

			heartbeatTimeout, err := resolveHeartbeatTimeout(hbTimeout, cmd.Flags().Changed("heartbeat-timeout"), configPath)
			if err != nil {
				return err
			}

			cfg := hub.Config{
				ListenAddr:       listen,
				DataDir:          dataDir,
				LogFile:          logFile,
				LogLevel:         common.ParseLogLevel(logLevel),
				Verbose:          verbose,
				ExtraSANs:        extraSANs,
				DeviceRetention:  retention,
				JoinTokenTTL:     joinTokenTTL,
				HeartbeatTimeout: heartbeatTimeout,
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
			defer removePIDFile(pidPath)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			sigCh := make(chan os.Signal, 1)
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
			defer signal.Stop(sigCh)

			// signaled and done let RunE tell "h.Start failed on its own, no
			// signal ever came in" (return immediately, requirement 3) apart
			// from "a signal is driving this shutdown, wait for it to finish
			// settling" (requirement 1) without reading sigCh a second time
			// itself — awaitTermination owns that channel. (#75)
			var signaled atomic.Bool
			done := make(chan struct{})
			go awaitTermination(sigCh, cancel, h.Stop, pidPath, &signaled, done)

			startErr := h.Start(ctx)

			waitForShutdown(done, &signaled)

			if startErr != nil {
				return fmt.Errorf("hub start: %w", startErr)
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
	cmd.Flags().StringVar(&hbTimeout, "heartbeat-timeout", hub.DefaultHeartbeatTimeout.String(), "mark a device offline after this long without a heartbeat")

	return cmd
}

// removePIDFile deletes the hub's PID file, tolerating one already gone.
// Every path that ends the process — RunE's own defer on a clean return, and
// each explicit call site right before an os.Exit(1) below, since deferred
// functions do not run under os.Exit — calls this exactly once, which is
// what keeps the removal itself from ever running twice. (#75)
func removePIDFile(pidPath string) {
	if err := os.Remove(pidPath); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "warning: remove pid file: %v\n", err)
	}
}

// awaitTermination reads OS termination signals for the running hub and
// drives its shutdown. It is the ONLY goroutine that can act on the outcome:
// while stop is in flight, the main goroutine is parked inside
// grpcServer.Serve, whose deferred <-s.done.Done() (grpc-go server.go:
// 885-892) does not release until the stop sequence completes, so RunE
// cannot regain control to decide anything on the settled == false path.
//
// signaled is stored true before stop is even called, not after it returns:
// Start's only way of unblocking is a side effect of stop (Serve returning
// nil once grpc's internal quit fires), so by the time RunE's h.Start(ctx)
// call returns, this store has already happened-before that on grpc's own
// synchronization — waitForShutdown's read is safe without extra locking.
// (#75)
func awaitTermination(sigCh <-chan os.Signal, cancel context.CancelFunc, stop func() (bool, error), pidPath string, signaled *atomic.Bool, done chan<- struct{}) {
	<-sigCh
	signaled.Store(true)
	cancel()

	shuttingDown := make(chan struct{})
	go func() {
		defer close(shuttingDown)
		settled, err := stop()
		if err != nil {
			fmt.Fprintf(os.Stderr, "hub stop: %v\n", err)
		}
		if !settled {
			// The forced stop itself did not return inside its budget: the
			// process cannot bring itself down cleanly, so leave the same
			// way a second signal does below rather than hang forever. (#75)
			removePIDFile(pidPath)
			os.Exit(1)
		}
		close(done)
	}()

	select {
	case <-shuttingDown:
		// stop settled inside its budget; done is already closed above, RunE
		// takes it from here.
	case <-sigCh:
		// A second signal arrived while the first stop was still running:
		// the operator already waited once through the budget and is asking
		// to leave now rather than have kill -9 be the only way out. (#75)
		removePIDFile(pidPath)
		os.Exit(1)
	}
}

// waitForShutdown blocks on done only when a signal was actually observed.
// Without the guard, a hub whose h.Start failed on its own — e.g. "listen:
// address already in use", before anyone sent a signal — would hang RunE
// forever: awaitTermination is still parked on sigCh and will never close
// done, because nothing is ever going to arrive on it. (#75)
func waitForShutdown(done <-chan struct{}, signaled *atomic.Bool) {
	if !signaled.Load() {
		return
	}
	<-done
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
