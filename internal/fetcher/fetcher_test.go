package fetcher

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
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

	f := NewHTTPFetcher(5*time.Second, 1024, AllowPrivateDestinations(true))
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

	f := NewHTTPFetcher(time.Second, 1024, AllowPrivateDestinations(true))
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

	f := NewHTTPFetcher(time.Second, 4, AllowPrivateDestinations(true))
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

	f := NewHTTPFetcher(30*time.Second, 1024, AllowPrivateDestinations(true))
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
	f := NewHTTPFetcher(time.Second, 1024, AllowPrivateDestinations(true))
	_, err := f.FetchPlaylist(context.Background(), "://not-a-url")
	if err == nil || !errors.Is(err, ErrBlockedDestination) {
		t.Fatalf("got %v", err)
	}
}

// The guard below is the reason group/channel ingest cannot be turned into an SSRF primitive: creation is
// open, so whoever names a playlist URL decides where the feed sends a request.

func TestBlockedIP_classification(t *testing.T) {
	t.Parallel()
	// Deny-by-default: every family IANA treats as not globally reachable, not just the famous ones.
	// The benchmarking, documentation, reserved and relay ranges are the gap an allow-list of
	// loopback/private/link-local predicates leaves open.
	blocked := []struct{ addr, why string }{
		{"127.0.0.1", "loopback"},
		{"::1", "loopback"},
		{"0.0.0.0", "unspecified"},
		{"::", "unspecified"},
		{"0.0.0.1", "this network (0.0.0.0/8)"},
		{"10.1.2.3", "RFC1918"},
		{"172.16.0.1", "RFC1918"},
		{"192.168.1.1", "RFC1918"},
		{"fd00::1", "unique-local"},
		{"169.254.169.254", "link-local cloud metadata"},
		{"fe80::1", "link-local"},
		{"fe80::1%eth0", "link-local with a zone must not escape the prefix check"},
		{"fec0::1", "deprecated site-local: sits past fe80::/10, so fc00::/7 and fe80::/10 both miss it"},
		{"feff::1", "top of deprecated site-local"},
		{"100.64.0.1", "carrier-grade NAT"},
		{"100.127.255.254", "carrier-grade NAT"},
		{"224.0.0.1", "multicast"},
		{"ff02::1", "multicast"},
		{"192.0.0.1", "IETF protocol assignments"},
		{"192.0.2.5", "documentation TEST-NET-1"},
		{"198.51.100.5", "documentation TEST-NET-2"},
		{"203.0.113.5", "documentation TEST-NET-3"},
		{"198.18.0.1", "benchmarking"},
		{"198.19.255.254", "benchmarking"},
		{"192.88.99.1", "deprecated 6to4 relay anycast"},
		{"192.31.196.1", "AS112-v4"},
		{"192.52.193.1", "AMT"},
		{"240.0.0.1", "reserved"},
		{"255.255.255.255", "broadcast"},
		{"2001:db8::1", "documentation"},
		{"2001::1", "Teredo / IETF protocol assignments"},
		{"2002::1", "6to4 embeds IPv4"},
		{"64:ff9b::7f00:1", "NAT64 well-known embeds IPv4"},
		{"64:ff9b:1::1", "NAT64 local-use"},
		{"100::1", "discard-only"},
		{"3fff::1", "documentation (RFC 9637)"},
		{"::ffff:127.0.0.1", "IPv4-mapped loopback must be unwrapped"},
		{"::ffff:192.168.0.1", "IPv4-mapped RFC1918 must be unwrapped"},
		{"::ffff:198.18.0.1", "IPv4-mapped benchmarking must be unwrapped"},
	}
	for _, tc := range blocked {
		addr, err := netip.ParseAddr(tc.addr)
		if err != nil {
			t.Errorf("parse %s: %v", tc.addr, err)
			continue
		}
		if !blockedAddr(addr) {
			t.Errorf("expected %s to be blocked (%s)", tc.addr, tc.why)
		}
	}

	// Genuinely public addresses must still resolve, or the feed cannot ingest anything.
	allowed := []string{
		"8.8.8.8", "1.1.1.1", "93.184.216.34",
		"100.63.255.255", "100.128.0.1", // either side of the CGNAT block
		"2606:2800:220:1:248:1893:25c8:1946",
		"2606:4700:4700::1111",
	}
	for _, s := range allowed {
		addr, err := netip.ParseAddr(s)
		if err != nil {
			t.Errorf("parse %s: %v", s, err)
			continue
		}
		if blockedAddr(addr) {
			t.Errorf("expected %s to be allowed", s)
		}
	}
}

func TestHTTPFetcher_FetchPlaylist_blocksLoopbackByDefault(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"dpVersion":"1.1.0"}`))
	}))
	t.Cleanup(srv.Close)

	f := NewHTTPFetcher(5*time.Second, 1024) // production default: no private destinations
	if _, err := f.FetchPlaylist(context.Background(), srv.URL); !errors.Is(err, ErrBlockedDestination) {
		t.Fatalf("expected blocked destination, got %v", err)
	}
}

func TestHTTPFetcher_FetchPlaylist_rejectsNonHTTPScheme(t *testing.T) {
	t.Parallel()
	f := NewHTTPFetcher(time.Second, 1024, AllowPrivateDestinations(true))
	for _, uri := range []string{"file:///etc/passwd", "gopher://example.com/", "ftp://example.com/x"} {
		if _, err := f.FetchPlaylist(context.Background(), uri); !errors.Is(err, ErrBlockedDestination) {
			t.Errorf("%s: expected blocked destination, got %v", uri, err)
		}
	}
}

func TestHTTPFetcher_FetchPlaylist_rejectsEmbeddedCredentials(t *testing.T) {
	t.Parallel()
	f := NewHTTPFetcher(time.Second, 1024, AllowPrivateDestinations(true))
	if _, err := f.FetchPlaylist(context.Background(), "http://user:pass@example.com/p"); !errors.Is(err, ErrBlockedDestination) {
		t.Fatalf("expected blocked destination, got %v", err)
	}
}

// A public URL that redirects into a private address must still be refused: the dial guard re-runs on
// every hop, which is why validating the original hostname alone would not be enough.
func TestHTTPFetcher_FetchPlaylist_blocksRedirectIntoPrivateAddress(t *testing.T) {
	t.Parallel()
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"secret":true}`))
	}))
	t.Cleanup(internal.Close)

	// The redirector itself is on loopback, so allow private to reach it, then assert the *hop* is judged
	// on its own: with the guard off entirely the fetch would succeed, so the interesting case is that a
	// production fetcher refuses both.
	prod := NewHTTPFetcher(5*time.Second, 1024)
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL, http.StatusFound)
	}))
	t.Cleanup(redirector.Close)
	if _, err := prod.FetchPlaylist(context.Background(), redirector.URL); !errors.Is(err, ErrBlockedDestination) {
		t.Fatalf("expected blocked destination, got %v", err)
	}
}

func TestHTTPFetcher_FetchPlaylist_capsRedirects(t *testing.T) {
	t.Parallel()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/next", http.StatusFound) // endless
	}))
	t.Cleanup(srv.Close)

	f := NewHTTPFetcher(5*time.Second, 1024, AllowPrivateDestinations(true))
	if _, err := f.FetchPlaylist(context.Background(), srv.URL); err == nil {
		t.Fatal("expected redirect cap error")
	}
}

// A proxy performs the request on this process's behalf, so the dial guard — which only ever sees the
// address this process connects to — would vet the proxy and never the attacker-supplied destination.
// http.DefaultTransport carries ProxyFromEnvironment, so cloning it silently reopens the SSRF path on any
// deployment that sets HTTP_PROXY; this pins the transport to direct dialing.
func TestNewHTTPFetcher_neverUsesAnEnvironmentProxy(t *testing.T) {
	var proxyHits atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		proxyHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("ALL_PROXY", proxy.URL)

	f := NewHTTPFetcher(2*time.Second, 1<<20)

	tr, ok := f.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", f.client.Transport)
	}
	if tr.Proxy != nil {
		t.Fatal("fetcher transport must not consult a proxy: the proxy would reach the destination for us, bypassing the dial guard")
	}

	// Behaviorally: the metadata endpoint is refused by the guard rather than forwarded to the proxy.
	if _, err := f.FetchPlaylist(context.Background(), "http://169.254.169.254/latest/meta-data"); !errors.Is(err, ErrBlockedDestination) {
		t.Fatalf("FetchPlaylist error = %v, want ErrBlockedDestination", err)
	}
	if n := proxyHits.Load(); n != 0 {
		t.Fatalf("proxy received %d requests; untrusted playlist fetches must never be proxied", n)
	}
}
