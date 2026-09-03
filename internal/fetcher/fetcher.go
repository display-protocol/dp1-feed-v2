// Package fetcher performs outbound HTTP GETs to ingest remote playlist JSON (universal playlists).
// Limits are conservative defaults to avoid SSRF abuse and unbounded memory use.
package fetcher

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// ErrBlockedDestination is returned when a playlist URL, or any host it redirects to, resolves to an
// address this feed refuses to contact. Callers classify it as a client error: the requester chose the
// URL, so it is their input that is wrong, not an internal fault.
var ErrBlockedDestination = errors.New("playlist fetch destination is not allowed")

// maxFetchRedirects caps redirect following. Each hop is re-validated by the dial guard, so the cap is
// only about bounding work, not safety.
const maxFetchRedirects = 5

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

// Option configures an HTTPFetcher.
type Option func(*options)

type options struct{ allowPrivate bool }

// AllowPrivateDestinations permits fetching from loopback and other normally-blocked addresses.
//
// This exists for tests and local development, where playlists are served from 127.0.0.1. It must stay
// off in production: group and channel creation is open to any client, so an attacker who can name a URL
// can make the feed issue requests on its behalf. Leaving this enabled turns that into a probe of
// whatever the feed can reach — the loopback interface, link-local metadata endpoints, internal services.
func AllowPrivateDestinations(allow bool) Option {
	return func(o *options) { o.allowPrivate = allow }
}

// NewHTTPFetcher returns a fetcher with timeout and max body size.
//
// The returned client refuses to connect to non-public addresses unless AllowPrivateDestinations is set.
// The check runs in the dialer's Control hook, on the address actually being connected to, rather than on
// the hostname: a name check alone is defeated by a DNS record that points at an internal address (and by
// rebinding between the check and the dial), and it would miss redirects entirely. Guarding the dial
// covers the original request and every redirect hop for free.
func NewHTTPFetcher(timeout time.Duration, maxBodyBytes int64, opts ...Option) *HTTPFetcher {
	var o options
	for _, opt := range opts {
		opt(&o)
	}

	dialer := &net.Dialer{
		Timeout:   timeout,
		KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			if o.allowPrivate {
				return nil
			}
			return checkDialAddress(address)
		},
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = dialer.DialContext

	return &HTTPFetcher{
		client: &http.Client{
			Timeout:   timeout,
			Transport: transport,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= maxFetchRedirects {
					return fmt.Errorf("%w: more than %d redirects", ErrBlockedDestination, maxFetchRedirects)
				}
				// The dial guard vets the address; this only rejects hops to schemes we never fetch.
				return validateFetchURL(req.URL)
			},
		},
		max: maxBodyBytes,
	}
}

// checkDialAddress rejects a resolved "host:port" whose IP is not publicly routable.
func checkDialAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: cannot parse address %q", ErrBlockedDestination, address)
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("%w: %q did not resolve to an IP address", ErrBlockedDestination, host)
	}
	if blockedIP(ip) {
		return fmt.Errorf("%w: %s is not a public address", ErrBlockedDestination, ip)
	}
	return nil
}

// blockedIP reports whether ip is one the feed must not contact. An IPv4-mapped IPv6 address is unwrapped
// first, so ::ffff:127.0.0.1 is classified as the loopback address it really is.
func blockedIP(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	switch {
	case ip.IsLoopback(), // 127.0.0.0/8, ::1
		ip.IsUnspecified(),             // 0.0.0.0, ::
		ip.IsPrivate(),                 // RFC1918, fc00::/7 unique-local
		ip.IsLinkLocalUnicast(),        // 169.254.0.0/16, fe80::/10 (cloud metadata lives here)
		ip.IsLinkLocalMulticast(),      //
		ip.IsInterfaceLocalMulticast(), //
		ip.IsMulticast():
		return true
	}
	// Carrier-grade NAT, 100.64.0.0/10: shared address space, never a legitimate playlist host.
	if v4 := ip.To4(); v4 != nil && v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127 {
		return true
	}
	return false
}

// validateFetchURL rejects a URL the feed will not fetch regardless of where it resolves: a non-HTTP(S)
// scheme (file:, gopher:, …), a missing host, or embedded credentials.
func validateFetchURL(u *url.URL) error {
	if u == nil {
		return fmt.Errorf("%w: missing URL", ErrBlockedDestination)
	}
	if !strings.EqualFold(u.Scheme, "http") && !strings.EqualFold(u.Scheme, "https") {
		return fmt.Errorf("%w: scheme %q is not http(s)", ErrBlockedDestination, u.Scheme)
	}
	if strings.TrimSpace(u.Hostname()) == "" {
		return fmt.Errorf("%w: URL has no host", ErrBlockedDestination)
	}
	if u.User != nil {
		return fmt.Errorf("%w: URL must not embed credentials", ErrBlockedDestination)
	}
	return nil
}

// FetchPlaylist GETs the URI and returns the response body (JSON).
func (f *HTTPFetcher) FetchPlaylist(ctx context.Context, uri string) ([]byte, error) {
	// Enforce client timeout (per-request ctx) and cap body size with LimitReader to bound memory (max+1 triggers error).
	parsed, err := url.Parse(strings.TrimSpace(uri))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrBlockedDestination, err)
	}
	if err := validateFetchURL(parsed); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := f.client.Do(req)
	if err != nil {
		// A blocked address surfaces from the dial guard or CheckRedirect wrapped in *url.Error; keep the
		// sentinel detectable so the HTTP layer answers 400 rather than 500.
		if errors.Is(err, ErrBlockedDestination) {
			return nil, err
		}
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
