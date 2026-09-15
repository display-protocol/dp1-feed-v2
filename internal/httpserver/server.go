// Package httpserver is the Gin HTTP layer: routes, JSON/error envelopes, signature gating on mutating
// endpoints, and wiring to executor.Executor (business logic). List/read routes are public; POST/PUT/DELETE
// require a signed body (RequireSignatures), and the executor enforces ownership. There is no API key.
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

// Server wraps stdlib http.Server with the Gin engine built in New. Tests drive srv.Handler rather than
// the engine: that is the production chain, body cap included.
type Server struct {
	cfg *config.Config
	srv *http.Server
	log *zap.Logger
}

// New builds a Gin engine — recovery, optional Sentry, Zap request logging, RegisterRoutes — wraps it in
// the inbound body cap, and applies http.Server timeouts from cfg.
func New(cfg *config.Config, log *zap.Logger, exec executor.Executor, version string) *Server {
	if !cfg.Logging.Debug {
		gin.SetMode(gin.ReleaseMode)
	}
	r := gin.New()
	if cfg.Sentry.DSN != "" {
		r.Use(sentrygin.New(sentrygin.Options{Repanic: true}))
	}
	r.Use(gin.Recovery())
	r.Use(newCORSMiddleware(cfg))
	r.Use(ZapLogger(log))

	h := &Handler{Exec: exec, Log: log, Version: version}
	RegisterRoutes(r, h, cfg, log)

	// The inbound body cap sits outside gin, on the http.Server handler, so it is structurally ahead of
	// every route middleware. It has to be: RequireSignatures reads the whole body to look for signatures
	// before any credential exists, so this cap is the only thing between an anonymous caller and an
	// allocation of their choosing. As a gin middleware it could be registered after the routes (gin
	// snapshots each route's chain at registration) and nothing would fail to compile; here there is no
	// order to get wrong. TestNew_CapsBodyBeforeSignatureCheck drives this handler for that reason.
	//
	// MaxBytesHandler hands net/http's own writer to MaxBytesReader, so on overflow the server flags the
	// connection close-after-reply: the 413 RequireSignatures writes goes out with Connection: close, the
	// connection is never reused, and the socket is half-closed with the RST-avoidance wait so the client
	// still receives the reply. (A gin-level wrapper would hide that hook behind gin's ResponseWriter and
	// lose the flag.) What the flag does not change: after the handler returns, net/http still closes the
	// request body, which discards — to io.Discard, never buffered — up to 256 KiB of the remainder, or
	// none when a declared Content-Length leaves more than that. So the bound is cap+1 bytes buffered,
	// plus at most 256 KiB read and dropped. A declared Content-Length above the cap is not
	// short-circuited: it would spare reading one cap's worth of bytes on an honest oversize upload, but
	// the memory bound is identical, and the abusive case sends chunked anyway.
	maxBody := cfg.Server.MaxRequestBytes
	if maxBody <= 0 {
		maxBody = config.DefaultMaxRequestBytes
	}

	addr := cfg.Address()
	srv := &http.Server{
		Addr:         addr,
		Handler:      http.MaxBytesHandler(r, maxBody),
		ReadTimeout:  cfg.Server.ReadTimeout,
		WriteTimeout: cfg.Server.WriteTimeout,
		IdleTimeout:  cfg.Server.IdleTimeout,
	}
	return &Server{cfg: cfg, srv: srv, log: log}
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
