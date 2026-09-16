// Package logger builds the process logger and owns optional Cloudflare Stream delivery.
package logger

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// Config controls local and remote log output.
type Config struct {
	Debug      bool
	Output     io.Writer
	Cloudflare StreamConfig
}

// StreamConfig identifies the authenticated Cloudflare Pipeline Stream producer.
type StreamConfig struct {
	URL         string
	APIKey      string
	Service     string
	Environment string
	HTTPClient  *http.Client

	// These controls are intentionally package-private. Production uses conservative bounded defaults;
	// tests shorten flush timing without widening the application's configuration surface.
	batchSize     int
	flushInterval time.Duration
	queueSize     int
	maxBatchBytes int
	maxQueueBytes int64
}

// ShutdownFunc flushes accepted remote records and releases logger-owned resources.
type ShutdownFunc func(context.Context) error

// New returns a Zap logger that always writes to stdout and, when configured, also batches schema-valid
// records to a Cloudflare Pipeline Stream. Call shutdown before process exit so queued records are flushed.
func New(cfg Config) (*zap.Logger, ShutdownFunc, error) {
	level := zapcore.InfoLevel
	if cfg.Debug {
		level = zapcore.DebugLevel
	}

	output := cfg.Output
	if output == nil {
		output = os.Stdout
	}
	stdout := zapcore.NewCore(stdoutEncoder(cfg.Debug), zapcore.AddSync(output), level)
	cores := []zapcore.Core{stdout}

	var stream *streamSender
	if cfg.Cloudflare.configured() {
		var err error
		stream, err = newStreamSender(cfg.Cloudflare, os.Stderr)
		if err != nil {
			return nil, nil, err
		}
	}
	if stream != nil {
		cores = append(cores, newCloudflareCore(level, cfg.Cloudflare, stream))
	}

	core := zapcore.NewTee(cores...)
	if !cfg.Debug {
		// Match zap.NewProductionConfig's sampling behavior while applying it equally to both outputs.
		core = zapcore.NewSamplerWithOptions(core, time.Second, 100, 100)
	}
	stacktraceLevel := zapcore.ErrorLevel
	options := []zap.Option{
		zap.ErrorOutput(zapcore.AddSync(os.Stderr)),
		zap.AddCaller(),
	}
	if cfg.Debug {
		options = append(options, zap.Development())
		stacktraceLevel = zapcore.WarnLevel
	}
	options = append(options, zap.AddStacktrace(stacktraceLevel))
	log := zap.New(core, options...)
	shutdown := func(ctx context.Context) error {
		var streamErr error
		if stream != nil {
			streamErr = stream.close(ctx)
		}
		// Stdout writes synchronously. Sync is best-effort because terminals and pipes commonly reject fsync;
		// remote flush errors remain actionable and are returned to the process owner.
		_ = stdout.Sync()
		return streamErr
	}
	return log, shutdown, nil
}

func stdoutEncoder(debug bool) zapcore.Encoder {
	var encoderConfig zapcore.EncoderConfig
	if debug {
		encoderConfig = zap.NewDevelopmentEncoderConfig()
	} else {
		encoderConfig = zap.NewProductionEncoderConfig()
	}
	encoderConfig.EncodeTime = zapcore.RFC3339TimeEncoder
	if debug {
		return zapcore.NewConsoleEncoder(encoderConfig)
	}
	return zapcore.NewJSONEncoder(encoderConfig)
}

func (cfg StreamConfig) configured() bool {
	return strings.TrimSpace(cfg.URL) != "" || strings.TrimSpace(cfg.APIKey) != ""
}

func (cfg StreamConfig) validate() error {
	if strings.TrimSpace(cfg.URL) == "" {
		return fmt.Errorf("cloudflare stream URL is required")
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		return fmt.Errorf("cloudflare API key is required")
	}
	if strings.TrimSpace(cfg.Service) == "" {
		return fmt.Errorf("log service is required")
	}
	if strings.TrimSpace(cfg.Environment) == "" {
		return fmt.Errorf("log environment is required")
	}
	return nil
}
