package notification

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestWebhookClientNotify(t *testing.T) {
	t.Parallel()

	const secret = "test-webhook-secret" // #nosec G101 -- deterministic test-only HMAC key.
	requestTime := time.Date(2026, 8, 24, 1, 2, 3, 0, time.UTC)
	eventTime := time.Date(2026, 8, 24, 1, 2, 2, 0, time.UTC)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if got, want := r.Header.Get("Content-Type"), "application/json"; got != want {
			t.Errorf("Content-Type = %q, want %q", got, want)
		}
		if got, want := r.Header.Get("Webhook-Id"), "evt_test"; got != want {
			t.Errorf("Webhook-Id = %q, want %q", got, want)
		}
		if got, want := r.Header.Get("Webhook-Timestamp"), "1787533323"; got != want {
			t.Errorf("Webhook-Timestamp = %q, want %q", got, want)
		}

		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write([]byte("evt_test.1787533323."))
		_, _ = mac.Write(body)
		wantSignature := "v1=" + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
		if got := r.Header.Get("Webhook-Signature"); got != wantSignature {
			t.Errorf("Webhook-Signature = %q, want %q", got, wantSignature)
		}

		var got Event
		if err := json.Unmarshal(body, &got); err != nil {
			t.Errorf("decode event: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if got.Type != ChannelUpdated || !got.Time.Equal(eventTime) || got.Channel.URL != "https://feed.example/api/v1/channels/test" {
			t.Errorf("event = %#v", got)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	client, err := NewWebhookClient(server.URL, secret, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	client.now = func() time.Time { return requestTime }
	client.newEventID = func() string { return "evt_test" }

	err = client.Notify(context.Background(), Event{
		Type:    ChannelUpdated,
		Time:    eventTime,
		Channel: ChannelRef{URL: "https://feed.example/api/v1/channels/test"},
	})
	if err != nil {
		t.Fatalf("Notify: %v", err)
	}
}

func TestWebhookClientNotifyRejectsNon2xx(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, strings.Repeat("x", 10_000), http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client, err := NewWebhookClient(server.URL, "do-not-leak-me", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	err = client.Notify(context.Background(), Event{Type: ChannelAdded, Time: time.Now(), Channel: ChannelRef{URL: "https://feed.example/channel"}})
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("Notify error = %v, want status", err)
	}
	if strings.Contains(err.Error(), "do-not-leak-me") {
		t.Fatalf("Notify error leaked secret: %v", err)
	}
	if len(err.Error()) > 2_000 {
		t.Fatalf("Notify error is unexpectedly large: %d", len(err.Error()))
	}
}

func TestNewWebhookClientValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		endpoint string
		secret   string
	}{
		{name: "missing endpoint", secret: "secret"},
		{name: "invalid endpoint", endpoint: "://bad", secret: "secret"},
		{name: "missing secret", endpoint: "https://example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if _, err := NewWebhookClient(tt.endpoint, tt.secret, http.DefaultClient); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

type stubClient struct {
	err    error
	events *[]Event
}

func (c stubClient) Notify(_ context.Context, event Event) error {
	*c.events = append(*c.events, event)
	return c.err
}

func TestDispatcherContinuesAfterClientFailure(t *testing.T) {
	t.Parallel()

	core, logs := observer.New(zap.ErrorLevel)
	firstEvents := []Event{}
	secondEvents := []Event{}
	dispatcher := NewDispatcher(zap.New(core), time.Second, []NamedClient{
		{Name: "first", Client: stubClient{err: errors.New("unavailable"), events: &firstEvents}},
		{Name: "second", Client: stubClient{events: &secondEvents}},
	})
	event := Event{Type: ChannelDeleted, Time: time.Now(), Channel: ChannelRef{URL: "https://feed.example/channel"}}

	if err := dispatcher.Notify(context.Background(), event); err != nil {
		t.Fatalf("best-effort dispatcher returned error: %v", err)
	}
	if len(firstEvents) != 1 || len(secondEvents) != 1 {
		t.Fatalf("client calls = (%d, %d), want (1, 1)", len(firstEvents), len(secondEvents))
	}
	if logs.Len() != 1 || logs.All()[0].ContextMap()["client"] != "first" {
		t.Fatalf("logs = %#v", logs.All())
	}
}

type blockingClient struct {
	started chan<- struct{}
}

func (c blockingClient) Notify(ctx context.Context, _ Event) error {
	c.started <- struct{}{}
	<-ctx.Done()
	return ctx.Err()
}

func TestDispatcherUsesOneConcurrentDeadline(t *testing.T) {
	t.Parallel()

	core, _ := observer.New(zap.ErrorLevel)
	started := make(chan struct{}, 2)
	dispatcher := NewDispatcher(zap.New(core), 20*time.Millisecond, []NamedClient{
		{Name: "first", Client: blockingClient{started: started}},
		{Name: "second", Client: blockingClient{started: started}},
	})
	done := make(chan struct{})
	go func() {
		_ = dispatcher.Notify(context.Background(), Event{Type: ChannelAdded})
		close(done)
	}()

	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("clients did not start concurrently")
		}
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("dispatcher did not honor aggregate timeout")
	}
}
