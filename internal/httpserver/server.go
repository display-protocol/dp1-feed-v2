// Package httpserver is the Gin HTTP layer: routes, JSON/error envelopes, API-key auth on mutating endpoints,
// and wiring to executor.Executor (business logic). List/read routes are public; POST/PUT/DELETE require Bearer API key.
package httpserver

import (
	"context"
	"fmt"
	"net/http"
	"time"

	sentrygin "github.com/getsentry/sentry-go/gin"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/display-protocol/dp1-feed-v2/internal/config"
	"github.com/display-protocol/dp1-feed-v2/internal/executor"
)

// Server wraps stdlib http.Server with the Gin engine built in New.
type Server struct {
	cfg    *config.Config
	engine *gin.Engine
	srv    *http.Server
	log    *zap.Logger
}

// New builds a Gin engine: recovery, optional Sentry, Zap request logging, RegisterRoutes, and http.Server timeouts from cfg.
func New(cfg *config.Config, log *zap.Logger, exec executor.Executor, version string) *Server {
	if !cfg.Logging.Debug {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.New()
	if cfg.Sentry.DSN != "" {
		r.Use(sentrygin.New(sentrygin.Options{Repanic: true}))
	}
	r.Use(gin.Recovery())
	r.Use(ZapLogger(log))

	h := &Handler{Exec: exec, Log: log, Version: version}
	RegisterRoutes(r, h, cfg, log)

	addr := cfg.Address()
	srv := &http.Server{
		Addr:         addr,
		Handler:      r,
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		IdleTimeout:  cfg.Server.IdleTimeout,
	}
	return &Server{cfg: cfg, engine: r, srv: srv, log: log}
}

// ListenAndServe starts the HTTP server (blocking).
func (s *Server) ListenAndServe() error {
	s.log.Info("listening", zap.String("addr", s.srv.Addr))
	if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("http server: %w", err)
	}
	return nil
}

// Shutdown gracefully stops the server (caller may use a deadline; this also applies an internal 15s cap).
func (s *Server) Shutdown(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	return s.srv.Shutdown(ctx)
}
