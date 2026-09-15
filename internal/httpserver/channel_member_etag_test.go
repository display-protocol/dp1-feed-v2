package httpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/display-protocol/dp1-go/extension/channels"
	"github.com/display-protocol/dp1-go/playlist"

	"github.com/display-protocol/dp1-feed-v2/internal/config"
	"github.com/display-protocol/dp1-feed-v2/internal/mocks"
	"github.com/display-protocol/dp1-feed-v2/internal/models"
	"github.com/display-protocol/dp1-feed-v2/internal/store"
)

// GET /channels/{id} folds the store's MembersDigest into the ETag so a member playlist edit — which
// leaves the channel bytes untouched — still changes the tag (issue #25). These tests pin the tag
// construction and the handler wiring; the store side is covered by the integration suite.

func TestStrongETagFromJSONBytesAndRevision_distinctFromBytesOnly(t *testing.T) {
	t.Parallel()
	b := []byte(`{"id":"x","playlists":["a"]}`)
	plain := strongETagFromJSONBytes(b)
	empty := strongETagFromJSONBytesAndRevision(b, "")
	r1 := strongETagFromJSONBytesAndRevision(b, "digest-1")
	r2 := strongETagFromJSONBytesAndRevision(b, "digest-2")

	assert.NotEqual(t, plain, empty, "a revision-bearing tag must never collide with the bytes-only tag")
	assert.NotEqual(t, empty, r1)
	assert.NotEqual(t, r1, r2)
	assert.Equal(t, r1, strongETagFromJSONBytesAndRevision(b, "digest-1"), "deterministic")
	assert.Len(t, r1, 2+64)
}

// Pins the exact framing (bytes ‖ 0x00 ‖ revision) so the separator cannot drift silently; every
// consumer holding a cached tag would see a spurious change if it did.
func TestStrongETagFromJSONBytesAndRevision_knownVector(t *testing.T) {
	t.Parallel()
	sum := sha256.Sum256([]byte("{}\x00x@1"))
	want := `"` + hex.EncodeToString(sum[:]) + `"`
	assert.Equal(t, want, strongETagFromJSONBytesAndRevision([]byte("{}"), "x@1"))
}

func getChannelWith(t *testing.T, h *Handler, ifNoneMatch string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/channels/ch", nil)
	if ifNoneMatch != "" {
		req.Header.Set("If-None-Match", ifNoneMatch)
	}
	c.Request = req
	c.Params = gin.Params{{Key: "id", Value: "ch"}}
	h.GetChannel(c)
	return w
}

func TestGetChannel_ETagTracksMembersDigest(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	doc := channels.Channel{ID: "ch", Slug: "ch", Title: "Same bytes", Version: "1.0.0", Playlists: []string{"https://feed/api/v1/playlists/a"}}
	before := channelRecPtr(doc)
	before.MembersDigest = "aaaa"
	after := channelRecPtr(doc)
	after.MembersDigest = "bbbb"
	require.Equal(t, before.Raw, after.Raw, "fixture: the channel document itself must not change")

	mockExec := mocks.NewMockExecutor(ctrl)
	gomock.InOrder(
		mockExec.EXPECT().GetChannel(gomock.Any(), "ch").Return(before, nil),
		mockExec.EXPECT().GetChannel(gomock.Any(), "ch").Return(after, nil),
	)
	h := &Handler{Exec: mockExec, Log: zaptest.NewLogger(t)}

	w1 := getChannelWith(t, h, "")
	require.Equal(t, http.StatusOK, w1.Code)
	etag1 := w1.Header().Get("ETag")
	require.NotEmpty(t, etag1)

	// A member changed underneath: the same document is served again with a new tag, and the stale
	// If-None-Match does not short-circuit to 304 — that 200 is the client's signal to re-check members.
	w2 := getChannelWith(t, h, etag1)
	assert.Equal(t, http.StatusOK, w2.Code)
	assert.Equal(t, w1.Body.Bytes(), w2.Body.Bytes(), "body is byte-identical; only the tag moved")
	assert.NotEqual(t, etag1, w2.Header().Get("ETag"))
}

func TestGetChannel_IfNoneMatchNotModified(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	rec := channelRecPtr(channels.Channel{ID: "ch", Slug: "ch", Title: "Cached", Version: "1.0.0"})
	rec.MembersDigest = "cccc"
	mockExec := mocks.NewMockExecutor(ctrl)
	mockExec.EXPECT().GetChannel(gomock.Any(), "ch").Return(rec, nil).Times(2)
	h := &Handler{Exec: mockExec, Log: zaptest.NewLogger(t)}

	w1 := getChannelWith(t, h, "")
	require.Equal(t, http.StatusOK, w1.Code)
	etag := w1.Header().Get("ETag")
	require.NotEmpty(t, etag)

	w2 := getChannelWith(t, h, etag)
	assert.Equal(t, http.StatusNotModified, w2.Code)
	assert.Empty(t, w2.Body.Bytes())
	assert.Equal(t, etag, w2.Header().Get("ETag"))
}

// Playlist replace and delete emit channel notifications, and the executor refuses a notified mutation
// without a request deadline, so both routes must carry one. Each case pins one route by observing the
// context the handler hands the executor; a route missing from this table is exactly the omission the
// routes.go comment describes happening twice before.
func TestRegisterRoutes_playlistMutationsCarryDeadline(t *testing.T) {
	setGinTestMode()
	const id = "11111111-1111-1111-1111-111111111111"

	cases := []struct {
		name       string
		method     string
		body       io.Reader
		wantStatus int
		expect     func(m *mocks.MockExecutor, saw chan<- bool)
	}{
		{
			name:   "PUT /playlists/{id}",
			method: http.MethodPut,
			body: bytes.NewReader(putEnvelope(models.PlaylistReplaceRequest{
				DPVersion:  "1.1.0",
				Title:      "Replaced",
				Items:      []playlist.PlaylistItem{{ID: "item1"}},
				Signatures: []playlist.Signature{{Alg: "ed25519", Kid: "did:key:test", Sig: "sig"}},
			}, models.IntentTargetPlaylist, id, "slug")),
			wantStatus: http.StatusOK,
			expect: func(m *mocks.MockExecutor, saw chan<- bool) {
				m.EXPECT().ReplacePlaylist(gomock.Any(), id, gomock.Any(), gomock.Any()).
					DoAndReturn(func(ctx context.Context, _ string, _ *models.PlaylistReplaceRequest, _ *models.SignedIntent) (*store.PlaylistRecord, error) {
						_, ok := ctx.Deadline()
						saw <- ok
						return playlistRecPtr(playlist.Playlist{DPVersion: "1.1.0", Title: "Replaced"}), nil
					})
			},
		},
		{
			name:       "DELETE /playlists/{id}",
			method:     http.MethodDelete,
			body:       deleteIntentBody("playlist", id, "slug"),
			wantStatus: http.StatusNoContent,
			expect: func(m *mocks.MockExecutor, saw chan<- bool) {
				m.EXPECT().DeletePlaylist(gomock.Any(), id, gomock.Any()).
					DoAndReturn(func(ctx context.Context, _ string, _ *models.SignedDeleteRequest) error {
						_, ok := ctx.Deadline()
						saw <- ok
						return nil
					})
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			mockExec := mocks.NewMockExecutor(ctrl)
			sawDeadline := make(chan bool, 1)
			tc.expect(mockExec, sawDeadline)

			cfg := &config.Config{}
			cfg.Server.WriteTimeout = 5 * time.Second
			cfg.Server.ResponseWriteReserve = time.Second
			r := gin.New()
			RegisterRoutes(r, &Handler{Exec: mockExec, Log: zap.NewNop()}, cfg, zap.NewNop())

			req := httptest.NewRequest(tc.method, "/api/v1/playlists/"+id, tc.body)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			require.Equal(t, tc.wantStatus, w.Code, w.Body.String())
			assert.True(t, <-sawDeadline, "%s must reach the executor with a request deadline", tc.name)
		})
	}
}
