package httpserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/mock/gomock"
	"go.uber.org/zap/zaptest"

	dp1 "github.com/display-protocol/dp1-go"
	"github.com/display-protocol/dp1-go/extension/channels"
	"github.com/display-protocol/dp1-go/playlist"
	"github.com/display-protocol/dp1-go/playlistgroup"
	"github.com/display-protocol/dp1-go/sign"

	"github.com/display-protocol/dp1-feed-v2/internal/executor"
	"github.com/display-protocol/dp1-feed-v2/internal/mocks"
	"github.com/display-protocol/dp1-feed-v2/internal/models"
	"github.com/display-protocol/dp1-feed-v2/internal/store"
)

// deleteIntentBody builds a parseable signed delete-intent body for DELETE handler tests. The executor
// is mocked in these tests, so the signature need not verify — only the JSON must decode.
func deleteIntentBody(targetType, id, slug string) *bytes.Reader {
	b, _ := json.Marshal(models.SignedDeleteRequest{
		Action:     models.IntentActionDelete,
		Target:     models.IntentTarget{Type: targetType, ID: id, Slug: slug},
		Created:    "2026-01-01T00:00:00Z",
		Signatures: []playlist.Signature{{Kid: "did:key:test", Alg: "ed25519", Sig: "s"}},
	})
	return bytes.NewReader(b)
}

func TestHealth(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockExec := mocks.NewMockExecutor(ctrl)
	h := &Handler{
		Exec:    mockExec,
		Log:     zaptest.NewLogger(t),
		Version: "1.2.3",
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/health", nil)

	h.Health(c)

	assert.Equal(t, http.StatusOK, w.Code)

	var resp map[string]any
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Equal(t, "healthy", resp["status"])
	assert.Equal(t, "1.2.3", resp["version"])
	assert.NotEmpty(t, resp["timestamp"])
}

func TestHealthAPI(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockExec := mocks.NewMockExecutor(ctrl)
	h := &Handler{
		Exec:    mockExec,
		Log:     zaptest.NewLogger(t),
		Version: "1.2.3",
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)

	h.HealthAPI(c)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]any
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Equal(t, "healthy", resp["status"])
}

func TestAPIInfo(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockExec := mocks.NewMockExecutor(ctrl)
	expectedInfo := map[string]any{
		"version": "1.2.3",
		"name":    "dp1-feed",
	}
	mockExec.EXPECT().APIInfo("1.2.3").Return(expectedInfo)

	h := &Handler{
		Exec:    mockExec,
		Log:     zaptest.NewLogger(t),
		Version: "1.2.3",
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1", nil)

	h.APIInfo(c)

	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]any
	err := json.Unmarshal(w.Body.Bytes(), &resp)
	require.NoError(t, err)
	assert.Equal(t, expectedInfo, resp)
}

func TestListPlaylists(t *testing.T) {
	tests := []struct {
		name           string
		queryParams    string
		setupMock      func(*mocks.MockExecutor)
		expectedStatus int
		checkResponse  func(*testing.T, []byte)
	}{
		{
			name:        "success with default params",
			queryParams: "",
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					ListPlaylists(gomock.Any(), 100, "", store.SortAsc, "", "").
					Return([]store.PlaylistRecord{playlistRec(playlist.Playlist{DPVersion: "1.1.0"})}, "cursor1", nil)
			},
			expectedStatus: http.StatusOK,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ListResponse[playlist.Playlist]
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Len(t, resp.Items, 1)
				assert.Equal(t, "cursor1", resp.Cursor)
				assert.True(t, resp.HasMore)
			},
		},
		{
			name:        "success with custom limit and sort",
			queryParams: "?limit=50&sort=desc",
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					ListPlaylists(gomock.Any(), 50, "", store.SortDesc, "", "").
					Return([]store.PlaylistRecord{}, "", nil)
			},
			expectedStatus: http.StatusOK,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ListResponse[playlist.Playlist]
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Len(t, resp.Items, 0)
				assert.False(t, resp.HasMore)
			},
		},
		{
			name:        "success with cursor",
			queryParams: "?cursor=abc123",
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					ListPlaylists(gomock.Any(), 100, "abc123", store.SortAsc, "", "").
					Return([]store.PlaylistRecord{playlistRec(playlist.Playlist{DPVersion: "1.1.0"})}, "", nil)
			},
			expectedStatus: http.StatusOK,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ListResponse[playlist.Playlist]
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.False(t, resp.HasMore)
			},
		},
		{
			name:           "invalid limit",
			queryParams:    "?limit=invalid",
			setupMock:      func(m *mocks.MockExecutor) {},
			expectedStatus: http.StatusBadRequest,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "bad_request", resp.Error)
			},
		},
		{
			name:           "invalid sort order",
			queryParams:    "?sort=invalid",
			setupMock:      func(m *mocks.MockExecutor) {},
			expectedStatus: http.StatusBadRequest,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "bad_request", resp.Error)
			},
		},
		{
			name:        "success with channel filter",
			queryParams: "?channel=my-channel",
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					ListPlaylists(gomock.Any(), 100, "", store.SortAsc, "my-channel", "").
					Return([]store.PlaylistRecord{}, "", nil)
			},
			expectedStatus: http.StatusOK,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ListResponse[playlist.Playlist]
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Len(t, resp.Items, 0)
			},
		},
		{
			name:        "success with playlist-group filter",
			queryParams: "?playlist-group=my-group",
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					ListPlaylists(gomock.Any(), 100, "", store.SortAsc, "", "my-group").
					Return([]store.PlaylistRecord{}, "", nil)
			},
			expectedStatus: http.StatusOK,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ListResponse[playlist.Playlist]
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Len(t, resp.Items, 0)
			},
		},
		{
			name:           "both filters provided",
			queryParams:    "?channel=ch&playlist-group=pg",
			setupMock:      func(m *mocks.MockExecutor) {},
			expectedStatus: http.StatusBadRequest,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "bad_request", resp.Error)
				assert.Contains(t, resp.Message, "cannot be used together")
			},
		},
		{
			name:        "executor returns not found error",
			queryParams: "",
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					ListPlaylists(gomock.Any(), 100, "", store.SortAsc, "", "").
					Return(nil, "", store.ErrNotFound)
			},
			expectedStatus: http.StatusNotFound,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "not_found", resp.Error)
			},
		},
		{
			name:        "executor returns internal error",
			queryParams: "",
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					ListPlaylists(gomock.Any(), 100, "", store.SortAsc, "", "").
					Return(nil, "", errors.New("db connection failed"))
			},
			expectedStatus: http.StatusInternalServerError,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "internal_error", resp.Error)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockExec := mocks.NewMockExecutor(ctrl)
			tt.setupMock(mockExec)

			h := &Handler{
				Exec: mockExec,
				Log:  zaptest.NewLogger(t),
			}

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/playlists"+tt.queryParams, nil)

			h.ListPlaylists(c)

			assert.Equal(t, tt.expectedStatus, w.Code)
			tt.checkResponse(t, w.Body.Bytes())
		})
	}
}

func TestCreatePlaylist(t *testing.T) {
	validBody := models.PlaylistCreateRequest{
		DPVersion: "1.1.0",
		Title:     "Test Playlist",
		Items:     []playlist.PlaylistItem{{ID: "item1"}},
	}

	tests := []struct {
		name           string
		body           any
		setupMock      func(*mocks.MockExecutor)
		expectedStatus int
		checkResponse  func(*testing.T, []byte)
	}{
		{
			name: "success",
			body: validBody,
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					CreatePlaylist(gomock.Any(), gomock.Any()).
					Return(playlistRecPtr(playlist.Playlist{DPVersion: "1.1.0", Title: "Test Playlist"}), nil)
			},
			expectedStatus: http.StatusCreated,
			checkResponse: func(t *testing.T, body []byte) {
				var resp playlist.Playlist
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "1.1.0", resp.DPVersion)
				assert.Equal(t, "Test Playlist", resp.Title)
			},
		},
		{
			name:           "invalid JSON",
			body:           "not a valid json",
			setupMock:      func(m *mocks.MockExecutor) {},
			expectedStatus: http.StatusBadRequest,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "bad_request", resp.Error)
			},
		},
		{
			name: "validation error",
			body: validBody,
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					CreatePlaylist(gomock.Any(), gomock.Any()).
					Return(nil, dp1.ErrValidation)
			},
			expectedStatus: http.StatusBadRequest,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "validation_error", resp.Error)
			},
		},
		{
			name: "sign error",
			body: validBody,
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					CreatePlaylist(gomock.Any(), gomock.Any()).
					Return(nil, sign.ErrSigInvalid)
			},
			expectedStatus: http.StatusBadRequest,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "signature_invalid", resp.Error)
			},
		},
		{
			name: "nil body from executor",
			body: validBody,
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					CreatePlaylist(gomock.Any(), gomock.Any()).
					Return(nil, nil)
			},
			expectedStatus: http.StatusInternalServerError,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "internal_error", resp.Error)
				assert.Contains(t, resp.Message, "empty document")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockExec := mocks.NewMockExecutor(ctrl)
			tt.setupMock(mockExec)

			h := &Handler{
				Exec: mockExec,
				Log:  zaptest.NewLogger(t),
			}

			bodyBytes, _ := json.Marshal(tt.body)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/playlists", bytes.NewReader(bodyBytes))
			c.Request.Header.Set("Content-Type", "application/json")

			h.CreatePlaylist(c)

			assert.Equal(t, tt.expectedStatus, w.Code)
			tt.checkResponse(t, w.Body.Bytes())
		})
	}
}

func TestGetPlaylist(t *testing.T) {
	tests := []struct {
		name           string
		playlistID     string
		setupMock      func(*mocks.MockExecutor)
		expectedStatus int
		checkResponse  func(*testing.T, []byte)
	}{
		{
			name:       "success by ID",
			playlistID: uuid.New().String(),
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					GetPlaylist(gomock.Any(), gomock.Any()).
					Return(playlistRecPtr(playlist.Playlist{DPVersion: "1.1.0", Title: "Test"}), nil)
			},
			expectedStatus: http.StatusOK,
			checkResponse: func(t *testing.T, body []byte) {
				var resp playlist.Playlist
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "Test", resp.Title)
			},
		},
		{
			name:       "success by slug",
			playlistID: "my-playlist",
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					GetPlaylist(gomock.Any(), "my-playlist").
					Return(playlistRecPtr(playlist.Playlist{DPVersion: "1.1.0"}), nil)
			},
			expectedStatus: http.StatusOK,
			checkResponse: func(t *testing.T, body []byte) {
				var resp playlist.Playlist
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "1.1.0", resp.DPVersion)
			},
		},
		{
			name:       "not found",
			playlistID: "nonexistent",
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					GetPlaylist(gomock.Any(), "nonexistent").
					Return(nil, store.ErrNotFound)
			},
			expectedStatus: http.StatusNotFound,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "not_found", resp.Error)
			},
		},
		{
			name:       "nil body from executor",
			playlistID: "test",
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					GetPlaylist(gomock.Any(), "test").
					Return(nil, nil)
			},
			expectedStatus: http.StatusInternalServerError,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "internal_error", resp.Error)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockExec := mocks.NewMockExecutor(ctrl)
			tt.setupMock(mockExec)

			h := &Handler{
				Exec: mockExec,
				Log:  zaptest.NewLogger(t),
			}

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/playlists/"+tt.playlistID, nil)
			c.Params = gin.Params{{Key: "id", Value: tt.playlistID}}

			h.GetPlaylist(c)

			assert.Equal(t, tt.expectedStatus, w.Code)
			if tt.expectedStatus == http.StatusOK {
				assert.NotEmpty(t, w.Header().Get("ETag"))
			}
			tt.checkResponse(t, w.Body.Bytes())
		})
	}
}

func TestGetPlaylist_IfNoneMatchNotModified(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	pl := playlistRecPtr(playlist.Playlist{DPVersion: "1.1.0", Title: "Cached"})
	mockExec := mocks.NewMockExecutor(ctrl)
	mockExec.EXPECT().
		GetPlaylist(gomock.Any(), "slug-or-id").
		Return(pl, nil).
		Times(2)

	h := &Handler{Exec: mockExec, Log: zaptest.NewLogger(t)}

	w1 := httptest.NewRecorder()
	c1, _ := gin.CreateTestContext(w1)
	c1.Request = httptest.NewRequest(http.MethodGet, "/api/v1/playlists/slug-or-id", nil)
	c1.Params = gin.Params{{Key: "id", Value: "slug-or-id"}}
	h.GetPlaylist(c1)
	require.Equal(t, http.StatusOK, w1.Code)
	etag := w1.Header().Get("ETag")
	require.NotEmpty(t, etag)

	w2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(w2)
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/playlists/slug-or-id", nil)
	req2.Header.Set("If-None-Match", etag)
	c2.Request = req2
	c2.Params = gin.Params{{Key: "id", Value: "slug-or-id"}}
	h.GetPlaylist(c2)
	assert.Equal(t, http.StatusNotModified, w2.Code)
	assert.Empty(t, w2.Body.Bytes())
	assert.Equal(t, etag, w2.Header().Get("ETag"))
}

func TestListPlaylistItems(t *testing.T) {
	tests := []struct {
		name           string
		queryParams    string
		setupMock      func(*mocks.MockExecutor)
		expectedStatus int
		checkResponse  func(*testing.T, []byte)
	}{
		{
			name:        "success",
			queryParams: "",
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					ListPlaylistItems(gomock.Any(), 100, "", store.SortAsc, "", "").
					Return([]playlist.PlaylistItem{{ID: "item1"}}, "", nil)
			},
			expectedStatus: http.StatusOK,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ListResponse[playlist.PlaylistItem]
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Len(t, resp.Items, 1)
			},
		},
		{
			name:        "success with channel filter",
			queryParams: "?channel=my-channel",
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					ListPlaylistItems(gomock.Any(), 100, "", store.SortAsc, "my-channel", "").
					Return([]playlist.PlaylistItem{}, "", nil)
			},
			expectedStatus: http.StatusOK,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ListResponse[playlist.PlaylistItem]
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Len(t, resp.Items, 0)
			},
		},
		{
			name:        "success with playlist-group filter",
			queryParams: "?playlist-group=my-group",
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					ListPlaylistItems(gomock.Any(), 100, "", store.SortAsc, "", "my-group").
					Return([]playlist.PlaylistItem{}, "", nil)
			},
			expectedStatus: http.StatusOK,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ListResponse[playlist.PlaylistItem]
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Len(t, resp.Items, 0)
			},
		},
		{
			name:           "both filters provided",
			queryParams:    "?channel=ch&playlist-group=pg",
			setupMock:      func(m *mocks.MockExecutor) {},
			expectedStatus: http.StatusBadRequest,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "bad_request", resp.Error)
				assert.Contains(t, resp.Message, "cannot be used together")
			},
		},
		{
			name:           "invalid limit",
			queryParams:    "?limit=abc",
			setupMock:      func(m *mocks.MockExecutor) {},
			expectedStatus: http.StatusBadRequest,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "bad_request", resp.Error)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockExec := mocks.NewMockExecutor(ctrl)
			tt.setupMock(mockExec)

			h := &Handler{
				Exec: mockExec,
				Log:  zaptest.NewLogger(t),
			}

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/playlist-items"+tt.queryParams, nil)

			h.ListPlaylistItems(c)

			assert.Equal(t, tt.expectedStatus, w.Code)
			tt.checkResponse(t, w.Body.Bytes())
		})
	}
}

func TestGetPlaylistItem(t *testing.T) {
	itemID := uuid.New()

	tests := []struct {
		name           string
		itemIDStr      string
		setupMock      func(*mocks.MockExecutor)
		expectedStatus int
		checkResponse  func(*testing.T, []byte)
	}{
		{
			name:      "success",
			itemIDStr: itemID.String(),
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					GetPlaylistItem(gomock.Any(), itemID).
					Return(&playlist.PlaylistItem{ID: "test-item"}, nil)
			},
			expectedStatus: http.StatusOK,
			checkResponse: func(t *testing.T, body []byte) {
				var resp playlist.PlaylistItem
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "test-item", resp.ID)
			},
		},
		{
			name:           "invalid UUID",
			itemIDStr:      "not-a-uuid",
			setupMock:      func(m *mocks.MockExecutor) {},
			expectedStatus: http.StatusBadRequest,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "bad_request", resp.Error)
				assert.Contains(t, resp.Message, "UUID")
			},
		},
		{
			name:      "not found",
			itemIDStr: itemID.String(),
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					GetPlaylistItem(gomock.Any(), itemID).
					Return(nil, store.ErrNotFound)
			},
			expectedStatus: http.StatusNotFound,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "not_found", resp.Error)
			},
		},
		{
			name:      "nil body from executor",
			itemIDStr: itemID.String(),
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					GetPlaylistItem(gomock.Any(), itemID).
					Return(nil, nil)
			},
			expectedStatus: http.StatusInternalServerError,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "internal_error", resp.Error)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockExec := mocks.NewMockExecutor(ctrl)
			tt.setupMock(mockExec)

			h := &Handler{
				Exec: mockExec,
				Log:  zaptest.NewLogger(t),
			}

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/playlist-items/"+tt.itemIDStr, nil)
			c.Params = gin.Params{{Key: "id", Value: tt.itemIDStr}}

			h.GetPlaylistItem(c)

			assert.Equal(t, tt.expectedStatus, w.Code)
			if tt.expectedStatus == http.StatusOK {
				assert.NotEmpty(t, w.Header().Get("ETag"))
			}
			tt.checkResponse(t, w.Body.Bytes())
		})
	}
}

// putEnvelope builds a PUT body: the document plus the signed intent that authorizes replacing this
// resource with it. These handler tests mock the executor, so the intent only has to parse — the
// executor is what verifies it.
func putEnvelope(doc any, targetType, id, slug string) []byte {
	d, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	a, err := json.Marshal(models.SignedIntent{
		Action:      models.IntentActionReplace,
		Target:      models.IntentTarget{Type: targetType, ID: id, Slug: slug},
		PayloadHash: "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		Created:     time.Now().UTC().Format(time.RFC3339),
		Signatures:  []playlist.Signature{{Alg: "ed25519", Kid: "did:key:test", Sig: "sig"}},
	})
	if err != nil {
		panic(err)
	}
	b, err := json.Marshal(map[string]json.RawMessage{"document": d, "authorization": a})
	if err != nil {
		panic(err)
	}
	return b
}

func TestReplacePlaylist(t *testing.T) {
	playlistID := uuid.New().String()
	validBody := models.PlaylistReplaceRequest{
		DPVersion: "1.1.0",
		Title:     "Updated Playlist",
		Items:     []playlist.PlaylistItem{{ID: "item1"}},
	}

	tests := []struct {
		name           string
		body           any
		setupMock      func(*mocks.MockExecutor)
		expectedStatus int
		checkResponse  func(*testing.T, []byte)
	}{
		{
			name: "success",
			body: validBody,
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					ReplacePlaylist(gomock.Any(), playlistID, gomock.Any(), gomock.Any()).
					Return(playlistRecPtr(playlist.Playlist{DPVersion: "1.1.0", Title: "Updated Playlist"}), nil)
			},
			expectedStatus: http.StatusOK,
			checkResponse: func(t *testing.T, body []byte) {
				var resp playlist.Playlist
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "Updated Playlist", resp.Title)
			},
		},
		{
			name:           "invalid JSON",
			body:           "invalid",
			setupMock:      func(m *mocks.MockExecutor) {},
			expectedStatus: http.StatusBadRequest,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "bad_request", resp.Error)
			},
		},
		{
			name: "not found",
			body: validBody,
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					ReplacePlaylist(gomock.Any(), playlistID, gomock.Any(), gomock.Any()).
					Return(nil, store.ErrNotFound)
			},
			expectedStatus: http.StatusNotFound,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "not_found", resp.Error)
			},
		},
		{
			name: "nil body from executor",
			body: validBody,
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					ReplacePlaylist(gomock.Any(), playlistID, gomock.Any(), gomock.Any()).
					Return(nil, nil)
			},
			expectedStatus: http.StatusInternalServerError,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "internal_error", resp.Error)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockExec := mocks.NewMockExecutor(ctrl)
			tt.setupMock(mockExec)

			h := &Handler{
				Exec: mockExec,
				Log:  zaptest.NewLogger(t),
			}

			bodyBytes := putEnvelope(tt.body, models.IntentTargetPlaylist, playlistID, "slug")
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/playlists/"+playlistID, bytes.NewReader(bodyBytes))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Params = gin.Params{{Key: "id", Value: playlistID}}

			h.ReplacePlaylist(c)

			assert.Equal(t, tt.expectedStatus, w.Code)
			tt.checkResponse(t, w.Body.Bytes())
		})
	}
}

func TestDeletePlaylist(t *testing.T) {
	playlistID := uuid.New().String()

	tests := []struct {
		name           string
		setupMock      func(*mocks.MockExecutor)
		expectedStatus int
	}{
		{
			name: "success",
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					DeletePlaylist(gomock.Any(), playlistID, gomock.Any()).
					Return(nil)
			},
			expectedStatus: http.StatusNoContent,
		},
		{
			name: "not found",
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					DeletePlaylist(gomock.Any(), playlistID, gomock.Any()).
					Return(store.ErrNotFound)
			},
			expectedStatus: http.StatusNotFound,
		},
		{
			name: "internal error",
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					DeletePlaylist(gomock.Any(), playlistID, gomock.Any()).
					Return(errors.New("db error"))
			},
			expectedStatus: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockExec := mocks.NewMockExecutor(ctrl)
			tt.setupMock(mockExec)

			h := &Handler{
				Exec: mockExec,
				Log:  zaptest.NewLogger(t),
			}

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodDelete, "/api/v1/playlists/"+playlistID, deleteIntentBody(models.IntentTargetPlaylist, playlistID, "slug"))
			c.Params = gin.Params{{Key: "id", Value: playlistID}}

			h.DeletePlaylist(c)

			// For success case with no body, Gin sets status but doesn't write until body write
			// Check the Gin writer's status which is set by c.Status()
			if tt.expectedStatus == http.StatusNoContent {
				assert.Equal(t, tt.expectedStatus, c.Writer.Status())
			} else {
				assert.Equal(t, tt.expectedStatus, w.Code)
			}
		})
	}
}

// TestDeletePlaylist_forbidden covers the delete handler mapping an ownership failure to 403.
func TestDeletePlaylist_forbidden(t *testing.T) {
	playlistID := uuid.New().String()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockExec := mocks.NewMockExecutor(ctrl)
	mockExec.EXPECT().
		DeletePlaylist(gomock.Any(), playlistID, gomock.Any()).
		Return(executor.ErrNotResourceOwner)

	h := &Handler{Exec: mockExec, Log: zaptest.NewLogger(t)}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodDelete, "/api/v1/playlists/"+playlistID, deleteIntentBody(models.IntentTargetPlaylist, playlistID, "slug"))
	c.Params = gin.Params{{Key: "id", Value: playlistID}}

	h.DeletePlaylist(c)

	assert.Equal(t, http.StatusForbidden, w.Code)
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "forbidden", resp.Error)
}

// TestDeletePlaylist_malformedBody covers the delete handler rejecting an unparseable body with 400,
// before the executor is called.
func TestDeletePlaylist_malformedBody(t *testing.T) {
	playlistID := uuid.New().String()
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockExec := mocks.NewMockExecutor(ctrl) // no DeletePlaylist call expected

	h := &Handler{Exec: mockExec, Log: zaptest.NewLogger(t)}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodDelete, "/api/v1/playlists/"+playlistID, bytes.NewReader([]byte(`{invalid`)))
	c.Params = gin.Params{{Key: "id", Value: playlistID}}

	h.DeletePlaylist(c)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	var resp ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, "bad_request", resp.Error)
}

// TestDeletePlaylist_strictDecoding covers the delete handler applying the same strict decoding as every
// other write. encoding/json matches member names case-insensitively and ignores unknown ones, so without
// the strict pass {"Action":"delete"} or a stray member would bind and the delete would proceed — on the
// least forgiving route in the API.
func TestDeletePlaylist_strictDecoding(t *testing.T) {
	playlistID := uuid.New().String()
	sig := `[{"alg":"ed25519","kid":"did:key:z6Mk","ts":"2026-01-01T00:00:00Z","payload_hash":"sha256:x","role":"curator","sig":"s"}]`

	cases := []struct {
		name string
		body string
	}{
		{
			name: "case variant member",
			body: `{"Action":"delete","target":{"type":"playlist","id":"` + playlistID + `","slug":"s"},"created":"2026-01-01T00:00:00Z","signatures":` + sig + `}`,
		},
		{
			name: "unknown member",
			body: `{"action":"delete","target":{"type":"playlist","id":"` + playlistID + `","slug":"s"},"created":"2026-01-01T00:00:00Z","signatures":` + sig + `,"extra":1}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			mockExec := mocks.NewMockExecutor(ctrl) // no DeletePlaylist call expected

			h := &Handler{Exec: mockExec, Log: zaptest.NewLogger(t)}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodDelete, "/api/v1/playlists/"+playlistID, bytes.NewReader([]byte(tc.body)))
			c.Params = gin.Params{{Key: "id", Value: playlistID}}

			h.DeletePlaylist(c)

			assert.Equal(t, http.StatusBadRequest, w.Code)
			var resp ErrorResponse
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
			assert.Equal(t, "bad_request", resp.Error)
		})
	}
}

func TestListPlaylistGroups(t *testing.T) {
	tests := []struct {
		name           string
		queryParams    string
		setupMock      func(*mocks.MockExecutor)
		expectedStatus int
		checkResponse  func(*testing.T, []byte)
	}{
		{
			name:        "success",
			queryParams: "",
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					ListPlaylistGroups(gomock.Any(), 100, "", store.SortAsc).
					Return([]store.PlaylistGroupRecord{groupRec(playlistgroup.Group{Title: "Test Group"})}, "", nil)
			},
			expectedStatus: http.StatusOK,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ListResponse[playlistgroup.Group]
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Len(t, resp.Items, 1)
			},
		},
		{
			name:           "invalid sort",
			queryParams:    "?sort=invalid",
			setupMock:      func(m *mocks.MockExecutor) {},
			expectedStatus: http.StatusBadRequest,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "bad_request", resp.Error)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockExec := mocks.NewMockExecutor(ctrl)
			tt.setupMock(mockExec)

			h := &Handler{
				Exec: mockExec,
				Log:  zaptest.NewLogger(t),
			}

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/playlist-groups"+tt.queryParams, nil)

			h.ListPlaylistGroups(c)

			assert.Equal(t, tt.expectedStatus, w.Code)
			tt.checkResponse(t, w.Body.Bytes())
		})
	}
}

func TestCreatePlaylistGroup(t *testing.T) {
	validBody := models.PlaylistGroupCreateRequest{
		Title:     "Test Group",
		Playlists: []string{"http://example.com/api/v1/playlists/test"},
	}

	tests := []struct {
		name           string
		body           any
		setupMock      func(*mocks.MockExecutor)
		expectedStatus int
		checkResponse  func(*testing.T, []byte)
	}{
		{
			name: "success",
			body: validBody,
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					CreatePlaylistGroup(gomock.Any(), gomock.Any()).
					Return(groupRecPtr(playlistgroup.Group{Title: "Test Group"}), nil)
			},
			expectedStatus: http.StatusCreated,
			checkResponse: func(t *testing.T, body []byte) {
				var resp playlistgroup.Group
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "Test Group", resp.Title)
			},
		},
		{
			name:           "invalid JSON",
			body:           "invalid",
			setupMock:      func(m *mocks.MockExecutor) {},
			expectedStatus: http.StatusBadRequest,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "bad_request", resp.Error)
			},
		},
		{
			name: "nil body from executor",
			body: validBody,
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					CreatePlaylistGroup(gomock.Any(), gomock.Any()).
					Return(nil, nil)
			},
			expectedStatus: http.StatusInternalServerError,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "internal_error", resp.Error)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockExec := mocks.NewMockExecutor(ctrl)
			tt.setupMock(mockExec)

			h := &Handler{
				Exec: mockExec,
				Log:  zaptest.NewLogger(t),
			}

			bodyBytes, _ := json.Marshal(tt.body)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/playlist-groups", bytes.NewReader(bodyBytes))
			c.Request.Header.Set("Content-Type", "application/json")

			h.CreatePlaylistGroup(c)

			assert.Equal(t, tt.expectedStatus, w.Code)
			tt.checkResponse(t, w.Body.Bytes())
		})
	}
}

func TestGetPlaylistGroup(t *testing.T) {
	groupID := uuid.New().String()

	tests := []struct {
		name           string
		setupMock      func(*mocks.MockExecutor)
		expectedStatus int
		checkResponse  func(*testing.T, []byte)
	}{
		{
			name: "success",
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					GetPlaylistGroup(gomock.Any(), groupID).
					Return(groupRecPtr(playlistgroup.Group{Title: "Test Group"}), nil)
			},
			expectedStatus: http.StatusOK,
			checkResponse: func(t *testing.T, body []byte) {
				var resp playlistgroup.Group
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "Test Group", resp.Title)
			},
		},
		{
			name: "not found",
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					GetPlaylistGroup(gomock.Any(), groupID).
					Return(nil, store.ErrNotFound)
			},
			expectedStatus: http.StatusNotFound,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "not_found", resp.Error)
			},
		},
		{
			name: "nil body from executor",
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					GetPlaylistGroup(gomock.Any(), groupID).
					Return(nil, nil)
			},
			expectedStatus: http.StatusInternalServerError,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "internal_error", resp.Error)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockExec := mocks.NewMockExecutor(ctrl)
			tt.setupMock(mockExec)

			h := &Handler{
				Exec: mockExec,
				Log:  zaptest.NewLogger(t),
			}

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/playlist-groups/"+groupID, nil)
			c.Params = gin.Params{{Key: "id", Value: groupID}}

			h.GetPlaylistGroup(c)

			assert.Equal(t, tt.expectedStatus, w.Code)
			if tt.expectedStatus == http.StatusOK {
				assert.NotEmpty(t, w.Header().Get("ETag"))
			}
			tt.checkResponse(t, w.Body.Bytes())
		})
	}
}

func TestReplacePlaylistGroup(t *testing.T) {
	groupID := uuid.New().String()
	validBody := models.PlaylistGroupReplaceRequest{
		Title:     "Updated Group",
		Playlists: []string{"http://example.com/api/v1/playlists/test"},
	}

	tests := []struct {
		name           string
		body           any
		setupMock      func(*mocks.MockExecutor)
		expectedStatus int
		checkResponse  func(*testing.T, []byte)
	}{
		{
			name: "success",
			body: validBody,
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					ReplacePlaylistGroup(gomock.Any(), groupID, gomock.Any(), gomock.Any()).
					Return(groupRecPtr(playlistgroup.Group{Title: "Updated Group"}), nil)
			},
			expectedStatus: http.StatusOK,
			checkResponse: func(t *testing.T, body []byte) {
				var resp playlistgroup.Group
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "Updated Group", resp.Title)
			},
		},
		{
			name:           "invalid JSON",
			body:           "invalid",
			setupMock:      func(m *mocks.MockExecutor) {},
			expectedStatus: http.StatusBadRequest,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "bad_request", resp.Error)
			},
		},
		{
			name: "nil body from executor",
			body: validBody,
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					ReplacePlaylistGroup(gomock.Any(), groupID, gomock.Any(), gomock.Any()).
					Return(nil, nil)
			},
			expectedStatus: http.StatusInternalServerError,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "internal_error", resp.Error)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockExec := mocks.NewMockExecutor(ctrl)
			tt.setupMock(mockExec)

			h := &Handler{
				Exec: mockExec,
				Log:  zaptest.NewLogger(t),
			}

			bodyBytes := putEnvelope(tt.body, models.IntentTargetPlaylistGroup, groupID, "slug")
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/playlist-groups/"+groupID, bytes.NewReader(bodyBytes))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Params = gin.Params{{Key: "id", Value: groupID}}

			h.ReplacePlaylistGroup(c)

			assert.Equal(t, tt.expectedStatus, w.Code)
			tt.checkResponse(t, w.Body.Bytes())
		})
	}
}

func TestDeletePlaylistGroup(t *testing.T) {
	groupID := uuid.New().String()

	tests := []struct {
		name           string
		setupMock      func(*mocks.MockExecutor)
		expectedStatus int
	}{
		{
			name: "success",
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					DeletePlaylistGroup(gomock.Any(), groupID, gomock.Any()).
					Return(nil)
			},
			expectedStatus: http.StatusNoContent,
		},
		{
			name: "not found",
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					DeletePlaylistGroup(gomock.Any(), groupID, gomock.Any()).
					Return(store.ErrNotFound)
			},
			expectedStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockExec := mocks.NewMockExecutor(ctrl)
			tt.setupMock(mockExec)

			h := &Handler{
				Exec: mockExec,
				Log:  zaptest.NewLogger(t),
			}

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodDelete, "/api/v1/playlist-groups/"+groupID, deleteIntentBody(models.IntentTargetPlaylistGroup, groupID, "slug"))
			c.Params = gin.Params{{Key: "id", Value: groupID}}

			h.DeletePlaylistGroup(c)

			// For success case with no body, check Gin writer's status
			if tt.expectedStatus == http.StatusNoContent {
				assert.Equal(t, tt.expectedStatus, c.Writer.Status())
			} else {
				assert.Equal(t, tt.expectedStatus, w.Code)
			}
		})
	}
}

func TestListChannels(t *testing.T) {
	tests := []struct {
		name           string
		queryParams    string
		setupMock      func(*mocks.MockExecutor)
		expectedStatus int
		checkResponse  func(*testing.T, []byte)
	}{
		{
			name:        "success",
			queryParams: "",
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					ListChannels(gomock.Any(), 100, "", store.SortAsc).
					Return([]store.ChannelRecord{channelRec(channels.Channel{Title: "Test Channel"})}, "", nil)
			},
			expectedStatus: http.StatusOK,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ListResponse[channels.Channel]
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Len(t, resp.Items, 1)
			},
		},
		{
			name:           "invalid limit",
			queryParams:    "?limit=abc",
			setupMock:      func(m *mocks.MockExecutor) {},
			expectedStatus: http.StatusBadRequest,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "bad_request", resp.Error)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockExec := mocks.NewMockExecutor(ctrl)
			tt.setupMock(mockExec)

			h := &Handler{
				Exec: mockExec,
				Log:  zaptest.NewLogger(t),
			}

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/channels"+tt.queryParams, nil)

			h.ListChannels(c)

			assert.Equal(t, tt.expectedStatus, w.Code)
			tt.checkResponse(t, w.Body.Bytes())
		})
	}
}

func TestCreateChannel(t *testing.T) {
	validBody := models.ChannelCreateRequest{
		Title:     "Test Channel",
		Playlists: []string{"http://example.com/api/v1/playlists/test"},
	}

	tests := []struct {
		name           string
		body           any
		setupMock      func(*mocks.MockExecutor)
		expectedStatus int
		checkResponse  func(*testing.T, []byte)
	}{
		{
			name: "success",
			body: validBody,
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					CreateChannel(gomock.Any(), gomock.Any()).
					Return(channelRecPtr(channels.Channel{Title: "Test Channel"}), nil)
			},
			expectedStatus: http.StatusCreated,
			checkResponse: func(t *testing.T, body []byte) {
				var resp channels.Channel
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "Test Channel", resp.Title)
			},
		},
		{
			name: "extensions disabled",
			body: validBody,
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					CreateChannel(gomock.Any(), gomock.Any()).
					Return(nil, executor.ErrExtensionsDisabled)
			},
			expectedStatus: http.StatusNotFound,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "extensions_disabled", resp.Error)
			},
		},
		{
			name:           "invalid JSON",
			body:           "invalid",
			setupMock:      func(m *mocks.MockExecutor) {},
			expectedStatus: http.StatusBadRequest,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "bad_request", resp.Error)
			},
		},
		{
			name: "nil body from executor",
			body: validBody,
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					CreateChannel(gomock.Any(), gomock.Any()).
					Return(nil, nil)
			},
			expectedStatus: http.StatusInternalServerError,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "internal_error", resp.Error)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockExec := mocks.NewMockExecutor(ctrl)
			tt.setupMock(mockExec)

			h := &Handler{
				Exec: mockExec,
				Log:  zaptest.NewLogger(t),
			}

			bodyBytes, _ := json.Marshal(tt.body)
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/channels", bytes.NewReader(bodyBytes))
			c.Request.Header.Set("Content-Type", "application/json")

			h.CreateChannel(c)

			assert.Equal(t, tt.expectedStatus, w.Code)
			tt.checkResponse(t, w.Body.Bytes())
		})
	}
}

func TestGetChannel(t *testing.T) {
	channelID := uuid.New().String()

	tests := []struct {
		name           string
		setupMock      func(*mocks.MockExecutor)
		expectedStatus int
		checkResponse  func(*testing.T, []byte)
	}{
		{
			name: "success",
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					GetChannel(gomock.Any(), channelID).
					Return(channelRecPtr(channels.Channel{Title: "Test Channel"}), nil)
			},
			expectedStatus: http.StatusOK,
			checkResponse: func(t *testing.T, body []byte) {
				var resp channels.Channel
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "Test Channel", resp.Title)
			},
		},
		{
			name: "extensions disabled",
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					GetChannel(gomock.Any(), channelID).
					Return(nil, executor.ErrExtensionsDisabled)
			},
			expectedStatus: http.StatusNotFound,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "extensions_disabled", resp.Error)
			},
		},
		{
			name: "not found",
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					GetChannel(gomock.Any(), channelID).
					Return(nil, store.ErrNotFound)
			},
			expectedStatus: http.StatusNotFound,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "not_found", resp.Error)
			},
		},
		{
			name: "nil body from executor",
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					GetChannel(gomock.Any(), channelID).
					Return(nil, nil)
			},
			expectedStatus: http.StatusInternalServerError,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "internal_error", resp.Error)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockExec := mocks.NewMockExecutor(ctrl)
			tt.setupMock(mockExec)

			h := &Handler{
				Exec: mockExec,
				Log:  zaptest.NewLogger(t),
			}

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/channels/"+channelID, nil)
			c.Params = gin.Params{{Key: "id", Value: channelID}}

			h.GetChannel(c)

			assert.Equal(t, tt.expectedStatus, w.Code)
			if tt.expectedStatus == http.StatusOK {
				assert.NotEmpty(t, w.Header().Get("ETag"))
			}
			tt.checkResponse(t, w.Body.Bytes())
		})
	}
}

func TestReplaceChannel(t *testing.T) {
	channelID := uuid.New().String()
	validBody := models.ChannelReplaceRequest{
		Title:     "Updated Channel",
		Slug:      "updated-channel",
		Playlists: []string{"http://example.com/api/v1/playlists/test"},
	}

	tests := []struct {
		name           string
		body           any
		setupMock      func(*mocks.MockExecutor)
		expectedStatus int
		checkResponse  func(*testing.T, []byte)
	}{
		{
			name: "success",
			body: validBody,
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					ReplaceChannel(gomock.Any(), channelID, gomock.Any(), gomock.Any()).
					Return(channelRecPtr(channels.Channel{Title: "Updated Channel"}), nil)
			},
			expectedStatus: http.StatusOK,
			checkResponse: func(t *testing.T, body []byte) {
				var resp channels.Channel
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "Updated Channel", resp.Title)
			},
		},
		{
			name: "extensions disabled",
			body: validBody,
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					ReplaceChannel(gomock.Any(), channelID, gomock.Any(), gomock.Any()).
					Return(nil, executor.ErrExtensionsDisabled)
			},
			expectedStatus: http.StatusNotFound,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "extensions_disabled", resp.Error)
			},
		},
		{
			name:           "invalid JSON",
			body:           "invalid",
			setupMock:      func(m *mocks.MockExecutor) {},
			expectedStatus: http.StatusBadRequest,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "bad_request", resp.Error)
			},
		},
		{
			name: "nil body from executor",
			body: validBody,
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					ReplaceChannel(gomock.Any(), channelID, gomock.Any(), gomock.Any()).
					Return(nil, nil)
			},
			expectedStatus: http.StatusInternalServerError,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "internal_error", resp.Error)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockExec := mocks.NewMockExecutor(ctrl)
			tt.setupMock(mockExec)

			h := &Handler{
				Exec: mockExec,
				Log:  zaptest.NewLogger(t),
			}

			bodyBytes := putEnvelope(tt.body, models.IntentTargetChannel, channelID, "slug")
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/channels/"+channelID, bytes.NewReader(bodyBytes))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Params = gin.Params{{Key: "id", Value: channelID}}

			h.ReplaceChannel(c)

			assert.Equal(t, tt.expectedStatus, w.Code)
			tt.checkResponse(t, w.Body.Bytes())
		})
	}
}

func TestDeleteChannel(t *testing.T) {
	channelID := uuid.New().String()

	tests := []struct {
		name           string
		setupMock      func(*mocks.MockExecutor)
		expectedStatus int
		checkResponse  func(*testing.T, []byte)
	}{
		{
			name: "success",
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					DeleteChannel(gomock.Any(), channelID, gomock.Any()).
					Return(nil)
			},
			expectedStatus: http.StatusNoContent,
			checkResponse:  func(t *testing.T, body []byte) {},
		},
		{
			name: "extensions disabled",
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					DeleteChannel(gomock.Any(), channelID, gomock.Any()).
					Return(executor.ErrExtensionsDisabled)
			},
			expectedStatus: http.StatusNotFound,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "extensions_disabled", resp.Error)
			},
		},
		{
			name: "not found",
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					DeleteChannel(gomock.Any(), channelID, gomock.Any()).
					Return(store.ErrNotFound)
			},
			expectedStatus: http.StatusNotFound,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "not_found", resp.Error)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockExec := mocks.NewMockExecutor(ctrl)
			tt.setupMock(mockExec)

			h := &Handler{
				Exec: mockExec,
				Log:  zaptest.NewLogger(t),
			}

			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodDelete, "/api/v1/channels/"+channelID, deleteIntentBody(models.IntentTargetChannel, channelID, "slug"))
			c.Params = gin.Params{{Key: "id", Value: channelID}}

			h.DeleteChannel(c)

			// For success case with no body, check Gin writer's status
			if tt.expectedStatus == http.StatusNoContent {
				assert.Equal(t, tt.expectedStatus, c.Writer.Status())
			} else {
				assert.Equal(t, tt.expectedStatus, w.Code)
			}
			tt.checkResponse(t, w.Body.Bytes())
		})
	}
}
