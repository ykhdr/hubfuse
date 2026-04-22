package main

import (
	"context"
	"fmt"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"github.com/ykhdr/hubfuse/internal/common"
	"github.com/ykhdr/hubfuse/internal/hub"
	"github.com/ykhdr/hubfuse/internal/hub/store"
)

// resolveDataDir expands the data-dir flag, falling back to the default
// ~/.hubfuse-hub when the flag was not set.
func resolveDataDir(dataDir string) (string, error) {
	if dataDir == "" {
		dataDir = common.HubDataDir
	}
	return common.ExpandHome(dataDir), nil
}

// openStore opens the SQLite store in dataDir using the same path and
// constructor that hub.NewHub uses, allowing concurrent access via WAL mode.
func openStore(ctx context.Context, dataDir string) (store.Store, error) {
	dbPath := filepath.Join(dataDir, common.DBFile)
	s, err := store.NewSQLiteStore(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open store at %q: %w", dbPath, err)
	}
	return s, nil
}

func issueJoinCmd() *cobra.Command {
	var dataDir string
	var configPath string
	var ttlOverride time.Duration
	cmd := &cobra.Command{
		Use:   "issue-join",
		Short: "Issue a single-use token that authorises one Join call.",
		Long: `Creates and stores a join token, printing it to stdout. ` +
			`The token is valid for the configured TTL (default 10m) and can ` +
			`only be used once. Run on the hub host while 'hubfuse-hub start' ` +
			`is running — SQLite WAL mode permits concurrent access.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := context.Background()

			dir, err := resolveDataDir(dataDir)
			if err != nil {
				return err
			}

			ttl := ttlOverride
			if ttl <= 0 {
				cfgPath := configPath
				if cfgPath == "" {
					cfgPath = filepath.Join(dir, common.ConfigFile)
				}
				resolved, err := resolveJoinTokenTTL(cfgPath)
				if err != nil {
					return err
				}
				ttl = resolved
			}

			s, err := openStore(ctx, dir)
			if err != nil {
				return err
			}
			defer s.Close()

			now := time.Now()
			tok := &store.JoinToken{
				Token:     hub.GenerateInviteCode(),
				ExpiresAt: now.Add(ttl),
				CreatedAt: now,
			}
			if err := s.CreateJoinToken(ctx, tok); err != nil {
				return fmt.Errorf("create join token: %w", err)
			}

			if _, err := fmt.Fprintln(cmd.OutOrStdout(), tok.Token); err != nil {
				return fmt.Errorf("write token: %w", err)
			}
			if _, err := fmt.Fprintf(cmd.ErrOrStderr(),
				"Share this token with the joining device. Expires at %s.\n",
				tok.ExpiresAt.UTC().Format(time.RFC3339)); err != nil {
				return fmt.Errorf("write expiry notice: %w", err)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&dataDir, "data-dir", "", "hub data directory (defaults to ~/.hubfuse-hub)")
	cmd.Flags().StringVar(&configPath, "config", "", "path to hub config file (defaults to <data-dir>/config.kdl)")
	cmd.Flags().DurationVar(&ttlOverride, "ttl", 0, "override token TTL (0 = use config/default)")
	return cmd
}
