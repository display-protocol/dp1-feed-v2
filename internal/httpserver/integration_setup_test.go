//go:build integration

package httpserver

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/display-protocol/dp1-go/extension/identity"
	"github.com/display-protocol/dp1-go/playlist"
	"github.com/display-protocol/dp1-go/playlistgroup"
	dp1sign "github.com/display-protocol/dp1-go/sign"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/display-protocol/dp1-feed-v2/internal/config"
	"github.com/display-protocol/dp1-feed-v2/internal/dp1svc"
	"github.com/display-protocol/dp1-feed-v2/internal/executor"
	"github.com/display-protocol/dp1-feed-v2/internal/fetcher"
	"github.com/display-protocol/dp1-feed-v2/internal/store"
	"github.com/display-protocol/dp1-feed-v2/internal/store/pg/pgtest"
)

// Shared harness for the HTTP integration tests in this package: one Postgres container for the
// whole package (TestMain), truncated between tests by the provider's Cleanup.

const integrationSeedHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

var integrationTestProvider store.TestProvider

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	integrationTestProvider, err = pgtest.NewProvider(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "httpserver integration setup: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	integrationTestProvider.Close()
	os.Exit(code)
}

// newIntegrationServer builds a server backed by the shared Postgres provider with extensions
// enabled, and registers the per-test database cleanup. It has no fetcher, so playlist URIs must
// resolve locally; use newIntegrationServerWithFetcher to exercise remote resolution.
func newIntegrationServer(t *testing.T) *Server {
	t.Helper()
	return newIntegrationServerWithFetcher(t, nil)
}

// newIntegrationServerWithFetcher is newIntegrationServer plus an HTTP fetcher, which is what
// splits local from remote ingest: resolveOnePlaylistRef only fetches URIs outside publicBaseURL,
// and only that branch re-validates and re-stores a document the feed did not author.
func newIntegrationServerWithFetcher(t *testing.T, fetch fetcher.Fetcher) *Server {
	t.Helper()
	t.Cleanup(func() {
		integrationTestProvider.Cleanup(t)
	})

	dp1Service, kid := newIntegrationSignerAndKid(t)

	cfg := &config.Config{
		Server: config.ServerConfig{
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 30 * time.Second,
			IdleTimeout:  120 * time.Second,
		},
		Auth:       config.AuthConfig{IntentMaxClockSkew: 5 * time.Minute},
		Logging:    config.LoggingConfig{Debug: true},
		Extensions: config.ExtensionsConfig{Enabled: true},
		Playlist: config.PlaylistConfig{
			SigningKeyHex: integrationSeedHex,
			SigningKid:    kid,
			PublicBaseURL: "http://example.com",
		},
		CORS: config.CORSConfig{},
	}

	gin.SetMode(gin.TestMode)
	exec := executor.New(integrationTestProvider.NewStore(), dp1Service, true, fetch, cfg.Playlist.PublicBaseURL)
	return New(cfg, zap.NewNop(), exec, "test")
}

// newIntegrationSigner returns a signer holding the same key the integration server signs with,
// so a test can produce a document that server will accept as validly signed.
func newIntegrationSigner(t *testing.T) *dp1svc.Service {
	t.Helper()
	svc, _ := newIntegrationSignerAndKid(t)
	return svc
}

func newIntegrationSignerAndKid(t *testing.T) (*dp1svc.Service, string) {
	t.Helper()
	priv, err := dp1svc.Ed25519PrivateKeyFromHex(integrationSeedHex)
	if err != nil {
		t.Fatal(err)
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		t.Fatal("unexpected public key type")
	}
	kid, err := dp1sign.Ed25519DIDKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := dp1svc.New(integrationSeedHex, kid)
	if err != nil {
		t.Fatal(err)
	}
	return svc, kid
}

// newCuratorKeypair returns a fresh Ed25519 curator identity for signed-path integration requests.
// Mutations are authorized by a signature from a key the document declares as curator/publisher, so
// tests generate their own key rather than reusing the feed's (which only ever signs with the feed role).
func newCuratorKeypair(t *testing.T) (ed25519.PrivateKey, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	kid, err := dp1sign.Ed25519DIDKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return priv, kid
}

// signedPlaylistBody curator-signs doc exactly as the feed will rebuild it from the request fields, and
// returns the POST/PUT JSON body (curators + signatures). doc.Curators must include the signer's key.
func signedPlaylistBody(t *testing.T, priv ed25519.PrivateKey, doc playlist.Playlist) map[string]any {
	t.Helper()
	unsigned, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := dp1sign.SignMultiEd25519(unsigned, priv, playlist.RoleCurator, doc.Created)
	if err != nil {
		t.Fatal(err)
	}
	body := map[string]any{
		"dpVersion":  doc.DPVersion,
		"id":         doc.ID,
		"created":    doc.Created,
		"slug":       doc.Slug,
		"title":      doc.Title,
		"curators":   doc.Curators,
		"items":      doc.Items,
		"signatures": []playlist.Signature{sig},
	}
	return body
}

// signedDeleteBody builds a delete-intent signed by priv (a key the stored resource names as owner).
func signedDeleteBody(t *testing.T, priv ed25519.PrivateKey, targetType, id, slug string) map[string]any {
	t.Helper()
	created := time.Now().UTC().Format(time.RFC3339)
	intent := map[string]any{
		"action":  "delete",
		"target":  map[string]any{"type": targetType, "id": id, "slug": slug},
		"created": created,
	}
	unsigned, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := dp1sign.SignMultiEd25519(unsigned, priv, playlist.RoleCurator, created)
	if err != nil {
		t.Fatal(err)
	}
	intent["signatures"] = []playlist.Signature{sig}
	return intent
}

// curatorEntities wraps a single curator kid as the curators[] the document declares.
func curatorEntities(kid string) []identity.Entity {
	return []identity.Entity{{Name: "Curator", Key: kid}}
}

// signedGroupBody curator-signs a playlist-group (curator = kid) as the feed rebuilds it, returning the
// POST/PUT JSON body. uris are the member playlist URIs, preserved verbatim in the signed document.
func signedGroupBody(t *testing.T, priv ed25519.PrivateKey, kid, id, slug, title, created string, uris []string) map[string]any {
	t.Helper()
	doc := playlistgroup.Group{
		ID:        id,
		Slug:      slug,
		Title:     title,
		Playlists: uris,
		Created:   created,
		Curator:   kid,
	}
	unsigned, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := dp1sign.SignMultiEd25519(unsigned, priv, playlist.RoleCurator, created)
	if err != nil {
		t.Fatal(err)
	}
	// No dpVersion: the playlist-group schema (and playlistgroup.Group) does not define it, and the body
	// must be byte-identical to what was signed above.
	return map[string]any{
		"id":         id,
		"created":    created,
		"slug":       slug,
		"title":      title,
		"curator":    kid,
		"playlists":  uris,
		"signatures": []playlist.Signature{sig},
	}
}

func mustDoPlaylistJSON(t *testing.T, srv *Server, method, path string, body any, wantStatus int) playlist.Playlist {
	t.Helper()
	var out playlist.Playlist
	mustDoJSON(t, srv, method, path, body, wantStatus, &out)
	return out
}

func mustDoJSON(t *testing.T, srv *Server, method, path string, body any, wantStatus int, out any) {
	t.Helper()
	raw := mustDoRaw(t, srv, method, path, body, wantStatus)
	if out == nil {
		return
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("%s %s: decode response: %v body=%s", method, path, err, raw)
	}
}

// mustDoRaw returns the undecoded response body, which callers need when the exact bytes matter
// (signature verification, raw-JSON fields such as inlineManifest).
func mustDoRaw(t *testing.T, srv *Server, method, path string, body any, wantStatus int) []byte {
	t.Helper()
	return doRaw(t, srv, method, path, body, wantStatus)
}

// mustDoRawUnauthenticated is retained as an alias of mustDoRaw: with the API key removed, every mutating
// request is authorized the same way — by the signatures[] in its body — so there is no separate path.
func mustDoRawUnauthenticated(t *testing.T, srv *Server, method, path string, body any, wantStatus int) []byte {
	t.Helper()
	return doRaw(t, srv, method, path, body, wantStatus)
}

func doRaw(t *testing.T, srv *Server, method, path string, body any, wantStatus int) []byte {
	t.Helper()
	var payload *bytes.Reader
	if body == nil {
		payload = bytes.NewReader(nil)
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		payload = bytes.NewReader(raw)
	}

	// Authentication is carried in the request body (signatures), not a header. There is no API key.
	req := httptest.NewRequest(method, path, payload)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.engine.ServeHTTP(rec, req)
	if rec.Code != wantStatus {
		t.Fatalf("%s %s: status=%d want %d body=%s", method, path, rec.Code, wantStatus, rec.Body.String())
	}
	return rec.Body.Bytes()
}

// signedReplaceEnvelope builds a PUT body: the signed document plus an owner-signed mutation intent.
//
// The intent carries its own `created` inside the bytes its signature covers, which is what bounds
// replay — the per-signature `ts` cannot, because it sits outside the signing digest. payloadHash ties
// the intent to this exact document, so a captured intent cannot install different content.
func signedReplaceEnvelope(t *testing.T, priv ed25519.PrivateKey, targetType, id, slug string, doc any) map[string]any {
	t.Helper()
	docBytes, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	payloadHash, err := dp1sign.PayloadHashString(docBytes)
	if err != nil {
		t.Fatal(err)
	}
	created := time.Now().UTC().Format(time.RFC3339)
	intent := map[string]any{
		"action":      "replace",
		"target":      map[string]any{"type": targetType, "id": id, "slug": slug},
		"payloadHash": payloadHash,
		"created":     created,
	}
	unsigned, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := dp1sign.SignMultiEd25519(unsigned, priv, playlist.RoleCurator, created)
	if err != nil {
		t.Fatal(err)
	}
	intent["signatures"] = []playlist.Signature{sig}
	return map[string]any{"document": doc, "authorization": intent}
}
