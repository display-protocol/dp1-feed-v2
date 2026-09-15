package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	feedlogger "github.com/display-protocol/dp1-feed-v2/internal/logger"
)

type drainingServer struct {
	started         chan struct{}
	shutdownStarted chan struct{}
	releaseShutdown chan struct{}
	listenDone      chan struct{}
	once            sync.Once
}

func (s *drainingServer) ListenAndServe() error {
	close(s.started)
	<-s.listenDone
	return nil
}

func (s *drainingServer) Shutdown(context.Context) error {
	close(s.shutdownStarted)
	<-s.releaseShutdown
	s.once.Do(func() { close(s.listenDone) })
	return nil
}

func TestServeUntilShutdownKeepsLoggerAvailableWhileHandlersDrain(t *testing.T) {
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
	log, shutdownLogger, err := feedlogger.New(feedlogger.Config{
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

	srv := &drainingServer{
		started:         make(chan struct{}),
		shutdownStarted: make(chan struct{}),
		releaseShutdown: make(chan struct{}),
		listenDone:      make(chan struct{}),
	}
	processContext, cancelProcess := context.WithCancel(context.Background())
	serveResult := make(chan error, 1)
	go func() {
		serveResult <- serveUntilShutdown(processContext, srv)
	}()
	<-srv.started
	cancelProcess()
	<-srv.shutdownStarted

	log.Info("in-flight request completed")
	close(srv.releaseShutdown)
	if err := <-serveResult; err != nil {
		t.Fatalf("serveUntilShutdown: %v", err)
	}
	if err := shutdownLogger(context.Background()); err != nil {
		t.Fatalf("logger shutdown: %v", err)
	}

	if !bytes.Contains(stdout.Bytes(), []byte("in-flight request completed")) {
		t.Fatalf("stdout = %q, want in-flight completion", stdout.String())
	}
	var records []map[string]any
	if err := json.Unmarshal(<-requests, &records); err != nil {
		t.Fatalf("decode Cloudflare records: %v", err)
	}
	if len(records) != 1 || records[0]["message"] != "in-flight request completed" {
		t.Fatalf("Cloudflare records = %#v", records)
	}
}
