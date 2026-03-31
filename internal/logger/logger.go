// Package logger builds Zap loggers. Sentry is integrated via sentry.Init + gin middleware in httpserver,
// not duplicated here, to keep a single Sentry client lifecycle.
package logger

import (
	"fmt"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Config controls Zap output.
type Config struct {
	Debug bool
}

// New returns a Zap logger: DevelopmentConfig+Debug when cfg.Debug, else ProductionConfig at Info; time encoder is RFC3339.
func New(cfg Config) (*zap.Logger, error) {
	var zc zap.Config
	if cfg.Debug {
		zc = zap.NewDevelopmentConfig()
		zc.Level = zap.NewAtomicLevelAt(zapcore.DebugLevel)
	} else {
		zc = zap.NewProductionConfig()
		zc.Level = zap.NewAtomicLevelAt(zapcore.InfoLevel)
	}
	zc.EncoderConfig.EncodeTime = zapcore.RFC3339TimeEncoder
	log, err := zc.Build()
	if err != nil {
		return nil, fmt.Errorf("zap: %w", err)
	}
	return log, nil
}

// Flush is a no-op placeholder for API symmetry with Sentry flush from main.
func Flush(_ interface{}, _ interface{}) {}
