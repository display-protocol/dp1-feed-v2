package httpserver

import (
	"crypto/subtle"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/display-protocol/dp1-feed-v2/internal/config"
	"github.com/display-protocol/dp1-feed-v2/internal/publisherauth"
)

const authHeader = "Authorization"

type authPrincipalKind string

const (
	principalOperator  authPrincipalKind = "operator"
	principalPublisher authPrincipalKind = "publisher"
)

const authPrincipalContextKey = "auth_principal"

type authPrincipal struct {
	Kind         authPrincipalKind
	Name         string
	PublisherKey string
	AccountID    string
	ProofCount   int
}

// WriteAuth requires Authorization: Bearer <secret> for mutating/internal routes.
// Operator API key retains full access; publisher tokens authenticate a specific publisher identity.
func WriteAuth(cfg *config.Config, authService publisherauth.Service, log *zap.Logger) gin.HandlerFunc {
	operatorWant := []byte("Bearer " + cfg.Auth.APIKey)
	return func(c *gin.Context) {
		got := []byte(c.GetHeader(authHeader))
		if len(got) == len(operatorWant) && subtle.ConstantTimeCompare(got, operatorWant) == 1 {
			c.Set(authPrincipalContextKey, authPrincipal{Kind: principalOperator, Name: "operator"})
			c.Next()
			return
		}

		if authService != nil && cfg != nil {
			sessionToken, err := c.Cookie(cfg.PublisherAuth.SessionCookieName)
			if err == nil && strings.TrimSpace(sessionToken) != "" {
				principal, lookupErr := authService.LookupSession(c.Request.Context(), sessionToken)
				if lookupErr == nil && principal != nil {
					c.Set(authPrincipalContextKey, authPrincipal{
						Kind:         principalPublisher,
						Name:         principal.DisplayName,
						PublisherKey: principal.PublisherKey,
						AccountID:    principal.AccountID.String(),
						ProofCount:   principal.ProofCount,
					})
					c.Next()
					return
				}
			}
		}

		token := strings.TrimSpace(strings.TrimPrefix(string(got), "Bearer "))
		if token != "" && len(got) > len("Bearer ") {
			for _, entry := range cfg.Auth.PublisherTokens {
				want := []byte("Bearer " + entry.Token)
				if len(got) == len(want) && subtle.ConstantTimeCompare(got, want) == 1 {
					c.Set(authPrincipalContextKey, authPrincipal{
						Kind:         principalPublisher,
						Name:         entry.Name,
						PublisherKey: strings.TrimSpace(entry.PublisherKey),
						ProofCount:   1,
					})
					c.Next()
					return
				}
			}
		}

		log.Warn("unauthorized", zap.String("path", c.Request.URL.Path))
		c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorResponse{Error: "unauthorized", Message: "invalid or missing API key"})
	}
}

func currentPrincipal(c *gin.Context) *authPrincipal {
	if c == nil {
		return nil
	}
	raw, ok := c.Get(authPrincipalContextKey)
	if !ok {
		return nil
	}
	principal, ok := raw.(authPrincipal)
	if !ok {
		return nil
	}
	return &principal
}

func isPublisherPrincipal(c *gin.Context) bool {
	principal := currentPrincipal(c)
	return principal != nil && principal.Kind == principalPublisher
}

func isOperatorPrincipal(c *gin.Context) bool {
	principal := currentPrincipal(c)
	return principal != nil && principal.Kind == principalOperator
}

func requireOperator(c *gin.Context) bool {
	principal := currentPrincipal(c)
	if principal == nil {
		return true
	}
	if principal.Kind == principalOperator {
		return true
	}
	c.AbortWithStatusJSON(http.StatusForbidden, ErrorResponse{Error: "forbidden", Message: "operator credentials required"})
	return false
}

func requirePublisherPrincipal(c *gin.Context) (*authPrincipal, bool) {
	principal := currentPrincipal(c)
	if principal == nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorResponse{Error: "unauthorized", Message: "missing auth principal"})
		return nil, false
	}
	if principal.Kind != principalPublisher {
		c.AbortWithStatusJSON(http.StatusForbidden, ErrorResponse{Error: "forbidden", Message: "publisher credentials required"})
		return nil, false
	}
	if strings.TrimSpace(principal.PublisherKey) == "" {
		c.AbortWithStatusJSON(http.StatusForbidden, ErrorResponse{Error: "forbidden", Message: "publisher identity missing"})
		return nil, false
	}
	return principal, true
}

func writeForbidden(c *gin.Context, message string) {
	if c == nil {
		return
	}
	c.AbortWithStatusJSON(http.StatusForbidden, ErrorResponse{Error: "forbidden", Message: message})
}

func writeMappedError(c *gin.Context, err error) {
	if c == nil || err == nil {
		return
	}
	st, code, msg := mapExecutorError(err)
	c.AbortWithStatusJSON(st, ErrorResponse{Error: code, Message: msg})
}

// ZapLogger emits basic request logs (method, path, status, latency).
func ZapLogger(log *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		log.Info("http",
			zap.String("method", c.Request.Method),
			zap.String("path", c.Request.URL.Path),
			zap.Int("status", c.Writer.Status()),
			zap.Duration("latency", time.Since(start)),
		)
	}
}
