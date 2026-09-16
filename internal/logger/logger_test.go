package logger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestNewWritesStdoutAndCloudflareSchema(t *testing.T) {
	t.Parallel()

	var (
		mu      sync.Mutex
		request *http.Request
		body    []byte
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		request = r.Clone(r.Context())
		var err error
		body, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	var stdout bytes.Buffer
	log, shutdown, err := New(Config{
		Debug:  true,
		Output: &stdout,
		Cloudflare: StreamConfig{
			URL:           server.URL,
			APIKey:        "send-token",
			Service:       "dp1-feed-v2",
			Environment:   "test",
			HTTPClient:    server.Client(),
			batchSize:     1,
			flushInterval: time.Hour,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	log.Named("http.request").With(zap.String("release", "2026.09.15")).Error("request failed",
		zap.String("event", "request_failed"),
		zap.String("trace_id", "trace-123"),
		zap.String("span_id", "span-123"),
		zap.String("request_id", "req-123"),
		zap.Int("status", http.StatusInternalServerError),
		zap.Any("context", map[string]any{"region": "wnam"}),
		zap.Error(errors.New("database unavailable")),
	)
	if err := log.Sync(); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	if got := stdout.String(); !bytes.Contains([]byte(got), []byte("request failed")) {
		t.Fatalf("stdout = %q, want log message", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if request == nil {
		t.Fatal("Cloudflare request was not sent")
	}
	if got := request.Header.Get("Authorization"); got != "Bearer send-token" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := request.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q", got)
	}

	var records []map[string]any
	if err := json.Unmarshal(body, &records); err != nil {
		t.Fatalf("decode records: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("record count = %d, want 1", len(records))
	}
	record := records[0]
	validateCloudflareRecordAgainstCanonicalSchema(t, record)
	for key, want := range map[string]any{
		"level":       "error",
		"service":     "dp1-feed-v2",
		"environment": "test",
		"message":     "request failed",
		"logger":      "http.request",
		"event":       "request_failed",
		"trace_id":    "trace-123",
		"span_id":     "span-123",
		"request_id":  "req-123",
	} {
		if got := record[key]; got != want {
			t.Errorf("%s = %#v, want %#v", key, got, want)
		}
	}
	if _, err := time.Parse(time.RFC3339Nano, record["timestamp"].(string)); err != nil {
		t.Errorf("timestamp = %q: %v", record["timestamp"], err)
	}
	structured := record["structured"].(map[string]any)
	if got := structured["status"]; got != float64(http.StatusInternalServerError) {
		t.Errorf("structured.status = %#v", got)
	}
	if got := structured["release"]; got != "2026.09.15" {
		t.Errorf("structured.release = %#v", got)
	}
	contextObject := record["context"].(map[string]any)
	if got := contextObject["region"]; got != "wnam" {
		t.Errorf("context.region = %#v", got)
	}
	exception := record["exception"].(map[string]any)
	if got := exception["message"]; got != "database unavailable" {
		t.Errorf("exception.message = %#v", got)
	}
	if _, exists := structured["error"]; exists {
		t.Error("error must be encoded under exception, not structured")
	}
}

func validateCloudflareRecordAgainstCanonicalSchema(t *testing.T, record map[string]any) {
	t.Helper()

	// This fixture is pinned from ff-logging's immutable service_logs_stream contract at commit fb9c4fd.
	rawSchema, err := os.ReadFile("testdata/service-log-schema.json")
	if err != nil {
		t.Fatalf("read canonical schema fixture: %v", err)
	}
	var schema struct {
		Fields []struct {
			Name     string `json:"name"`
			Type     string `json:"type"`
			Required bool   `json:"required"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(rawSchema, &schema); err != nil {
		t.Fatalf("decode canonical schema fixture: %v", err)
	}

	fields := make(map[string]struct {
		typeName string
		required bool
	}, len(schema.Fields))
	for _, field := range schema.Fields {
		fields[field.Name] = struct {
			typeName string
			required bool
		}{typeName: field.Type, required: field.Required}
	}
	for name, field := range fields {
		value, present := record[name]
		if field.required && !present {
			t.Errorf("required schema field %q is absent", name)
			continue
		}
		if !present {
			continue
		}
		switch field.typeName {
		case "timestamp":
			text, ok := value.(string)
			if !ok {
				t.Errorf("schema field %q has type %T, want timestamp string", name, value)
				continue
			}
			if _, err := time.Parse(time.RFC3339Nano, text); err != nil {
				t.Errorf("schema field %q is not an RFC 3339 timestamp: %v", name, err)
			}
		case "string":
			if _, ok := value.(string); !ok {
				t.Errorf("schema field %q has type %T, want string", name, value)
			}
		case "json":
			if _, ok := value.(map[string]any); !ok {
				t.Errorf("schema field %q has type %T, want JSON object", name, value)
			}
		default:
			t.Errorf("canonical schema fixture has unsupported type %q for %q", field.typeName, name)
		}
	}
	for name := range record {
		if _, ok := fields[name]; !ok {
			t.Errorf("record contains field %q outside the canonical schema", name)
		}
	}
}

func TestCloudflareLevelUsesSchemaVocabulary(t *testing.T) {
	t.Parallel()

	tests := []struct {
		level zapcore.Level
		want  string
	}{
		{level: zapcore.DebugLevel, want: "debug"},
		{level: zapcore.InfoLevel, want: "info"},
		{level: zapcore.WarnLevel, want: "warn"},
		{level: zapcore.ErrorLevel, want: "error"},
		{level: zapcore.DPanicLevel, want: "error"},
		{level: zapcore.PanicLevel, want: "fatal"},
		{level: zapcore.FatalLevel, want: "fatal"},
	}
	for _, tt := range tests {
		if got := cloudflareLevel(tt.level); got != tt.want {
			t.Errorf("cloudflareLevel(%s) = %q, want %q", tt.level, got, tt.want)
		}
	}
}

func TestCloudflareRecordHandlesOptionalObjectAndStackFields(t *testing.T) {
	t.Parallel()

	core := &cloudflareCore{service: "dp1-feed-v2", environment: "test"}
	record, err := core.record(zapcore.Entry{
		Level:      zapcore.ErrorLevel,
		Message:    "failed without error field",
		Stack:      "call stack",
		LoggerName: "",
	}, []zapcore.Field{
		zap.String("logger", "worker"),
		zap.String("trace_id", "trace-1"),
		zap.String("span_id", "span-1"),
		zap.String("context", "not-an-object"),
	})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if record.Logger != "worker" || record.TraceID != "trace-1" || record.SpanID != "span-1" {
		t.Fatalf("optional columns = %#v", record)
	}
	if got := record.Structured["context"]; got != "not-an-object" {
		t.Errorf("non-object context = %#v", got)
	}
	if got := record.Structured["stacktrace"]; got != "call stack" {
		t.Errorf("stacktrace = %#v", got)
	}

	exception := map[string]any{"type": "panic", "message": "boom"}
	record, err = core.record(zapcore.Entry{Level: zapcore.ErrorLevel}, []zapcore.Field{
		zap.Any("exception", exception),
	})
	if err != nil {
		t.Fatalf("record explicit exception: %v", err)
	}
	if got := record.Exception; !reflect.DeepEqual(got, exception) {
		t.Errorf("exception = %#v, want %#v", got, exception)
	}
}

func TestCloudflareCoreDropsUnencodableRemoteCopyWithoutWriteError(t *testing.T) {
	t.Parallel()

	sender := &streamSender{dropNotices: make(chan error, 1)}
	core := &cloudflareCore{
		level: zapcore.DebugLevel, service: "dp1-feed-v2", environment: "test", sender: sender,
	}
	err := core.Write(zapcore.Entry{Level: zapcore.InfoLevel, Message: "bad field"}, []zapcore.Field{
		zap.Reflect("unsupported", make(chan int)),
	})
	if err != nil {
		t.Fatalf("Write error = %v, want nil for a dropped remote copy", err)
	}
	if got := sender.droppedRecords.Load(); got != 1 {
		t.Fatalf("dropped records = %d, want 1", got)
	}
	select {
	case dropErr := <-sender.dropNotices:
		if !strings.Contains(dropErr.Error(), "encode Cloudflare log record") {
			t.Fatalf("drop error = %v", dropErr)
		}
	default:
		t.Fatal("drop notice was not queued")
	}
}

func TestCloudflareCoreQueueOverflowDoesNotReachZapErrorOutput(t *testing.T) {
	t.Parallel()

	sender := &streamSender{
		records:       make(chan []byte, 1),
		dropNotices:   make(chan error, 1),
		maxBatchBytes: 1024,
		maxQueueBytes: 1024,
	}
	if err := sender.enqueue([]byte(`{"message":"occupies queue"}`)); err != nil {
		t.Fatalf("fill queue: %v", err)
	}
	core := &cloudflareCore{
		level: zapcore.DebugLevel, service: "dp1-feed-v2", environment: "test", sender: sender,
	}
	var zapErrors bytes.Buffer
	log := zap.New(core, zap.ErrorOutput(zapcore.AddSync(&zapErrors)))

	log.Info("remote copy is dropped")

	if got := zapErrors.String(); got != "" {
		t.Fatalf("Zap error output = %q, want empty", got)
	}
	if got := sender.droppedRecords.Load(); got != 1 {
		t.Fatalf("dropped records = %d, want 1", got)
	}
}

func TestCloudflareCoreFlushesPanicLevelBeforeReturning(t *testing.T) {
	t.Parallel()

	requests := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		requests <- body
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	sender, err := newStreamSender(StreamConfig{
		URL: server.URL, APIKey: "send-token", Service: "dp1-feed-v2", Environment: "test",
		HTTPClient: server.Client(), flushInterval: time.Hour,
	}, io.Discard)
	if err != nil {
		t.Fatalf("newStreamSender: %v", err)
	}
	core := &cloudflareCore{
		level: zapcore.DebugLevel, service: "dp1-feed-v2", environment: "test", sender: sender,
	}
	if err := core.Write(zapcore.Entry{Level: zapcore.PanicLevel, Message: "panic"}, nil); err != nil {
		t.Fatalf("Write: %v", err)
	}
	select {
	case <-requests:
	default:
		t.Fatal("panic record was not flushed synchronously")
	}
	if err := sender.close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestCloudflareDirectZapPanicUsesExceptionSchemaField(t *testing.T) {
	t.Parallel()

	bodies := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		bodies <- body
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	log, shutdown, err := New(Config{
		Output: io.Discard,
		Cloudflare: StreamConfig{
			URL: server.URL, APIKey: "send-token", Service: "dp1-feed-v2", Environment: "test",
			HTTPClient: server.Client(), flushInterval: time.Hour,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var recovered any
	func() {
		defer func() {
			recovered = recover()
		}()
		log.Panic("boom")
	}()
	if recovered == nil {
		t.Fatal("log.Panic did not panic")
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	var records []map[string]any
	if err := json.Unmarshal(<-bodies, &records); err != nil {
		t.Fatalf("decode records: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("record count = %d, want 1", len(records))
	}
	exception, ok := records[0]["exception"].(map[string]any)
	if !ok {
		t.Fatalf("exception = %#v, want object", records[0]["exception"])
	}
	if got := exception["type"]; got != "panic" {
		t.Errorf("exception.type = %#v, want panic", got)
	}
	if got := exception["message"]; got != "boom" {
		t.Errorf("exception.message = %#v, want boom", got)
	}
	if stack, ok := exception["stack"].(string); !ok || stack == "" {
		t.Errorf("exception.stack = %#v, want non-empty string", exception["stack"])
	}
	if structured, ok := records[0]["structured"].(map[string]any); ok {
		if _, exists := structured["stacktrace"]; exists {
			t.Error("panic stack must be encoded under exception, not structured")
		}
	}
}

func TestCloudflareCoreFlushesAcceptedRecordsWhenTerminalRecordCannotEnqueue(t *testing.T) {
	t.Parallel()

	firstRequestStarted := make(chan struct{})
	releaseFirstRequest := make(chan struct{})
	bodies := make(chan []byte, 2)
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		bodies <- body
		if requests.Add(1) == 1 {
			close(firstRequestStarted)
			<-releaseFirstRequest
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	sender, err := newStreamSender(StreamConfig{
		URL: server.URL, APIKey: "send-token", Service: "dp1-feed-v2", Environment: "test",
		HTTPClient: server.Client(), batchSize: 1, queueSize: 1, flushInterval: time.Hour,
	}, io.Discard)
	if err != nil {
		t.Fatalf("newStreamSender: %v", err)
	}
	if err := sender.enqueue([]byte(`{"message":"in flight"}`)); err != nil {
		t.Fatalf("enqueue in-flight record: %v", err)
	}
	<-firstRequestStarted
	if err := sender.enqueue([]byte(`{"message":"accepted backlog"}`)); err != nil {
		t.Fatalf("enqueue accepted backlog: %v", err)
	}

	core := &cloudflareCore{
		level: zapcore.DebugLevel, service: "dp1-feed-v2", environment: "test", sender: sender,
	}
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- core.Write(zapcore.Entry{Level: zapcore.PanicLevel, Message: "queue is full"}, nil)
	}()
	select {
	case err := <-writeDone:
		t.Fatalf("terminal Write returned before the admitted backlog could flush: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseFirstRequest)
	if err := <-writeDone; err != nil {
		t.Fatalf("terminal Write: %v", err)
	}
	if got := sender.droppedRecords.Load(); got != 1 {
		t.Fatalf("dropped records = %d, want terminal record only", got)
	}
	if err := sender.close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}

	firstBody := <-bodies
	secondBody := <-bodies
	if !bytes.Contains(firstBody, []byte("in flight")) {
		t.Fatalf("first request body = %s", firstBody)
	}
	if !bytes.Contains(secondBody, []byte("accepted backlog")) {
		t.Fatalf("second request body = %s", secondBody)
	}
}

func TestCloudflareBatchesRecords(t *testing.T) {
	t.Parallel()

	bodies := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		bodies <- body
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	log, shutdown, err := New(Config{
		Output: &bytes.Buffer{},
		Cloudflare: StreamConfig{
			URL:           server.URL,
			APIKey:        "send-token",
			Service:       "dp1-feed-v2",
			Environment:   "test",
			HTTPClient:    server.Client(),
			batchSize:     100,
			flushInterval: time.Hour,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	log.Info("first")
	log.Warn("second")
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	var records []map[string]any
	if err := json.Unmarshal(<-bodies, &records); err != nil {
		t.Fatalf("decode records: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("record count = %d, want 2", len(records))
	}
}

func TestShutdownDrainsSlowSuccessfulMultipleBatches(t *testing.T) {
	t.Parallel()

	var delivered atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(40 * time.Millisecond)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		var records []json.RawMessage
		if err := json.Unmarshal(body, &records); err != nil {
			t.Errorf("decode request: %v", err)
		}
		delivered.Add(int64(len(records)))
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	log, shutdown, err := New(Config{
		Output: io.Discard,
		Cloudflare: StreamConfig{
			URL: server.URL, APIKey: "send-token", Service: "dp1-feed-v2", Environment: "test",
			HTTPClient: server.Client(), batchSize: 2, queueSize: 8, flushInterval: time.Hour,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := range 7 {
		log.Info("queued before shutdown", zap.Int("index", i))
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := shutdown(ctx); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if got := delivered.Load(); got != 7 {
		t.Fatalf("delivered records = %d, want 7 across multiple slow batches", got)
	}
}

func TestDefaultShutdownTimeoutCoversMaximumAcceptedRequests(t *testing.T) {
	t.Parallel()

	minimum := time.Duration(defaultMaxDrainRequests) * defaultRequestTimeout
	if DefaultShutdownTimeout <= minimum {
		t.Fatalf("DefaultShutdownTimeout = %s, want greater than maximum request time %s", DefaultShutdownTimeout, minimum)
	}
}

func TestStreamSenderMixedCountAndByteBatchingFitsShutdownBound(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		requests.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	sender, err := newStreamSender(StreamConfig{
		URL: server.URL, APIKey: "send-token", Service: "dp1-feed-v2", Environment: "test",
		HTTPClient: server.Client(), flushInterval: time.Hour,
	}, io.Discard)
	if err != nil {
		t.Fatalf("newStreamSender: %v", err)
	}
	for range defaultBatchSize - 1 {
		if err := sender.enqueue([]byte(`{}`)); err != nil {
			t.Fatalf("enqueue partial-batch record: %v", err)
		}
	}
	deadline := time.Now().Add(time.Second)
	for {
		sender.mu.Lock()
		pending := sender.pendingBytes
		sender.mu.Unlock()
		if pending == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("worker did not consume the initial partial batch")
		}
		time.Sleep(time.Millisecond)
	}

	// Together with the 99-record partial batch above, these small records produce eleven count-closed
	// requests and leave twenty records pending. The large/mid/large tail then forces three more requests
	// under next-fit byte batching: partial+large, mid, and large.
	for range 1021 {
		if err := sender.enqueue([]byte(`{}`)); err != nil {
			t.Fatalf("enqueue count-bound record: %v", err)
		}
	}
	sizedRecord := func(size int) []byte {
		const prefix = `{"message":"`
		const suffix = `"}`
		return []byte(prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix)
	}
	for _, size := range []int{3_600_000, 700_000, 3_600_000} {
		if err := sender.enqueue(sizedRecord(size)); err != nil {
			t.Fatalf("enqueue byte-bound record of %d bytes: %v", size, err)
		}
	}
	if err := sender.close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}

	if got := requests.Load(); got != 14 {
		t.Fatalf("delivery requests = %d, want 14 from mixed count/byte fragmentation", got)
	}
	if got := requests.Load(); got > int64(defaultMaxDrainRequests) {
		t.Fatalf("delivery requests = %d, shutdown bound covers %d", got, defaultMaxDrainRequests)
	}
}

func TestCloudflareDeliveryFailureDoesNotSuppressStdout(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	var stdout bytes.Buffer
	log, shutdown, err := New(Config{
		Output: &stdout,
		Cloudflare: StreamConfig{
			URL:           server.URL,
			APIKey:        "send-token",
			Service:       "dp1-feed-v2",
			Environment:   "test",
			HTTPClient:    server.Client(),
			flushInterval: time.Hour,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	log.Info("still local")
	if err := shutdown(context.Background()); err == nil {
		t.Fatal("shutdown error = nil, want Cloudflare status error")
	}
	if got := stdout.String(); !bytes.Contains([]byte(got), []byte("still local")) {
		t.Fatalf("stdout = %q, want local log", got)
	}
}

func TestNewRejectsIncompleteCloudflareConfig(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  StreamConfig
	}{
		{name: "missing API key", cfg: StreamConfig{URL: "https://example.com", Service: "svc", Environment: "test"}},
		{name: "missing service", cfg: StreamConfig{URL: "https://example.com", APIKey: "key", Environment: "test"}},
		{name: "missing environment", cfg: StreamConfig{URL: "https://example.com", APIKey: "key", Service: "svc"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, _, err := New(Config{Output: &bytes.Buffer{}, Cloudflare: tt.cfg}); err == nil {
				t.Fatal("New error = nil")
			}
		})
	}
}

func TestStreamSenderShutdownDrainsAcceptedConcurrentRecords(t *testing.T) {
	t.Parallel()

	var delivered atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		var records []json.RawMessage
		if err := json.Unmarshal(body, &records); err != nil {
			t.Errorf("decode request: %v", err)
		}
		delivered.Add(int64(len(records)))
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	sender, err := newStreamSender(StreamConfig{
		URL:           server.URL,
		APIKey:        "send-token",
		Service:       "dp1-feed-v2",
		Environment:   "test",
		HTTPClient:    server.Client(),
		batchSize:     1000,
		queueSize:     1024,
		flushInterval: time.Hour,
	}, io.Discard)
	if err != nil {
		t.Fatalf("newStreamSender: %v", err)
	}
	if err := sender.enqueue([]byte(`{"message":"before concurrency"}`)); err != nil {
		t.Fatalf("initial enqueue: %v", err)
	}

	var accepted atomic.Int64
	accepted.Store(1)
	var writers sync.WaitGroup
	for range 100 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			if err := sender.enqueue([]byte(`{"message":"concurrent"}`)); err == nil {
				accepted.Add(1)
			}
		}()
	}
	if err := sender.close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	writers.Wait()
	if got, want := delivered.Load(), accepted.Load(); got != want {
		t.Fatalf("delivered = %d, accepted = %d", got, want)
	}
}

func TestStreamSenderDoesNotFollowRedirectsWithAPIKey(t *testing.T) {
	t.Parallel()

	var redirectedRequests atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectedRequests.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()

	sender, err := newStreamSender(StreamConfig{
		URL:           redirect.URL,
		APIKey:        "send-token",
		Service:       "dp1-feed-v2",
		Environment:   "test",
		flushInterval: time.Hour,
	}, io.Discard)
	if err != nil {
		t.Fatalf("newStreamSender: %v", err)
	}
	if err := sender.enqueue([]byte(`{"message":"do not redirect"}`)); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if err := sender.close(context.Background()); err == nil {
		t.Fatal("close error = nil, want redirect status error")
	}
	if got := redirectedRequests.Load(); got != 0 {
		t.Fatalf("redirect target received %d requests, want 0", got)
	}
	if err := sender.enqueue([]byte(`{"message":"after close"}`)); err == nil {
		t.Fatal("enqueue after close error = nil")
	}
	if err := sender.close(context.Background()); err == nil {
		t.Fatal("second close must preserve the delivery error")
	}
	if err := sender.flush(context.Background()); err == nil {
		t.Fatal("flush after close must preserve the delivery error")
	}
}

func TestStreamSenderRejectsRecordsBeforeExceedingMemoryBounds(t *testing.T) {
	t.Parallel()

	sender := &streamSender{
		records:       make(chan []byte, 2),
		maxBatchBytes: 16,
		maxQueueBytes: 10,
	}
	accepted := []byte("123456")
	if err := sender.enqueue(accepted); err != nil {
		t.Fatalf("enqueue accepted record: %v", err)
	}
	if got := sender.pendingBytes; got != int64(len(accepted)) {
		t.Fatalf("pending bytes = %d, want %d", got, len(accepted))
	}
	if err := sender.enqueue([]byte("12345")); err == nil {
		t.Fatal("aggregate byte-bound enqueue error = nil")
	}
	if err := sender.enqueue([]byte("123456789012345")); err == nil {
		t.Fatal("oversized record enqueue error = nil")
	}

	record := <-sender.records
	sender.markDequeued(record)
	if got := sender.pendingBytes; got != 0 {
		t.Fatalf("pending bytes after dequeue = %d, want 0", got)
	}
	if err := sender.enqueue([]byte("1234")); err != nil {
		t.Fatalf("first count-bound enqueue: %v", err)
	}
	if err := sender.enqueue([]byte("5678")); err != nil {
		t.Fatalf("second count-bound enqueue: %v", err)
	}
	if err := sender.enqueue([]byte("9")); err == nil {
		t.Fatal("record-count-bound enqueue error = nil")
	}
}

func TestStreamSenderSplitsBatchAtByteLimit(t *testing.T) {
	t.Parallel()

	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	sender, err := newStreamSender(StreamConfig{
		URL: server.URL, APIKey: "send-token", Service: "dp1-feed-v2", Environment: "test",
		HTTPClient: server.Client(), batchSize: 100, flushInterval: time.Hour,
		maxBatchBytes: 24, maxQueueBytes: 100,
	}, io.Discard)
	if err != nil {
		t.Fatalf("newStreamSender: %v", err)
	}
	for range 2 {
		if err := sender.enqueue([]byte(`{"m":"123456"}`)); err != nil {
			t.Fatalf("enqueue: %v", err)
		}
	}
	if err := sender.close(context.Background()); err != nil {
		t.Fatalf("close: %v", err)
	}
	if got := requests.Load(); got != 2 {
		t.Fatalf("request count = %d, want 2", got)
	}
}
