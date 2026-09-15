package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/display-protocol/dp1-feed-v2/internal/config"
	feedlogger "github.com/display-protocol/dp1-feed-v2/internal/logger"
)

func TestRecoveryWritesPanicToStdoutAndCloudflare(t *testing.T) {
	requests := make(chan []byte, 1)
	stream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read Cloudflare request: %v", err)
		}
		requests <- body
		w.WriteHeader(http.StatusAccepted)
	}))
	defer stream.Close()

	var stdout bytes.Buffer
	log, shutdown, err := feedlogger.New(feedlogger.Config{
		Debug:  true,
		Output: &stdout,
		Cloudflare: feedlogger.StreamConfig{
			URL:         stream.URL,
			APIKey:      "send-token",
			Service:     "dp1-feed-v2",
			Environment: "test",
			HTTPClient:  stream.Client(),
		},
	})
	if err != nil {
		t.Fatalf("logger.New: %v", err)
	}

	gin.SetMode(gin.TestMode)
	srv := New(&config.Config{Logging: config.LoggingConfig{Debug: true}}, log, nil, "test")
	srv.engine.GET("/panic-test", func(*gin.Context) {
		panic("database invariant failed")
	})
	recorder := httptest.NewRecorder()
	srv.engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/panic-test", nil))
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("logger shutdown: %v", err)
	}

	if !bytes.Contains(stdout.Bytes(), []byte("http panic recovered")) {
		t.Fatalf("stdout = %q, want recovered panic log", stdout.String())
	}
	var cloudflareBody []byte
	select {
	case cloudflareBody = <-requests:
	case <-time.After(time.Second):
		t.Fatal("Cloudflare panic record was not sent")
	}
	var records []map[string]any
	if err := json.Unmarshal(cloudflareBody, &records); err != nil {
		t.Fatalf("decode Cloudflare records: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("Cloudflare record count = %d, want 1", len(records))
	}
	record := records[0]
	if got := record["level"]; got != "error" {
		t.Errorf("level = %#v, want error", got)
	}
	if got := record["message"]; got != "http panic recovered" {
		t.Errorf("message = %#v", got)
	}
	exception, ok := record["exception"].(map[string]any)
	if !ok {
		t.Fatalf("exception = %#v, want object", record["exception"])
	}
	if got := exception["message"]; got != "database invariant failed" {
		t.Errorf("exception.message = %#v", got)
	}
	if stack, ok := exception["stack"].(string); !ok || stack == "" {
		t.Errorf("exception.stack = %#v, want stack", exception["stack"])
	}
}
