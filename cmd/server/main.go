// Command server runs the DP-1 feed operator HTTP API: Gin, PostgreSQL (pgx), golang-migrate,
// and dp1-go for validation and signing. Configuration is YAML plus DP1_FEED_* environment overrides.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

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

const version = "1.2.0"

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

	zlog, shutdownLogger, err := logger.New(logger.Config{
		Debug: cfg.Logging.Debug,
		Cloudflare: logger.StreamConfig{
			URL:         cfg.Logging.Cloudflare.StreamURL,
			APIKey:      cfg.Logging.Cloudflare.APIKey,
			Service:     cfg.Logging.Service,
			Environment: cfg.Logging.Environment,
		},
	})
	if err != nil {
		panic(err)
	}
	// Keep a deferred close for unexpected panics. The normal serve path also closes explicitly before
	// any non-zero exit; stream shutdown is idempotent, so the successful path may safely reach both.
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), logger.DefaultShutdownTimeout)
		defer cancel()
		if err := shutdownLogger(ctx); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "logger shutdown failed: %v\n", err)
		}
	}()
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

	// 3) Keep the process logger open until graceful HTTP shutdown has finished draining handlers.
	processContext, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stopSignals()
	if err := serveUntilShutdownAndCloseLogger(processContext, srv, zlog, shutdownLogger); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "server stopped with error: %v\n", err)
		os.Exit(1)
	}
}

type gracefulServer interface {
	ListenAndServe() error
	Shutdown(context.Context) error
}

// serveUntilShutdown owns HTTP lifecycle ordering. Shutdown runs synchronously after a process signal,
// and the function does not return to main's deferred logger close until active handlers have drained.
func serveUntilShutdown(processContext context.Context, srv gracefulServer) error {
	serveErrors := make(chan error, 1)
	go func() {
		serveErrors <- srv.ListenAndServe()
	}()

	select {
	case err := <-serveErrors:
		return err
	case <-processContext.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		shutdownErr := srv.Shutdown(shutdownContext)
		cancel()
		serveErr := <-serveErrors
		return errors.Join(shutdownErr, serveErr)
	}
}

// serveUntilShutdownAndCloseLogger reports a server failure while remote admission is still open, then
// drains every accepted log before the caller may exit. This must not be replaced with zap.Logger.Fatal:
// Fatal calls os.Exit directly and would bypass the bounded logger shutdown on HTTP drain timeouts.
func serveUntilShutdownAndCloseLogger(
	processContext context.Context,
	srv gracefulServer,
	zlog *zap.Logger,
	shutdownLogger logger.ShutdownFunc,
) error {
	serveErr := serveUntilShutdown(processContext, srv)
	if serveErr != nil {
		zlog.Error("serve", zap.Error(serveErr))
	}

	shutdownContext, cancel := context.WithTimeout(context.Background(), logger.DefaultShutdownTimeout)
	defer cancel()
	return errors.Join(serveErr, shutdownLogger(shutdownContext))
}
