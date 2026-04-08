// Command server runs the DP-1 feed operator HTTP API: Gin, PostgreSQL (pgx), golang-migrate,
// and dp1-go for validation and signing. Configuration is YAML plus DP1_FEED_* environment overrides.
package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/getsentry/sentry-go"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/zap"

	"github.com/display-protocol/dp1-feed-v2/internal/config"
	"github.com/display-protocol/dp1-feed-v2/internal/dp1svc"
	"github.com/display-protocol/dp1-feed-v2/internal/executor"
	"github.com/display-protocol/dp1-feed-v2/internal/fetcher"
	"github.com/display-protocol/dp1-feed-v2/internal/httpserver"
	"github.com/display-protocol/dp1-feed-v2/internal/logger"
	"github.com/display-protocol/dp1-feed-v2/internal/publisherauth"
	"github.com/display-protocol/dp1-feed-v2/internal/store/pg"
)

const version = "0.1.0"

func main() {
	configPath := flag.String("config", "config/config.yaml", "path to YAML config")
	migrationsDir := flag.String("migrations", "db/migrations", "path to SQL migrations (golang-migrate)")
	skipMigrate := flag.Bool("skip-migrate", false, "skip running migrations on startup")
	flag.Parse()

	// 1) Load and validate config (DB URL, API key, signing key; derive did:key kid from the key).
	cfg, err := config.Load(*configPath)
	if err != nil {
		panic(err)
	}

	if cfg.Sentry.DSN != "" {
		if err := sentry.Init(sentry.ClientOptions{
			Dsn:   cfg.Sentry.DSN,
			Debug: cfg.Logging.Debug,
		}); err != nil {
			panic(err)
		}
		defer sentry.Flush(2 * time.Second)
	}

	zlog, err := logger.New(logger.Config{Debug: cfg.Logging.Debug})
	if err != nil {
		panic(err)
	}
	defer func() { _ = zlog.Sync() }()

	// 2) PostgreSQL pool, optional migrate-up on startup, then wire store → dp1 → fetcher → executor → HTTP.
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, cfg.Database.URL)
	if err != nil {
		zlog.Fatal("pg connect", zap.Error(err))
	}
	defer pool.Close()

	if !*skipMigrate {
		if err := pg.RunMigrations(cfg.Database.URL, *migrationsDir); err != nil {
			zlog.Fatal("migrate", zap.Error(err))
		}
	}

	st := pg.NewStore(pool)
	dp1, err := dp1svc.New(cfg.Playlist.SigningKeyHex, cfg.Playlist.SigningKid)
	if err != nil {
		zlog.Fatal("dp1svc", zap.Error(err))
	}
	f := fetcher.NewHTTPFetcher(cfg.Playlist.FetchTimeout, cfg.Playlist.FetchMaxBodyBytes)

	exec := executor.New(st, dp1, cfg.Extensions.Enabled, f, cfg.Playlist.PublicBaseURL)
	authz := publisherauth.New(st)
	pubauth, err := publisherauth.NewService(pool, cfg.PublisherAuth)
	if err != nil {
		zlog.Fatal("publisher auth", zap.Error(err))
	}
	srv := httpserver.New(cfg, zlog, exec, authz, pubauth, version)

	// 3) Graceful shutdown on SIGINT/SIGTERM, then block on ListenAndServe.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sig
		shctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = srv.Shutdown(shctx)
	}()

	if err := srv.ListenAndServe(); err != nil {
		zlog.Fatal("serve", zap.Error(err))
	}
}
