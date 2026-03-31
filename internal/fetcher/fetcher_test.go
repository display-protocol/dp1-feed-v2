package fetcher

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPFetcher_FetchPlaylist_ok(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method %s", r.Method)
		}
		if accept := r.Header.Get("Accept"); accept != "application/json" {
			t.Errorf("Accept header: %q", accept)
		}
		_, _ = w.Write([]byte(`{"dpVersion":"1.1.0"}`))
	}))
	t.Cleanup(srv.Close)

	f := NewHTTPFetcher(5*time.Second, 1024)
	body, err := f.FetchPlaylist(context.Background(), srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != `{"dpVersion":"1.1.0"}` {
		t.Fatalf("body %q", body)
	}
}

func TestHTTPFetcher_FetchPlaylist_nonOK(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)

	f := NewHTTPFetcher(time.Second, 1024)
	_, err := f.FetchPlaylist(context.Background(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "unexpected status 404") {
		t.Fatalf("got %v", err)
	}
}

func TestHTTPFetcher_FetchPlaylist_bodyExceedsMax(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("0123456789"))
	}))
	t.Cleanup(srv.Close)

	f := NewHTTPFetcher(time.Second, 4)
	_, err := f.FetchPlaylist(context.Background(), srv.URL)
	if err == nil || !strings.Contains(err.Error(), "body exceeds max 4") {
		t.Fatalf("got %v", err)
	}
}

func TestHTTPFetcher_FetchPlaylist_contextCanceled(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	t.Cleanup(srv.Close)

	f := NewHTTPFetcher(30*time.Second, 1024)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := f.FetchPlaylist(ctx, srv.URL)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "context canceled") {
		t.Fatalf("expected cancel error, got %v", err)
	}
}

func TestHTTPFetcher_FetchPlaylist_invalidURL(t *testing.T) {
	t.Parallel()
	f := NewHTTPFetcher(time.Second, 1024)
	_, err := f.FetchPlaylist(context.Background(), "://not-a-url")
	if err == nil || !strings.Contains(err.Error(), "request") {
		t.Fatalf("got %v", err)
	}
}
