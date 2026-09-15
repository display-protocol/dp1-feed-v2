//go:build integration

package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/display-protocol/dp1-go/extension/channels"
	"github.com/display-protocol/dp1-go/playlist"

	"github.com/display-protocol/dp1-feed-v2/internal/executor"
	"github.com/display-protocol/dp1-feed-v2/internal/notification"
)

// recordingClient captures channel events delivered by the executor; guarded because a playlist write
// fans deliveries out concurrently.
type recordingClient struct {
	mu     sync.Mutex
	events []notification.Event
}

func (c *recordingClient) Notify(_ context.Context, ev notification.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, ev)
	return nil
}

func (c *recordingClient) drain() []notification.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.events
	c.events = nil
	return out
}

// doRequest is doRaw returning the whole recorder, for tests that read response headers.
func doRequest(t *testing.T, srv *Server, method, path string, body any, headers map[string]string, wantStatus int) *httptest.ResponseRecorder {
	t.Helper()
	payload := bytes.NewReader(nil)
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		payload = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, payload)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	srv.engine.ServeHTTP(rec, req)
	if rec.Code != wantStatus {
		t.Fatalf("%s %s: status=%d want %d body=%s", method, path, rec.Code, wantStatus, rec.Body.String())
	}
	return rec
}

// TestIntegration_MemberPlaylistWriteIsObservableAtChannel is the end-to-end statement of issue #25:
// through the real routes, executor, signer and Postgres, replacing or deleting a playlist that a
// channel lists (a) changes the channel's ETag while its served bytes stay identical, and (b) emits one
// channel.updated per listing channel. A write to a playlist no channel lists does neither.
func TestIntegration_MemberPlaylistWriteIsObservableAtChannel(t *testing.T) {
	events := &recordingClient{}
	srv := newIntegrationServerWithOptions(t, nil, []executor.Option{executor.WithNotificationClient(events)})

	curator, curatorKid := newSigner(t, playlist.RoleCurator)
	type member struct {
		id   uuid.UUID
		slug string
	}
	members := []member{
		{uuid.MustParse("1a1a1a1a-1111-4333-8444-555555555555"), "member-one"},
		{uuid.MustParse("1a1a1a1a-2222-4333-8444-555555555555"), "member-two"},
		{uuid.MustParse("1a1a1a1a-3333-4333-8444-555555555555"), "bystander"},
	}
	playlistDoc := func(m member, title string) []byte {
		return []byte(`{"dpVersion":"1.1.0","id":"` + m.id.String() + `","slug":"` + m.slug + `","title":"` + title + `",` +
			`"created":"2026-01-02T03:04:05Z","curators":[{"name":"Curator","key":"` + curatorKid + `"}],` +
			`"items":[{"id":"` + uuid.NewSHA1(m.id, []byte(title)).String() + `","source":"https://cdn.example.com/` + m.slug + `.html"}]}`)
	}
	for _, m := range members {
		mustDoRaw(t, srv, http.MethodPost, "/api/v1/playlists", json.RawMessage(signWithAll(t, playlistDoc(m, "v1"), curator)), http.StatusCreated)
	}

	publisher, publisherKid := newSigner(t, channels.RolePublisher)
	chID := uuid.MustParse("2b2b2b2b-1111-4333-8444-555555555555")
	const chSlug = "member-signal"
	channelDoc := []byte(`{"id":"` + chID.String() + `","slug":"` + chSlug + `","title":"Member signal","version":"1.0.0",` +
		`"created":"2026-01-02T03:04:05Z","publisher":{"name":"Gallery","key":"` + publisherKid + `"},` +
		`"playlists":["http://example.com/api/v1/playlists/member-one","http://example.com/api/v1/playlists/member-two"]}`)
	mustDoRaw(t, srv, http.MethodPost, "/api/v1/channels", json.RawMessage(signWithAll(t, channelDoc, publisher)), http.StatusCreated)
	wantURL := "http://example.com/api/v1/channels/" + chID.String()
	if got := events.drain(); len(got) != 1 || got[0].Type != notification.ChannelAdded {
		t.Fatalf("events after channel create = %#v, want one channel.added", got)
	}

	channelPath := "/api/v1/channels/" + chID.String()
	first := doRequest(t, srv, http.MethodGet, channelPath, nil, nil, http.StatusOK)
	etag0 := first.Header().Get("ETag")
	if etag0 == "" {
		t.Fatal("channel GET carries no ETag")
	}
	body0 := first.Body.Bytes()
	doRequest(t, srv, http.MethodGet, channelPath, nil, map[string]string{"If-None-Match": etag0}, http.StatusNotModified)

	// (a) Replace a member: the channel document is byte-identical, its tag is not, and the stale tag no
	// longer yields 304. (b) Exactly one channel.updated for the one channel listing it.
	m0 := members[0]
	mustDoRaw(t, srv, http.MethodPut, "/api/v1/playlists/"+m0.slug,
		signedReplaceEnvelope(t, curator.priv, "playlist", m0.id.String(), m0.slug, json.RawMessage(signWithAll(t, playlistDoc(m0, "v2"), curator))), http.StatusOK)
	afterReplace := doRequest(t, srv, http.MethodGet, channelPath, nil, map[string]string{"If-None-Match": etag0}, http.StatusOK)
	etag1 := afterReplace.Header().Get("ETag")
	if etag1 == etag0 {
		t.Fatal("channel ETag unchanged after a member playlist was replaced")
	}
	if !bytes.Equal(afterReplace.Body.Bytes(), body0) {
		t.Fatalf("channel body changed on a member replace:\n%s\n%s", body0, afterReplace.Body.Bytes())
	}
	if got := events.drain(); len(got) != 1 || got[0].Type != notification.ChannelUpdated || got[0].Channel.URL != wantURL {
		t.Fatalf("events after member replace = %#v, want one channel.updated for %s", got, wantURL)
	}

	// A playlist no channel lists: nothing observable at the channel.
	by := members[2]
	mustDoRaw(t, srv, http.MethodPut, "/api/v1/playlists/"+by.slug,
		signedReplaceEnvelope(t, curator.priv, "playlist", by.id.String(), by.slug, json.RawMessage(signWithAll(t, playlistDoc(by, "v2"), curator))), http.StatusOK)
	doRequest(t, srv, http.MethodGet, channelPath, nil, map[string]string{"If-None-Match": etag1}, http.StatusNotModified)
	if got := events.drain(); len(got) != 0 {
		t.Fatalf("events after a non-member replace = %#v, want none", got)
	}

	// Delete a member: same two signals again.
	m1 := members[1]
	mustDoRaw(t, srv, http.MethodDelete, "/api/v1/playlists/"+m1.slug, signedDeleteBody(t, curator.priv, "playlist", m1.id.String(), m1.slug), http.StatusNoContent)
	afterDelete := doRequest(t, srv, http.MethodGet, channelPath, nil, map[string]string{"If-None-Match": etag1}, http.StatusOK)
	if etag2 := afterDelete.Header().Get("ETag"); etag2 == etag1 || etag2 == etag0 {
		t.Fatalf("channel ETag after a member delete = %q, want a fresh value (had %q, %q)", etag2, etag0, etag1)
	}
	if !bytes.Equal(afterDelete.Body.Bytes(), body0) {
		t.Fatal("channel body changed on a member delete; the signed document must be served untouched")
	}
	if got := events.drain(); len(got) != 1 || got[0].Type != notification.ChannelUpdated || got[0].Channel.URL != wantURL {
		t.Fatalf("events after member delete = %#v, want one channel.updated for %s", got, wantURL)
	}
}
