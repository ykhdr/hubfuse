package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"

	"github.com/spf13/cobra"
	"github.com/ykhdr/hubfuse/internal/hub"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hubfuse-hub",
		Short: "HubFuse hub server",
	}
	cmd.AddCommand(startCmd(), stopCmd(), statusCmd())
	return cmd
}

func startCmd() *cobra.Command {
	var (
		listen    string
		dataDir   string
		logLevel  string
		logOutput string
	)

	cmd := &cobra.Command{
		Use:   "start",
		Short: "Start the hub server",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg := hub.HubConfig{
				ListenAddr: listen,
				DataDir:    dataDir,
				LogLevel:   logLevel,
				LogOutput:  logOutput,
			}

			h, err := hub.NewHub(cfg)
			if err != nil {
				return fmt.Errorf("create hub: %w", err)
			}

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
	cmd.Flags().StringVar(&dataDir, "data-dir", "~/.hubfuse-hub", "data directory")
	cmd.Flags().StringVar(&logLevel, "log-level", "info", "log level (debug, info, warn, error)")
	cmd.Flags().StringVar(&logOutput, "log-output", "stderr", "log output (stderr or file path)")

	return cmd
}

func stopCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop a running hub server",
		RunE: func(cmd *cobra.Command, args []string) error {
			pidFile := expandHome("~/.hubfuse-hub/hubfuse-hub.pid")
			pid, err := readPID(pidFile)
			if err != nil {
				return fmt.Errorf("read PID file %q: %w", pidFile, err)
			}
			proc, err := os.FindProcess(pid)
			if err != nil {
				return fmt.Errorf("find process %d: %w", pid, err)
			}
			if err := proc.Signal(syscall.SIGTERM); err != nil {
				return fmt.Errorf("send SIGTERM to %d: %w", pid, err)
			}
			fmt.Printf("sent SIGTERM to hub (pid %d)\n", pid)
			return nil
		},
	}
}

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show hub server status",
		RunE: func(cmd *cobra.Command, args []string) error {
			pidFile := expandHome("~/.hubfuse-hub/hubfuse-hub.pid")
			pid, err := readPID(pidFile)
			if err != nil {
				fmt.Println("hub is not running (no PID file)")
				return nil
			}
			proc, err := os.FindProcess(pid)
			if err != nil {
				fmt.Printf("hub is not running (pid %d not found)\n", pid)
				return nil
			}
			if err := proc.Signal(syscall.Signal(0)); err != nil {
				fmt.Printf("hub is not running (pid %d, signal error: %v)\n", pid, err)
				return nil
			}
			fmt.Printf("hub is running (pid %d)\n", pid)
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

// readPID reads an integer PID from the file at path.
func readPID(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		return 0, fmt.Errorf("parse PID: %w", err)
	}
	return pid, nil
}
