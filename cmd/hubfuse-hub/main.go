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
			go awaitTermination(sigCh, cancel, h.Stop, pidPath, &signaled, done, os.Exit)

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
// exit is taken as a parameter rather than calling os.Exit directly so the two
// force-exit branches below are reachable from a test. They are the branches
// most worth pinning and the least observable: os.Exit runs no deferred
// function and leaves no return value to assert on. (#75)
func awaitTermination(sigCh <-chan os.Signal, cancel context.CancelFunc, stop func() (bool, error), pidPath string, signaled *atomic.Bool, done chan<- struct{}, exit func(int)) {
	<-sigCh
	signaled.Store(true)
	cancel()

	// succeeded records that the shutdown finished on its own, and it is set
	// BEFORE any channel is closed. Channel readiness cannot answer this
	// question: stop() can return successfully a few instructions before
	// shuttingDown closes, and a second signal landing in that window would
	// otherwise abort a shutdown that had already succeeded.
	var succeeded atomic.Bool

	shuttingDown := make(chan struct{})
	go func() {
		defer close(shuttingDown)
		settled, err := stop()
		if err != nil {
			fmt.Fprintf(os.Stderr, "hub stop: %v\n", err)
		}
		if settled {
			succeeded.Store(true)
		}
		if !settled {
			// The forced stop itself did not return inside its budget: the
			// process cannot bring itself down cleanly, so leave the same
			// way a second signal does below rather than hang forever. (#75)
			removePIDFile(pidPath)
			exit(1)
			return
		}
		close(done)
	}()

	select {
	case <-shuttingDown:
		// stop settled inside its budget; done is already closed above, RunE
		// takes it from here.
	case <-sigCh:
		// A second signal arrived. If the shutdown has ALREADY succeeded this is
		// not an abort, and it must not be reported as one: sigCh is buffered,
		// so a signal delivered at any point during the shutdown is still
		// sitting in it when the stop completes, at which point both select
		// cases are ready and Go picks between them at random. Exiting 1 there
		// would hand the process supervisor a failure for a shutdown that fully
		// succeeded, decided by a coin toss. (#75)
		// Deliberately untested: the window between stop() returning and the
		// channel closes is a few instructions wide and cannot be forced open
		// from a test — an attempt at one passed with this guard removed, which
		// makes it a test that proves nothing. The guard stays because it is
		// two instructions and the cost of being wrong is a process supervisor
		// recording a failed unit for a shutdown that worked.
		if succeeded.Load() {
			<-shuttingDown
			return
		}

		// A genuine abort: the shutdown had not finished, and the operator has
		// already waited through part of the budget rather than have kill -9 be
		// the only way out. The non-zero code is accurate here. (#75)
		removePIDFile(pidPath)
		exit(1)
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
