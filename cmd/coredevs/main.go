// Command coredevs serves the superset of Ethereum core developers across
// datasources over an HTTP API.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ethpandaops/coredevs/internal/api"
	"github.com/ethpandaops/coredevs/internal/config"
	"github.com/ethpandaops/coredevs/internal/index"
	"github.com/ethpandaops/coredevs/internal/keys"
	"github.com/ethpandaops/coredevs/internal/source"
	"github.com/ethpandaops/coredevs/internal/source/githuborg"
	manualsource "github.com/ethpandaops/coredevs/internal/source/manual"
	"github.com/ethpandaops/coredevs/internal/source/protocolguild"
	"github.com/ethpandaops/coredevs/internal/syncer"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	var configPath string

	cmd := &cobra.Command{
		Use:           "coredevs",
		Short:         "Index Ethereum core developers across datasources",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd.Context(), configPath)
		},
	}

	cmd.Flags().StringVarP(&configPath, "config", "c", "config.yaml", "path to the config file")

	return cmd
}

func run(ctx context.Context, configPath string) error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load(configPath)
	if err != nil {
		logger.ErrorContext(ctx, "failed to load config", slog.Any("error", err))

		return err
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	httpClient := &http.Client{Timeout: 30 * time.Second}

	sources, orgClient := buildSources(logger, cfg, httpClient)

	floors := map[string]int{
		source.NameProtocolGuild: cfg.Sources.ProtocolGuild.MinMembers,
		source.NameGitHubOrg:     cfg.Sources.GitHubOrg.MinMembers,
	}

	store := index.NewStore()
	sync := syncer.New(logger, store, sources, cfg.SyncInterval, cfg.SnapshotPath, floors, cfg.ExcludedHandles())

	if err := sync.Start(ctx); err != nil {
		logger.ErrorContext(ctx, "failed to start syncer", slog.Any("error", err))

		return err
	}
	defer func() {
		if err := sync.Stop(); err != nil {
			logger.ErrorContext(ctx, "failed to stop syncer", slog.Any("error", err))
		}
	}()

	keyCache := buildKeyCache(logger, cfg, store, httpClient)
	if keyCache != nil {
		if err := keyCache.Start(ctx); err != nil {
			logger.ErrorContext(ctx, "failed to start key cache", slog.Any("error", err))

			return err
		}
		defer func() {
			if err := keyCache.Stop(); err != nil {
				logger.ErrorContext(ctx, "failed to stop key cache", slog.Any("error", err))
			}
		}()
	}

	handler := api.New(logger, cfg, store, sync, orgResolver(orgClient), keyResolver(keyCache))

	srv := &http.Server{
		Addr:              cfg.HTTP.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		logger.InfoContext(ctx, "starting http server", slog.String("addr", cfg.HTTP.Addr))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
		}
	}()

	select {
	case <-ctx.Done():
		logger.InfoContext(ctx, "shutdown signal received")
	case err := <-serveErr:
		logger.ErrorContext(ctx, "http server failed", slog.Any("error", err))

		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.ErrorContext(ctx, "failed to shut down http server", slog.Any("error", err))

		return err
	}

	return nil
}

// buildSources constructs the enabled datasources from config. The GitHub org
// client is returned separately so the API can resolve arbitrary orgs on demand.
func buildSources(logger *slog.Logger, cfg *config.Config, httpClient *http.Client) ([]source.Source, *githuborg.Client) {
	sources := make([]source.Source, 0, 3)

	if manual := cfg.ManualMembers(); len(manual) > 0 {
		sources = append(sources, manualsource.New(manual))
	}

	if cfg.Sources.ProtocolGuild.Enabled {
		sources = append(sources, protocolguild.New(
			logger, httpClient, cfg.Sources.ProtocolGuild.URL, cfg.SectionTeams(),
		))
	}

	var orgClient *githuborg.Client
	if cfg.Sources.GitHubOrg.Enabled {
		token := os.Getenv(cfg.Sources.GitHubOrg.TokenEnv)
		orgClient = githuborg.NewClient(logger, httpClient, cfg.Sources.GitHubOrg.BaseURL, token)
		sources = append(sources, githuborg.NewSource(logger, orgClient, cfg.OrgTeams()))
	}

	return sources, orgClient
}

// orgResolver adapts the concrete client to the API's interface, returning a
// nil interface when the source is disabled so the API can detect it.
func orgResolver(c *githuborg.Client) api.OrgResolver {
	if c == nil {
		return nil
	}

	return c
}

// buildKeyCache constructs the GitHub key cache when enabled, sourcing its
// handle set from the live deduplicated index. It returns nil when disabled.
func buildKeyCache(logger *slog.Logger, cfg *config.Config, store *index.Store, httpClient *http.Client) *keys.Cache {
	if !cfg.Keys.Enabled {
		return nil
	}

	handlesFn := func() []string {
		idx := store.Get()
		if idx == nil {
			return nil
		}

		users := idx.Users()
		handles := make([]string, 0, len(users))
		for _, u := range users {
			handles = append(handles, u.Handle)
		}

		return handles
	}

	return keys.New(logger, httpClient, keys.Config{
		Enabled:              cfg.Keys.Enabled,
		BaseURL:              cfg.Keys.BaseURL,
		RefreshInterval:      cfg.Keys.RefreshInterval,
		MaxRequestsPerSecond: cfg.Keys.MaxRequestsPerSecond,
		SnapshotPath:         cfg.Keys.SnapshotPath,
	}, handlesFn)
}

// keyResolver adapts the concrete cache to the API's interface, returning a nil
// interface when the cache is disabled so the API can detect it.
func keyResolver(c *keys.Cache) api.KeyProvider {
	if c == nil {
		return nil
	}

	return c
}
