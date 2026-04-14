package httpserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

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
					Return([]playlist.Playlist{{DPVersion: "1.1.0"}}, "cursor1", nil)
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
					Return([]playlist.Playlist{}, "", nil)
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
					Return([]playlist.Playlist{{DPVersion: "1.1.0"}}, "", nil)
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
					Return([]playlist.Playlist{}, "", nil)
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
					Return([]playlist.Playlist{}, "", nil)
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
					Return(&playlist.Playlist{DPVersion: "1.1.0", Title: "Test Playlist"}, nil)
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
					Return(&playlist.Playlist{DPVersion: "1.1.0", Title: "Test"}, nil)
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
					Return(&playlist.Playlist{DPVersion: "1.1.0"}, nil)
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

	pl := &playlist.Playlist{DPVersion: "1.1.0", Title: "Cached"}
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
					ReplacePlaylist(gomock.Any(), playlistID, gomock.Any()).
					Return(&playlist.Playlist{DPVersion: "1.1.0", Title: "Updated Playlist"}, nil)
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
					ReplacePlaylist(gomock.Any(), playlistID, gomock.Any()).
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
					ReplacePlaylist(gomock.Any(), playlistID, gomock.Any()).
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
			c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/playlists/"+playlistID, bytes.NewReader(bodyBytes))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Params = gin.Params{{Key: "id", Value: playlistID}}

			h.ReplacePlaylist(c)

			assert.Equal(t, tt.expectedStatus, w.Code)
			tt.checkResponse(t, w.Body.Bytes())
		})
	}
}

func TestUpdatePlaylist(t *testing.T) {
	playlistID := uuid.New().String()
	title := "Updated Playlist"
	validBody := models.PlaylistUpdateRequest{
		Title: &title,
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
					UpdatePlaylist(gomock.Any(), playlistID, gomock.Any()).
					Return(&playlist.Playlist{DPVersion: "1.1.0", Title: "Updated Playlist"}, nil)
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
					UpdatePlaylist(gomock.Any(), playlistID, gomock.Any()).
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
					UpdatePlaylist(gomock.Any(), playlistID, gomock.Any()).
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
			c.Request = httptest.NewRequest(http.MethodPatch, "/api/v1/playlists/"+playlistID, bytes.NewReader(bodyBytes))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Params = gin.Params{{Key: "id", Value: playlistID}}

			h.UpdatePlaylist(c)

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
					DeletePlaylist(gomock.Any(), playlistID).
					Return(nil)
			},
			expectedStatus: http.StatusNoContent,
		},
		{
			name: "not found",
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					DeletePlaylist(gomock.Any(), playlistID).
					Return(store.ErrNotFound)
			},
			expectedStatus: http.StatusNotFound,
		},
		{
			name: "internal error",
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					DeletePlaylist(gomock.Any(), playlistID).
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
			c.Request = httptest.NewRequest(http.MethodDelete, "/api/v1/playlists/"+playlistID, nil)
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
					Return([]playlistgroup.Group{{Title: "Test Group"}}, "", nil)
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
					Return(&playlistgroup.Group{Title: "Test Group"}, nil)
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
					Return(&playlistgroup.Group{Title: "Test Group"}, nil)
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
					ReplacePlaylistGroup(gomock.Any(), groupID, gomock.Any()).
					Return(&playlistgroup.Group{Title: "Updated Group"}, nil)
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
					ReplacePlaylistGroup(gomock.Any(), groupID, gomock.Any()).
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
			c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/playlist-groups/"+groupID, bytes.NewReader(bodyBytes))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Params = gin.Params{{Key: "id", Value: groupID}}

			h.ReplacePlaylistGroup(c)

			assert.Equal(t, tt.expectedStatus, w.Code)
			tt.checkResponse(t, w.Body.Bytes())
		})
	}
}

func TestUpdatePlaylistGroup(t *testing.T) {
	groupID := uuid.New().String()
	title := "Updated Group"
	validBody := models.PlaylistGroupUpdateRequest{
		Title: &title,
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
					UpdatePlaylistGroup(gomock.Any(), groupID, gomock.Any()).
					Return(&playlistgroup.Group{Title: "Updated Group"}, nil)
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
			name: "not found",
			body: validBody,
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					UpdatePlaylistGroup(gomock.Any(), groupID, gomock.Any()).
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
					UpdatePlaylistGroup(gomock.Any(), groupID, gomock.Any()).
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
			c.Request = httptest.NewRequest(http.MethodPatch, "/api/v1/playlist-groups/"+groupID, bytes.NewReader(bodyBytes))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Params = gin.Params{{Key: "id", Value: groupID}}

			h.UpdatePlaylistGroup(c)

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
					DeletePlaylistGroup(gomock.Any(), groupID).
					Return(nil)
			},
			expectedStatus: http.StatusNoContent,
		},
		{
			name: "not found",
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					DeletePlaylistGroup(gomock.Any(), groupID).
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
			c.Request = httptest.NewRequest(http.MethodDelete, "/api/v1/playlist-groups/"+groupID, nil)
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
					Return([]channels.Channel{{Title: "Test Channel"}}, "", nil)
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
					Return(&channels.Channel{Title: "Test Channel"}, nil)
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
					Return(&channels.Channel{Title: "Test Channel"}, nil)
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
					ReplaceChannel(gomock.Any(), channelID, gomock.Any()).
					Return(&channels.Channel{Title: "Updated Channel"}, nil)
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
					ReplaceChannel(gomock.Any(), channelID, gomock.Any()).
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
					ReplaceChannel(gomock.Any(), channelID, gomock.Any()).
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
			c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/channels/"+channelID, bytes.NewReader(bodyBytes))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Params = gin.Params{{Key: "id", Value: channelID}}

			h.ReplaceChannel(c)

			assert.Equal(t, tt.expectedStatus, w.Code)
			tt.checkResponse(t, w.Body.Bytes())
		})
	}
}

func TestUpdateChannel(t *testing.T) {
	channelID := uuid.New().String()
	title := "Updated Channel"
	validBody := models.ChannelUpdateRequest{
		Title: &title,
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
					UpdateChannel(gomock.Any(), channelID, gomock.Any()).
					Return(&channels.Channel{Title: "Updated Channel"}, nil)
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
					UpdateChannel(gomock.Any(), channelID, gomock.Any()).
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
			name: "not found",
			body: validBody,
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					UpdateChannel(gomock.Any(), channelID, gomock.Any()).
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
					UpdateChannel(gomock.Any(), channelID, gomock.Any()).
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
			c.Request = httptest.NewRequest(http.MethodPatch, "/api/v1/channels/"+channelID, bytes.NewReader(bodyBytes))
			c.Request.Header.Set("Content-Type", "application/json")
			c.Params = gin.Params{{Key: "id", Value: channelID}}

			h.UpdateChannel(c)

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
					DeleteChannel(gomock.Any(), channelID).
					Return(nil)
			},
			expectedStatus: http.StatusNoContent,
			checkResponse:  func(t *testing.T, body []byte) {},
		},
		{
			name: "extensions disabled",
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					DeleteChannel(gomock.Any(), channelID).
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
					DeleteChannel(gomock.Any(), channelID).
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
			c.Request = httptest.NewRequest(http.MethodDelete, "/api/v1/channels/"+channelID, nil)
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

func TestGetChannelRegistry(t *testing.T) {
	pubID1 := uuid.New()
	pubID2 := uuid.New()

	tests := []struct {
		name           string
		setupMock      func(*mocks.MockExecutor)
		expectedStatus int
		checkResponse  func(*testing.T, []byte)
	}{
		{
			name: "success with publishers and channels",
			setupMock: func(m *mocks.MockExecutor) {
				pubs := []store.RegistryPublisher{
					{ID: pubID1, Name: "Publisher 1"},
					{ID: pubID2, Name: "Publisher 2"},
				}
				chans := []store.RegistryPublisherChannel{
					{PublisherID: pubID1, ChannelURL: "http://example.com/api/v1/channels/" + uuid.New().String()},
					{PublisherID: pubID1, ChannelURL: "http://example.com/api/v1/channels/" + uuid.New().String()},
					{PublisherID: pubID2, ChannelURL: "http://example.com/api/v1/channels/" + uuid.New().String()},
				}
				m.EXPECT().
					GetChannelRegistry(gomock.Any()).
					Return(pubs, chans, nil)
			},
			expectedStatus: http.StatusOK,
			checkResponse: func(t *testing.T, body []byte) {
				var resp []models.RegistryItem
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Len(t, resp, 2)
				assert.Equal(t, "Publisher 1", resp[0].Name)
				assert.Len(t, resp[0].ChannelURLs, 2)
				assert.Equal(t, "Publisher 2", resp[1].Name)
				assert.Len(t, resp[1].ChannelURLs, 1)
			},
		},
		{
			name: "success with no channels",
			setupMock: func(m *mocks.MockExecutor) {
				pubs := []store.RegistryPublisher{
					{ID: pubID1, Name: "Publisher 1"},
				}
				m.EXPECT().
					GetChannelRegistry(gomock.Any()).
					Return(pubs, []store.RegistryPublisherChannel{}, nil)
			},
			expectedStatus: http.StatusOK,
			checkResponse: func(t *testing.T, body []byte) {
				var resp []models.RegistryItem
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Len(t, resp, 1)
				assert.Len(t, resp[0].ChannelURLs, 0)
			},
		},
		{
			name: "internal error",
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					GetChannelRegistry(gomock.Any()).
					Return(nil, nil, errors.New("db error"))
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
			c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/registry/channels", nil)

			h.GetChannelRegistry(c)

			assert.Equal(t, tt.expectedStatus, w.Code)
			tt.checkResponse(t, w.Body.Bytes())
		})
	}
}

func TestReplaceChannelRegistry(t *testing.T) {
	validURL := "http://example.com/api/v1/channels/" + uuid.New().String()
	validBody := models.RegistryUpdateRequest{
		{Name: "Publisher 1", ChannelURLs: []string{validURL}},
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
					ReplaceChannelRegistry(gomock.Any(), gomock.Any()).
					Return(1, nil)
			},
			expectedStatus: http.StatusOK,
			checkResponse: func(t *testing.T, body []byte) {
				var resp map[string]any
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, true, resp["success"])
				assert.Equal(t, float64(1), resp["items_count"])
				assert.Equal(t, float64(1), resp["total_channels"])
			},
		},
		{
			name:           "empty registry",
			body:           models.RegistryUpdateRequest{},
			setupMock:      func(m *mocks.MockExecutor) {},
			expectedStatus: http.StatusBadRequest,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "bad_request", resp.Error)
				assert.Contains(t, resp.Message, "at least one publisher")
			},
		},
		{
			name: "publisher with no channels",
			body: models.RegistryUpdateRequest{
				{Name: "Publisher 1", ChannelURLs: []string{}},
			},
			setupMock:      func(m *mocks.MockExecutor) {},
			expectedStatus: http.StatusBadRequest,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "bad_request", resp.Error)
				// Gin validation error from the binding tag
				assert.Contains(t, resp.Message, "ChannelURLs")
			},
		},
		{
			name: "invalid channel URL format",
			body: models.RegistryUpdateRequest{
				{Name: "Publisher 1", ChannelURLs: []string{"http://example.com/invalid"}},
			},
			setupMock:      func(m *mocks.MockExecutor) {},
			expectedStatus: http.StatusBadRequest,
			checkResponse: func(t *testing.T, body []byte) {
				var resp ErrorResponse
				require.NoError(t, json.Unmarshal(body, &resp))
				assert.Equal(t, "bad_request", resp.Error)
				assert.Contains(t, resp.Message, "channel URL must end with")
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
			name: "executor error",
			body: validBody,
			setupMock: func(m *mocks.MockExecutor) {
				m.EXPECT().
					ReplaceChannelRegistry(gomock.Any(), gomock.Any()).
					Return(0, errors.New("db error"))
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
			c.Request = httptest.NewRequest(http.MethodPut, "/api/v1/registry/channels", bytes.NewReader(bodyBytes))
			c.Request.Header.Set("Content-Type", "application/json")

			h.ReplaceChannelRegistry(c)

			assert.Equal(t, tt.expectedStatus, w.Code)
			tt.checkResponse(t, w.Body.Bytes())
		})
	}
}

func TestIsValidChannelURL(t *testing.T) {
	tests := []struct {
		name     string
		url      string
		expected bool
	}{
		{
			name:     "valid HTTP URL",
			url:      "http://example.com/api/v1/channels/" + uuid.New().String(),
			expected: true,
		},
		{
			name:     "valid HTTPS URL",
			url:      "https://example.com/api/v1/channels/" + uuid.New().String(),
			expected: true,
		},
		{
			name:     "valid with subdomain",
			url:      "https://sub.example.com/api/v1/channels/" + uuid.New().String(),
			expected: true,
		},
		{
			name:     "invalid - missing UUID",
			url:      "https://example.com/api/v1/channels/",
			expected: false,
		},
		{
			name:     "invalid - wrong path",
			url:      "https://example.com/api/v2/channels/" + uuid.New().String(),
			expected: false,
		},
		{
			name:     "invalid - extra path after UUID",
			url:      "https://example.com/api/v1/channels/" + uuid.New().String() + "/extra",
			expected: false,
		},
		{
			name:     "invalid - not a UUID",
			url:      "https://example.com/api/v1/channels/not-a-uuid",
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isValidChannelURL(tt.url)
			assert.Equal(t, tt.expected, result)
		})
	}
}
