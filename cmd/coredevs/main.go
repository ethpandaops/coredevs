// Command coredevs serves the superset of Ethereum core developers across
// datasources over an HTTP API.
//
// It runs in one of three cluster roles (cluster.role):
//   - standalone: a single self-contained pod (the default; no Postgres).
//   - writer: the one pod that syncs, fetches keys and publishes the canonical
//     snapshot to Postgres. It receives no user traffic.
//   - reader: a stateless serving pod that loads the published snapshot from
//     Postgres and serves it. Many readers serve identical data — no split brain.
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
	"github.com/ethpandaops/coredevs/internal/store"
	"github.com/ethpandaops/coredevs/internal/syncer"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	var (
		configPath string
		role       string
	)

	cmd := &cobra.Command{
		Use:           "coredevs",
		Short:         "Index Ethereum core developers across datasources",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd.Context(), configPath, role)
		},
	}

	cmd.Flags().StringVarP(&configPath, "config", "c", "config.yaml", "path to the config file")
	// The cluster role differs per pod but the config is baked into one image, so
	// the writer and reader Deployments select their role with this flag.
	cmd.Flags().StringVar(&role, "role", "", "override cluster.role: standalone, writer or reader")

	return cmd
}

func run(ctx context.Context, configPath, roleOverride string) error {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load(configPath)
	if err != nil {
		logger.ErrorContext(ctx, "failed to load config", slog.Any("error", err))

		return err
	}

	if roleOverride != "" {
		cfg.Cluster.Role = roleOverride
		if err := cfg.Validate(); err != nil {
			logger.ErrorContext(ctx, "invalid role override", slog.Any("error", err))

			return err
		}
	}

	ctx, stop := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	httpClient := &http.Client{Timeout: 30 * time.Second}
	indexStore := index.NewStore()

	var cleanups []func()
	defer func() {
		for i := len(cleanups) - 1; i >= 0; i-- {
			cleanups[i]()
		}
	}()

	logger.InfoContext(ctx, "starting", slog.String("role", cfg.Cluster.Role))

	var (
		keyProvider api.KeyProvider
		statusFn    api.StatusFunc
		orgs        api.OrgResolver
	)

	switch cfg.Cluster.Role {
	case config.RoleReader:
		keyProvider, statusFn, err = startReader(ctx, logger, cfg, indexStore, &cleanups)
	default: // standalone or writer
		keyProvider, statusFn, orgs, err = startSyncing(ctx, logger, cfg, httpClient, indexStore, &cleanups)
	}
	if err != nil {
		logger.ErrorContext(ctx, "failed to start", slog.Any("error", err))

		return err
	}

	handler := api.New(logger, cfg, indexStore, statusFn, orgs, keyProvider)

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

// startSyncing wires the upstream syncer and key walker that drive standalone
// and writer pods. For the writer role it also publishes snapshots to Postgres.
func startSyncing(ctx context.Context, logger *slog.Logger, cfg *config.Config, httpClient *http.Client, indexStore *index.Store, cleanups *[]func()) (api.KeyProvider, api.StatusFunc, api.OrgResolver, error) {
	sources, orgClient := buildSources(logger, cfg, httpClient)

	floors := map[string]int{
		source.NameProtocolGuild: cfg.Sources.ProtocolGuild.MinMembers,
		source.NameGitHubOrg:     cfg.Sources.GitHubOrg.MinMembers,
	}

	sync := syncer.New(logger, indexStore, sources, cfg.SyncInterval, cfg.SnapshotPath, floors, cfg.ExcludedHandles())
	if err := sync.Start(ctx); err != nil {
		return nil, nil, nil, err
	}
	*cleanups = append(*cleanups, func() { _ = sync.Stop() })

	keyCache := buildKeyCache(logger, cfg, indexStore, httpClient)
	if keyCache != nil {
		if err := keyCache.Start(ctx); err != nil {
			return nil, nil, nil, err
		}
		*cleanups = append(*cleanups, func() { _ = keyCache.Stop() })
	}

	if cfg.Cluster.Role == config.RoleWriter {
		pg, err := store.New(ctx, logger, cfg.Cluster.DSN())
		if err != nil {
			return nil, nil, nil, err
		}
		*cleanups = append(*cleanups, pg.Close)

		pub := newPublisher(logger, pg, indexStore, keyCache, sync.Statuses, cfg.Cluster.Postgres.PublishInterval)
		pub.Start(ctx)
		*cleanups = append(*cleanups, pub.Stop)
	}

	return keyResolver(keyCache), sync.Statuses, orgResolver(orgClient), nil
}

// startReader connects to Postgres and continuously loads the published snapshot
// into the serving structures. Reader pods never call GitHub.
func startReader(ctx context.Context, logger *slog.Logger, cfg *config.Config, indexStore *index.Store, cleanups *[]func()) (api.KeyProvider, api.StatusFunc, error) {
	pg, err := store.New(ctx, logger, cfg.Cluster.DSN())
	if err != nil {
		return nil, nil, err
	}
	*cleanups = append(*cleanups, pg.Close)

	reader := keys.NewReader()
	statuses := &atomicStatuses{}

	p := newPoller(logger, pg, indexStore, reader, statuses, cfg.Cluster.Postgres.RefreshInterval)
	p.Start(ctx)
	*cleanups = append(*cleanups, p.Stop)

	var keyProvider api.KeyProvider
	if cfg.Keys.Enabled {
		keyProvider = reader
	}

	return keyProvider, statuses.Get, nil
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
		CacheDir:             cfg.Keys.CacheDir,
		WarmTimeout:          cfg.Keys.WarmTimeout,
	}, handlesFn)
}

// orgResolver adapts the concrete client to the API's interface, returning a
// nil interface when the source is disabled so the API can detect it.
func orgResolver(c *githuborg.Client) api.OrgResolver {
	if c == nil {
		return nil
	}

	return c
}

// keyResolver adapts the concrete cache to the API's interface, returning a nil
// interface when the cache is disabled so the API can detect it.
func keyResolver(c *keys.Cache) api.KeyProvider {
	if c == nil {
		return nil
	}

	return c
}
