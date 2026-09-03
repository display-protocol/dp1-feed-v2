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
	"net/netip"
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
	// Never route these fetches through an environment proxy. The dial guard vets the address this
	// process connects to; with a proxy configured that address is the proxy's, and the proxy — not this
	// process — would then connect to the attacker-supplied destination, reaching private ranges and
	// metadata endpoints the guard exists to refuse. Playlist URLs are untrusted input on an open create
	// route, so they must be fetched directly or not at all.
	transport.Proxy = nil

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

// checkDialAddress rejects a resolved "host:port" whose IP is not globally routable.
func checkDialAddress(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: cannot parse address %q", ErrBlockedDestination, address)
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("%w: %q did not resolve to an IP address", ErrBlockedDestination, host)
	}
	if blockedAddr(addr) {
		return fmt.Errorf("%w: %s is not a globally routable address", ErrBlockedDestination, addr)
	}
	return nil
}

// nonGlobalPrefixes is the set of IANA special-purpose prefixes this feed refuses to contact.
//
// The policy is deny-by-default over a prefix list rather than a handful of net.IP predicates, because
// the predicates only cover the famous ranges: benchmarking (198.18.0.0/15), documentation
// (192.0.2.0/24 and friends), reserved (240.0.0.0/4), "this network" (0.0.0.0/8) and 6to4/NAT64 relays
// are none of loopback, private, link-local or multicast, yet a deployment that routes special-use space
// internally would happily let the feed reach them. Every prefix here is one IANA does not consider
// globally reachable, so no legitimate playlist can be served from it.
//
// NAT64 (64:ff9b::/96) and 6to4 (2002::/16) matter for a subtler reason: both embed an IPv4 address, so
// allowing them would reintroduce every blocked IPv4 range through an IPv6 literal.
var nonGlobalPrefixes = []netip.Prefix{
	// IPv4
	netip.MustParsePrefix("0.0.0.0/8"),       // "this network" (RFC 1122)
	netip.MustParsePrefix("10.0.0.0/8"),      // private (RFC 1918)
	netip.MustParsePrefix("100.64.0.0/10"),   // carrier-grade NAT (RFC 6598)
	netip.MustParsePrefix("127.0.0.0/8"),     // loopback
	netip.MustParsePrefix("169.254.0.0/16"),  // link-local; cloud metadata lives at 169.254.169.254
	netip.MustParsePrefix("172.16.0.0/12"),   // private (RFC 1918)
	netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments (RFC 6890)
	netip.MustParsePrefix("192.0.2.0/24"),    // documentation TEST-NET-1
	netip.MustParsePrefix("192.31.196.0/24"), // AS112-v4
	netip.MustParsePrefix("192.52.193.0/24"), // AMT
	netip.MustParsePrefix("192.88.99.0/24"),  // deprecated 6to4 relay anycast
	netip.MustParsePrefix("192.168.0.0/16"),  // private (RFC 1918)
	netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking (RFC 2544)
	netip.MustParsePrefix("198.51.100.0/24"), // documentation TEST-NET-2
	netip.MustParsePrefix("203.0.113.0/24"),  // documentation TEST-NET-3
	netip.MustParsePrefix("224.0.0.0/4"),     // multicast
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved, incl. 255.255.255.255 broadcast
	// IPv6
	netip.MustParsePrefix("::/128"),         // unspecified
	netip.MustParsePrefix("::1/128"),        // loopback
	netip.MustParsePrefix("64:ff9b::/96"),   // NAT64 well-known (embeds IPv4)
	netip.MustParsePrefix("64:ff9b:1::/48"), // NAT64 local-use (embeds IPv4)
	netip.MustParsePrefix("100::/64"),       // discard-only
	netip.MustParsePrefix("2001::/23"),      // IETF protocol assignments (Teredo, ORCHIDv2, …)
	netip.MustParsePrefix("2001:db8::/32"),  // documentation — outside 2001::/23, so listed separately
	netip.MustParsePrefix("2002::/16"),      // 6to4 (embeds IPv4)
	netip.MustParsePrefix("3fff::/20"),      // documentation (RFC 9637)
	netip.MustParsePrefix("fc00::/7"),       // unique-local
	netip.MustParsePrefix("fe80::/10"),      // link-local
	// Deprecated site-local (RFC 3879). IANA dropped it from the special-purpose registry when it was
	// deprecated, so a list derived from that registry alone misses it — but deployments still route it
	// internally, and fec0::/10 sits just past fe80::/10 rather than inside it.
	netip.MustParsePrefix("fec0::/10"),
	netip.MustParsePrefix("ff00::/8"), // multicast
}

// blockedAddr reports whether addr is one the feed must not contact.
//
// An IPv4-mapped IPv6 address is unmapped first, so ::ffff:127.0.0.1 is judged as the loopback address it
// really is rather than sliding past the IPv4 prefixes. A zone is stripped for the same reason:
// netip.Prefix.Contains never matches a zoned address, so fe80::1%eth0 would otherwise escape fe80::/10.
func blockedAddr(addr netip.Addr) bool {
	if !addr.IsValid() {
		return true
	}
	addr = addr.Unmap().WithZone("")
	for _, prefix := range nonGlobalPrefixes {
		if prefix.Contains(addr) {
			return true
		}
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
