// Command server runs the DP-1 feed operator HTTP API: Gin, PostgreSQL (pgx), golang-migrate,
// and dp1-go for validation and signing. Configuration is YAML plus DP1_FEED_* environment overrides.
package main

import (
	"context"
	"flag"
	"net/http"
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
	"github.com/display-protocol/dp1-feed-v2/internal/notification"
	"github.com/display-protocol/dp1-feed-v2/internal/store/pg"
)

const version = "1.1.0"

func main() {
	configPath := flag.String("config", "config/config.yaml", "path to YAML config")
	migrationsDir := flag.String("migrations", "db/migrations", "path to SQL migrations (golang-migrate)")
	skipMigrate := flag.Bool("skip-migrate", false, "skip running migrations on startup")
	flag.Parse()

	// 1) Load and validate config (DB URL, signing key; derive did:key kid from the key). Mutating routes
	// are authorized by request signatures, not an API key.
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
	if cfg.Notifications.PublicKey != "" {
		zlog.Info("webhook signing public key", zap.String("public_key", cfg.Notifications.PublicKey))
	}

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
	f := fetcher.NewHTTPFetcher(cfg.Playlist.FetchTimeout, cfg.Playlist.FetchMaxBodyBytes,
		fetcher.AllowPrivateDestinations(cfg.Playlist.AllowPrivateFetchDestinations))
	if cfg.Playlist.AllowPrivateFetchDestinations {
		zlog.Warn("playlist fetch may reach private addresses; this must not be enabled in production",
			zap.String("setting", "playlist.allow_private_fetch_destinations"))
	}

	execOptions := []executor.Option{
		executor.WithIntentClockSkew(cfg.Auth.IntentMaxClockSkew),
		executor.WithMaxPlaylistReferences(cfg.Playlist.MaxPlaylistReferences),
		executor.WithMaxResolvedBytes(cfg.Playlist.MaxResolvedBytes),
	}
	if len(cfg.Notifications.Clients) > 0 {
		privateKey, err := notification.ParseP256PrivateKeyHex(cfg.Notifications.PrivateKeyHex)
		if err != nil {
			zlog.Fatal("webhook private key", zap.Error(err))
		}
		httpClient := &http.Client{Timeout: cfg.Notifications.Timeout}
		clients := make([]notification.NamedClient, 0, len(cfg.Notifications.Clients))
		for _, clientConfig := range cfg.Notifications.Clients {
			client, err := notification.NewWebhookClient(clientConfig.URL, privateKey, httpClient)
			if err != nil {
				zlog.Fatal("notification client", zap.String("client", clientConfig.Name), zap.Error(err))
			}
			clients = append(clients, notification.NamedClient{Name: clientConfig.Name, Client: client})
		}
		execOptions = append(execOptions, executor.WithNotificationClient(notification.NewDispatcher(zlog, cfg.Notifications.Timeout, clients)))
	}
	exec := executor.New(st, dp1, cfg.Extensions.Enabled, f, cfg.Playlist.PublicBaseURL, execOptions...)
	srv := httpserver.New(cfg, zlog, exec, version)

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
