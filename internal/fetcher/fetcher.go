// Package fetcher performs outbound HTTP GETs to ingest remote playlist JSON (universal playlists).
// Limits are conservative defaults to avoid SSRF abuse and unbounded memory use.
package fetcher

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Fetcher retrieves remote playlist JSON (e.g. when a group/channel references a URI not yet stored locally).
type Fetcher interface {
	// FetchPlaylist performs an HTTP GET and returns the response body capped by the fetcher max size.
	FetchPlaylist(ctx context.Context, uri string) ([]byte, error)
}

// HTTPFetcher implements Fetcher with net/http.
type HTTPFetcher struct {
	client *http.Client
	max    int64
}

// NewHTTPFetcher returns a fetcher with timeout and max body size.
func NewHTTPFetcher(timeout time.Duration, maxBodyBytes int64) *HTTPFetcher {
	return &HTTPFetcher{
		client: &http.Client{Timeout: timeout},
		max:    maxBodyBytes,
	}
}

// FetchPlaylist GETs the URI and returns the response body (JSON).
func (f *HTTPFetcher) FetchPlaylist(ctx context.Context, uri string) ([]byte, error) {
	// Enforce client timeout (per-request ctx) and cap body size with LimitReader to bound memory (max+1 triggers error).
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, uri, nil)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	r := io.LimitReader(resp.Body, f.max+1)
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if int64(len(b)) > f.max {
		return nil, fmt.Errorf("body exceeds max %d bytes", f.max)
	}
	return b, nil
}
