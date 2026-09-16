package logger

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap/zapcore"
)

const (
	defaultBatchSize      = 100
	defaultFlushInterval  = time.Second
	defaultQueueSize      = 1024
	defaultMaxBatchBytes  = 4 << 20
	defaultMaxQueueBytes  = 8 << 20
	defaultRequestTimeout = 5 * time.Second
	defaultDropReportRate = time.Minute

	// A close can begin with one request in flight, one partial batch retained by the worker, and a full
	// record queue. Batch closure can be triggered independently by record count or bytes. The next-fit
	// byte bound counts each pending byte at most twice: once in the batch it occupies and once as the
	// record that makes the preceding batch overflow. One byte per record conservatively covers commas.
	defaultMaxPendingRecords = defaultBatchSize - 1 + defaultQueueSize
	defaultMaxPendingBytes   = defaultMaxBatchBytes + defaultMaxQueueBytes + defaultMaxPendingRecords
	defaultMaxCountClosures  = defaultMaxPendingRecords / defaultBatchSize
	defaultMaxByteClosures   = (2*defaultMaxPendingBytes + defaultMaxBatchBytes - 1) / defaultMaxBatchBytes
	defaultMaxDrainRequests  = 1 + defaultMaxCountClosures + defaultMaxByteClosures + 1
)

// DefaultShutdownTimeout gives every record accepted by the default bounded sender one delivery attempt,
// even if each HTTP request consumes its full timeout. The extra request-timeout interval is scheduling
// margin so the final request is not canceled exactly at its own deadline.
const DefaultShutdownTimeout = time.Duration(defaultMaxDrainRequests+1) * defaultRequestTimeout

type cloudflareRecord struct {
	Timestamp   string         `json:"timestamp"`
	Level       string         `json:"level"`
	Service     string         `json:"service"`
	Environment string         `json:"environment"`
	Message     string         `json:"message"`
	Logger      string         `json:"logger,omitempty"`
	Event       string         `json:"event,omitempty"`
	TraceID     string         `json:"trace_id,omitempty"`
	SpanID      string         `json:"span_id,omitempty"`
	RequestID   string         `json:"request_id,omitempty"`
	Structured  map[string]any `json:"structured,omitempty"`
	Context     any            `json:"context,omitempty"`
	Exception   any            `json:"exception,omitempty"`
}

type cloudflareCore struct {
	level       zapcore.LevelEnabler
	service     string
	environment string
	sender      *streamSender
	boundFields []zapcore.Field
}

func newCloudflareCore(level zapcore.LevelEnabler, cfg StreamConfig, sender *streamSender) zapcore.Core {
	return &cloudflareCore{level: level, service: cfg.Service, environment: cfg.Environment, sender: sender}
}

func (c *cloudflareCore) Enabled(level zapcore.Level) bool {
	return c.level.Enabled(level)
}

func (c *cloudflareCore) With(fields []zapcore.Field) zapcore.Core {
	clone := *c
	clone.boundFields = append(append([]zapcore.Field(nil), c.boundFields...), fields...)
	return &clone
}

func (c *cloudflareCore) Check(entry zapcore.Entry, checked *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(entry.Level) {
		return checked.AddCore(entry, c)
	}
	return checked
}

func (c *cloudflareCore) Write(entry zapcore.Entry, fields []zapcore.Field) error {
	allFields := make([]zapcore.Field, 0, len(c.boundFields)+len(fields))
	allFields = append(allFields, c.boundFields...)
	allFields = append(allFields, fields...)
	record, err := c.record(entry, allFields)
	if err != nil {
		c.sender.reportDrop(err)
	} else {
		raw, marshalErr := json.Marshal(record)
		if marshalErr != nil {
			c.sender.reportDrop(fmt.Errorf("encode Cloudflare log record: %w", marshalErr))
		} else if enqueueErr := c.sender.enqueue(raw); enqueueErr != nil {
			c.sender.reportDrop(enqueueErr)
		}
	}

	// Zap's fatal and panic hooks terminate control flow immediately after Core.Write. Flush those entries
	// here because main's deferred shutdown cannot run after os.Exit and must not be relied on for fatal logs.
	if entry.Level >= zapcore.PanicLevel {
		ctx, cancel := context.WithTimeout(context.Background(), defaultRequestTimeout)
		defer cancel()
		return c.sender.flush(ctx)
	}
	return nil
}

func (c *cloudflareCore) Sync() error {
	ctx, cancel := context.WithTimeout(context.Background(), defaultRequestTimeout)
	defer cancel()
	return c.sender.flush(ctx)
}

func (c *cloudflareCore) record(entry zapcore.Entry, fields []zapcore.Field) (cloudflareRecord, error) {
	timestamp := entry.Time
	if timestamp.IsZero() {
		timestamp = time.Now()
	}
	record := cloudflareRecord{
		Timestamp:   timestamp.UTC().Format(time.RFC3339Nano),
		Level:       cloudflareLevel(entry.Level),
		Service:     c.service,
		Environment: c.environment,
		Message:     entry.Message,
		Logger:      entry.LoggerName,
	}

	encoder := zapcore.NewMapObjectEncoder()
	for _, field := range fields {
		if field.Type == zapcore.ErrorType {
			err, ok := field.Interface.(error)
			if ok && err != nil {
				exception := map[string]any{
					"type":    reflect.TypeOf(err).String(),
					"message": err.Error(),
				}
				if entry.Stack != "" {
					exception["stack"] = entry.Stack
				}
				record.Exception = exception
				continue
			}
		}
		field.AddTo(encoder)
	}

	record.Event = takeString(encoder.Fields, "event")
	record.TraceID = takeString(encoder.Fields, "trace_id")
	record.SpanID = takeString(encoder.Fields, "span_id")
	record.RequestID = takeString(encoder.Fields, "request_id")
	if record.Logger == "" {
		record.Logger = takeString(encoder.Fields, "logger")
	}
	if contextValue, ok := encoder.Fields["context"]; ok && isJSONObject(contextValue) {
		record.Context = contextValue
		delete(encoder.Fields, "context")
	}
	if exceptionValue, ok := encoder.Fields["exception"]; ok && record.Exception == nil {
		if isJSONObject(exceptionValue) {
			record.Exception = exceptionValue
			delete(encoder.Fields, "exception")
		}
	}
	if entry.Level >= zapcore.PanicLevel && record.Exception == nil {
		exceptionType := "panic"
		if entry.Level == zapcore.FatalLevel {
			exceptionType = "fatal"
		}
		record.Exception = map[string]any{
			"type":    exceptionType,
			"message": entry.Message,
			"stack":   entry.Stack,
		}
	}
	if entry.Caller.Defined {
		encoder.Fields["caller"] = entry.Caller.TrimmedPath()
	}
	if entry.Stack != "" && record.Exception == nil {
		encoder.Fields["stacktrace"] = entry.Stack
	}
	if len(encoder.Fields) > 0 {
		record.Structured = encoder.Fields
	}
	return record, nil
}

func cloudflareLevel(level zapcore.Level) string {
	switch level {
	case zapcore.DebugLevel:
		return "debug"
	case zapcore.InfoLevel:
		return "info"
	case zapcore.WarnLevel:
		return "warn"
	case zapcore.ErrorLevel, zapcore.DPanicLevel:
		return "error"
	case zapcore.PanicLevel, zapcore.FatalLevel:
		return "fatal"
	default:
		return "error"
	}
}

func takeString(fields map[string]any, key string) string {
	value, ok := fields[key].(string)
	if !ok {
		return ""
	}
	delete(fields, key)
	return value
}

func isJSONObject(value any) bool {
	if value == nil {
		return false
	}
	kind := reflect.TypeOf(value).Kind()
	return kind == reflect.Map || kind == reflect.Struct
}

type flushRequest struct {
	ctx    context.Context
	result chan error
}

type closeRequest struct {
	ctx context.Context
}

// streamSender owns the only delivery goroutine. Producers never wait on the network in request paths;
// the bounded queue makes overload explicit by dropping only the remote copy while Zap still writes the
// same entry to stdout. Shutdown stops admission, drains entries already accepted, and flushes one final
// batch under the caller's context.
type streamSender struct {
	url           string
	apiKey        string
	client        *http.Client
	diagnostic    io.Writer
	records       chan []byte
	flushes       chan flushRequest
	closeRequests chan closeRequest
	dropNotices   chan error
	done          chan struct{}
	batchSize     int
	flushInterval time.Duration
	maxBatchBytes int
	maxQueueBytes int64

	mu             sync.Mutex
	closed         bool
	pendingBytes   int64
	closeErr       error
	droppedRecords atomic.Uint64
}

func newStreamSender(cfg StreamConfig, diagnostic io.Writer) (*streamSender, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{
			Timeout: defaultRequestTimeout,
			// The bearer token is scoped to the configured Stream and must never follow an unexpected redirect.
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}
	batchSize := cfg.batchSize
	if batchSize <= 0 {
		batchSize = defaultBatchSize
	}
	flushInterval := cfg.flushInterval
	if flushInterval <= 0 {
		flushInterval = defaultFlushInterval
	}
	queueSize := cfg.queueSize
	if queueSize <= 0 {
		queueSize = defaultQueueSize
	}
	maxBatchBytes := cfg.maxBatchBytes
	if maxBatchBytes <= 0 {
		maxBatchBytes = defaultMaxBatchBytes
	}
	maxQueueBytes := cfg.maxQueueBytes
	if maxQueueBytes <= 0 {
		maxQueueBytes = defaultMaxQueueBytes
	}
	if diagnostic == nil {
		diagnostic = io.Discard
	}
	sender := &streamSender{
		url:           strings.TrimSpace(cfg.URL),
		apiKey:        strings.TrimSpace(cfg.APIKey),
		client:        client,
		diagnostic:    diagnostic,
		records:       make(chan []byte, queueSize),
		flushes:       make(chan flushRequest),
		closeRequests: make(chan closeRequest, 1),
		dropNotices:   make(chan error, 1),
		done:          make(chan struct{}),
		batchSize:     batchSize,
		flushInterval: flushInterval,
		maxBatchBytes: maxBatchBytes,
		maxQueueBytes: maxQueueBytes,
	}
	go sender.run()
	return sender, nil
}

// reportDrop records local admission failures without writing to stderr or returning an error on the
// caller's log path. The delivery goroutine owns diagnostics so queue pressure cannot add synchronous
// IO to request handling; the single pending notice also coalesces an error storm.
func (s *streamSender) reportDrop(err error) {
	s.droppedRecords.Add(1)
	select {
	case s.dropNotices <- err:
	default:
	}
}

func (s *streamSender) enqueue(record []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return fmt.Errorf("cloudflare log sender is closed")
	}
	// Reject a single oversized record before the channel retains it. The batch limit includes the JSON
	// array brackets, so one record may occupy at most maxBatchBytes-2 bytes.
	if len(record)+2 > s.maxBatchBytes {
		return fmt.Errorf("cloudflare log record is %d bytes, exceeding the %d-byte batch limit", len(record), s.maxBatchBytes)
	}
	if s.pendingBytes+int64(len(record)) > s.maxQueueBytes {
		return fmt.Errorf("cloudflare log queue exceeds its %d-byte limit; remote record dropped", s.maxQueueBytes)
	}
	select {
	case s.records <- record:
		s.pendingBytes += int64(len(record))
		return nil
	default:
		return fmt.Errorf("cloudflare log queue is full; remote record dropped")
	}
}

func (s *streamSender) markDequeued(record []byte) {
	s.mu.Lock()
	s.pendingBytes -= int64(len(record))
	s.mu.Unlock()
}

func (s *streamSender) flush(ctx context.Context) error {
	request := flushRequest{ctx: ctx, result: make(chan error, 1)}
	select {
	case s.flushes <- request:
	case <-s.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.closeErr
	case <-ctx.Done():
		return fmt.Errorf("flush Cloudflare logs: %w", ctx.Err())
	}
	select {
	case err := <-request.result:
		return err
	case <-s.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.closeErr
	case <-ctx.Done():
		return fmt.Errorf("flush Cloudflare logs: %w", ctx.Err())
	}
}

func (s *streamSender) close(ctx context.Context) error {
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		s.closeRequests <- closeRequest{ctx: ctx}
	}
	s.mu.Unlock()

	select {
	case <-s.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.closeErr
	case <-ctx.Done():
		return fmt.Errorf("close Cloudflare log sender: %w", ctx.Err())
	}
}

func (s *streamSender) run() {
	ticker := time.NewTicker(s.flushInterval)
	defer ticker.Stop()

	batch := make([][]byte, 0, s.batchSize)
	batchBytes := 2
	var lastErr error
	var lastDropReport time.Time
	reportDrop := func(err error) {
		// Admission failures can repeat for every request while the queue is saturated. Emit at most one
		// diagnostic per minute; droppedRecords retains the complete in-process count.
		now := time.Now()
		if !lastDropReport.IsZero() && now.Sub(lastDropReport) < defaultDropReportRate {
			return
		}
		lastDropReport = now
		_, _ = fmt.Fprintf(
			s.diagnostic,
			"cloudflare log record dropped (total dropped: %d): %v\n",
			s.droppedRecords.Load(),
			err,
		)
	}
	flush := func(ctx context.Context) error {
		if len(batch) == 0 {
			return nil
		}
		err := s.deliver(ctx, batch, batchBytes)
		batch = batch[:0]
		batchBytes = 2
		if err != nil {
			lastErr = err
			_, _ = fmt.Fprintf(s.diagnostic, "cloudflare log delivery failed: %v\n", err)
		}
		return err
	}
	appendRecord := func(ctx context.Context, record []byte) {
		recordBytes := len(record) + 1
		if len(batch) > 0 && batchBytes+recordBytes > s.maxBatchBytes {
			_ = flush(ctx)
		}
		batch = append(batch, record)
		batchBytes += recordBytes
		if len(batch) >= s.batchSize {
			_ = flush(ctx)
		}
	}

	for {
		select {
		case record := <-s.records:
			s.markDequeued(record)
			appendRecord(context.Background(), record)
		case request := <-s.flushes:
			s.drainRecords(func(record []byte) { appendRecord(request.ctx, record) })
			flushErr := flush(request.ctx)
			if flushErr == nil {
				flushErr = lastErr
			}
			request.result <- flushErr
		case <-ticker.C:
			_ = flush(context.Background())
		case err := <-s.dropNotices:
			reportDrop(err)
		case request := <-s.closeRequests:
			s.drainRecords(func(record []byte) { appendRecord(request.ctx, record) })
			_ = flush(request.ctx)
			s.mu.Lock()
			s.closeErr = lastErr
			s.mu.Unlock()
			close(s.done)
			return
		}
	}
}

func (s *streamSender) drainRecords(appendRecord func([]byte)) {
	for {
		select {
		case record := <-s.records:
			s.markDequeued(record)
			appendRecord(record)
		default:
			return
		}
	}
}

func (s *streamSender) deliver(ctx context.Context, records [][]byte, capacity int) error {
	body := bytes.NewBuffer(make([]byte, 0, capacity))
	body.WriteByte('[')
	for i, record := range records {
		if i > 0 {
			body.WriteByte(',')
		}
		body.Write(record)
	}
	body.WriteByte(']')

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, body)
	if err != nil {
		return fmt.Errorf("build Cloudflare request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+s.apiKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := s.client.Do(request)
	if err != nil {
		return fmt.Errorf("send Cloudflare logs: %w", err)
	}
	_, drainErr := io.Copy(io.Discard, io.LimitReader(response.Body, 4<<10))
	closeErr := response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("send Cloudflare logs: status %s", response.Status)
	}
	if drainErr != nil {
		return fmt.Errorf("read Cloudflare response: %w", drainErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close Cloudflare response: %w", closeErr)
	}
	return nil
}
