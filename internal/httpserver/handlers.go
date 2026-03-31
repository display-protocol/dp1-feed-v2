package httpserver

// HTTP handlers: parse query/body, call executor, map errors to OpenAPI-style JSON (see ErrorResponse).

import (
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/display-protocol/dp1-feed-v2/internal/executor"
	"github.com/display-protocol/dp1-feed-v2/internal/models"
	"github.com/display-protocol/dp1-feed-v2/internal/store"
)

// Handler carries the executor, logger, and build version for health/metadata responses.
type Handler struct {
	Exec    executor.Executor
	Log     *zap.Logger
	Version string
}

// channelURLPattern matches /api/v1/channels/{uuid} at the end of a URL.
var channelURLPattern = regexp.MustCompile(`^https?://.*\/api\/v1\/channels\/[0-9a-f-]{36}$`)

// isValidChannelURL checks if a URL matches the channel URL pattern.
func isValidChannelURL(url string) bool {
	return channelURLPattern.MatchString(url)
}

// Health is a liveness endpoint (no version prefix in plan; we expose both /health and /api/v1/health).
func (h *Handler) Health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":    "healthy",
		"timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"version":   h.Version,
	})
}

// HealthAPI matches OpenAPI /api/v1/health.
func (h *Handler) HealthAPI(c *gin.Context) {
	h.Health(c)
}

// APIInfo serves GET /api/v1.
func (h *Handler) APIInfo(c *gin.Context) {
	c.JSON(http.StatusOK, h.Exec.APIInfo(h.Version))
}

// ListPlaylists GET /api/v1/playlists.
func (h *Handler) ListPlaylists(c *gin.Context) {
	limit, err := ParseListLimitQuery(c.Query("limit"))
	if err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	cursor := c.Query("cursor")
	sortOrder, err := store.ParseSortOrder(c.DefaultQuery("sort", "asc"))
	if err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	pl, next, err := h.Exec.ListPlaylists(c.Request.Context(), limit, cursor, sortOrder)
	if err != nil {
		st, code, msg := mapExecutorError(err)
		writeError(c.Writer, st, code, msg)
		return
	}
	c.JSON(http.StatusOK, NewListResponse(pl, next))
}

// CreatePlaylist POST /api/v1/playlists.
func (h *Handler) CreatePlaylist(c *gin.Context) {
	var req models.PlaylistCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	body, err := h.Exec.CreatePlaylist(c.Request.Context(), &req)
	if err != nil {
		h.Log.Debug("create playlist", zap.Error(err))
		st, code, msg := mapExecutorError(err)
		writeError(c.Writer, st, code, msg)
		return
	}
	if body == nil {
		h.Log.Error("create playlist: nil body")
		writeError(c.Writer, http.StatusInternalServerError, "internal_error", "empty document")
		return
	}
	c.JSON(http.StatusCreated, body)
}

// GetPlaylist GET /api/v1/playlists/:id.
func (h *Handler) GetPlaylist(c *gin.Context) {
	id := c.Param("id")
	body, err := h.Exec.GetPlaylist(c.Request.Context(), id)
	if err != nil {
		st, code, msg := mapExecutorError(err)
		writeError(c.Writer, st, code, msg)
		return
	}
	if body == nil {
		h.Log.Error("get playlist: nil body")
		writeError(c.Writer, http.StatusInternalServerError, "internal_error", "empty document")
		return
	}
	c.JSON(http.StatusOK, body)
}

// ListPlaylistItems GET /api/v1/playlist-items.
func (h *Handler) ListPlaylistItems(c *gin.Context) {
	limit, err := ParseListLimitQuery(c.Query("limit"))
	if err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	cursor := c.Query("cursor")
	sortOrder, err := store.ParseSortOrder(c.DefaultQuery("sort", "asc"))
	if err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	chF := strings.TrimSpace(c.Query("channel"))
	pgF := strings.TrimSpace(c.Query("playlist-group"))
	if chF != "" && pgF != "" {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", "channel and playlist-group filters cannot be used together")
		return
	}

	items, next, err := h.Exec.ListPlaylistItems(c.Request.Context(), limit, cursor, sortOrder, chF, pgF)
	if err != nil {
		st, code, msg := mapExecutorError(err)
		writeError(c.Writer, st, code, msg)
		return
	}
	c.JSON(http.StatusOK, NewListResponse(items, next))
}

// GetPlaylistItem GET /api/v1/playlist-items/:id.
func (h *Handler) GetPlaylistItem(c *gin.Context) {
	idStr := c.Param("id")
	itemID, err := uuid.Parse(idStr)
	if err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", "playlist item id must be a UUID")
		return
	}
	body, err := h.Exec.GetPlaylistItem(c.Request.Context(), itemID)
	if err != nil {
		st, code, msg := mapExecutorError(err)
		writeError(c.Writer, st, code, msg)
		return
	}
	if body == nil {
		h.Log.Error("get playlist item: nil body")
		writeError(c.Writer, http.StatusInternalServerError, "internal_error", "empty document")
		return
	}
	c.JSON(http.StatusOK, body)
}

// ReplacePlaylist PUT /api/v1/playlists/:id.
func (h *Handler) ReplacePlaylist(c *gin.Context) {
	id := c.Param("id")
	var req models.PlaylistReplaceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	body, err := h.Exec.ReplacePlaylist(c.Request.Context(), id, &req)
	if err != nil {
		st, code, msg := mapExecutorError(err)
		writeError(c.Writer, st, code, msg)
		return
	}
	if body == nil {
		h.Log.Error("replace playlist: nil body")
		writeError(c.Writer, http.StatusInternalServerError, "internal_error", "empty document")
		return
	}
	c.JSON(http.StatusOK, body)
}

// UpdatePlaylist PATCH /api/v1/playlists/:id.
func (h *Handler) UpdatePlaylist(c *gin.Context) {
	id := c.Param("id")
	var req models.PlaylistUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	body, err := h.Exec.UpdatePlaylist(c.Request.Context(), id, &req)
	if err != nil {
		st, code, msg := mapExecutorError(err)
		writeError(c.Writer, st, code, msg)
		return
	}
	if body == nil {
		h.Log.Error("update playlist: nil body")
		writeError(c.Writer, http.StatusInternalServerError, "internal_error", "empty document")
		return
	}
	c.JSON(http.StatusOK, body)
}

// DeletePlaylist DELETE /api/v1/playlists/:id.
func (h *Handler) DeletePlaylist(c *gin.Context) {
	id := c.Param("id")
	if err := h.Exec.DeletePlaylist(c.Request.Context(), id); err != nil {
		st, code, msg := mapExecutorError(err)
		writeError(c.Writer, st, code, msg)
		return
	}
	c.Status(http.StatusNoContent)
}

// ListPlaylistGroups GET /api/v1/playlist-groups.
func (h *Handler) ListPlaylistGroups(c *gin.Context) {
	limit, err := ParseListLimitQuery(c.Query("limit"))
	if err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	cursor := c.Query("cursor")
	sortOrder, err := store.ParseSortOrder(c.DefaultQuery("sort", "asc"))
	if err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	bodies, next, err := h.Exec.ListPlaylistGroups(c.Request.Context(), limit, cursor, sortOrder)
	if err != nil {
		st, code, msg := mapExecutorError(err)
		writeError(c.Writer, st, code, msg)
		return
	}
	c.JSON(http.StatusOK, NewListResponse(bodies, next))
}

// CreatePlaylistGroup POST /api/v1/playlist-groups.
func (h *Handler) CreatePlaylistGroup(c *gin.Context) {
	var req models.PlaylistGroupCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	body, err := h.Exec.CreatePlaylistGroup(c.Request.Context(), &req)
	if err != nil {
		h.Log.Debug("create playlist group", zap.Error(err))
		st, code, msg := mapExecutorError(err)
		writeError(c.Writer, st, code, msg)
		return
	}
	if body == nil {
		h.Log.Error("create playlist group: nil body")
		writeError(c.Writer, http.StatusInternalServerError, "internal_error", "empty document")
		return
	}
	c.JSON(http.StatusCreated, body)
}

// GetPlaylistGroup GET /api/v1/playlist-groups/:id.
func (h *Handler) GetPlaylistGroup(c *gin.Context) {
	id := c.Param("id")
	body, err := h.Exec.GetPlaylistGroup(c.Request.Context(), id)
	if err != nil {
		st, code, msg := mapExecutorError(err)
		writeError(c.Writer, st, code, msg)
		return
	}
	if body == nil {
		h.Log.Error("get playlist group: nil body")
		writeError(c.Writer, http.StatusInternalServerError, "internal_error", "empty document")
		return
	}
	c.JSON(http.StatusOK, body)
}

// ReplacePlaylistGroup PUT /api/v1/playlist-groups/:id.
func (h *Handler) ReplacePlaylistGroup(c *gin.Context) {
	id := c.Param("id")
	var req models.PlaylistGroupReplaceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	body, err := h.Exec.ReplacePlaylistGroup(c.Request.Context(), id, &req)
	if err != nil {
		st, code, msg := mapExecutorError(err)
		writeError(c.Writer, st, code, msg)
		return
	}
	if body == nil {
		h.Log.Error("replace playlist group: nil body")
		writeError(c.Writer, http.StatusInternalServerError, "internal_error", "empty document")
		return
	}
	c.JSON(http.StatusOK, body)
}

// UpdatePlaylistGroup PATCH /api/v1/playlist-groups/:id.
func (h *Handler) UpdatePlaylistGroup(c *gin.Context) {
	id := c.Param("id")
	var req models.PlaylistGroupUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	body, err := h.Exec.UpdatePlaylistGroup(c.Request.Context(), id, &req)
	if err != nil {
		st, code, msg := mapExecutorError(err)
		writeError(c.Writer, st, code, msg)
		return
	}
	if body == nil {
		h.Log.Error("update playlist group: nil body")
		writeError(c.Writer, http.StatusInternalServerError, "internal_error", "empty document")
		return
	}
	c.JSON(http.StatusOK, body)
}

// DeletePlaylistGroup DELETE /api/v1/playlist-groups/:id.
func (h *Handler) DeletePlaylistGroup(c *gin.Context) {
	id := c.Param("id")
	if err := h.Exec.DeletePlaylistGroup(c.Request.Context(), id); err != nil {
		st, code, msg := mapExecutorError(err)
		writeError(c.Writer, st, code, msg)
		return
	}
	c.Status(http.StatusNoContent)
}

// ListChannels GET /api/v1/channels.
func (h *Handler) ListChannels(c *gin.Context) {
	limit, err := ParseListLimitQuery(c.Query("limit"))
	if err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	cursor := c.Query("cursor")
	sortOrder, err2 := store.ParseSortOrder(c.DefaultQuery("sort", "asc"))
	if err2 != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err2.Error())
		return
	}
	bodies, next, err := h.Exec.ListChannels(c.Request.Context(), limit, cursor, sortOrder)
	if err != nil {
		st, code, msg := mapExecutorError(err)
		writeError(c.Writer, st, code, msg)
		return
	}
	c.JSON(http.StatusOK, NewListResponse(bodies, next))
}

// CreateChannel POST /api/v1/channels.
func (h *Handler) CreateChannel(c *gin.Context) {
	var req models.ChannelCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	body, err := h.Exec.CreateChannel(c.Request.Context(), &req)
	if err != nil {
		if executor.IsExtensionsDisabled(err) {
			writeError(c.Writer, http.StatusNotFound, "extensions_disabled", "DP-1 extensions are disabled on this deployment")
			return
		}
		h.Log.Debug("create channel", zap.Error(err))
		st, code, msg := mapExecutorError(err)
		writeError(c.Writer, st, code, msg)
		return
	}
	if body == nil {
		h.Log.Error("create channel: nil body")
		writeError(c.Writer, http.StatusInternalServerError, "internal_error", "empty document")
		return
	}
	c.JSON(http.StatusCreated, body)
}

// GetChannel GET /api/v1/channels/:id.
func (h *Handler) GetChannel(c *gin.Context) {
	id := c.Param("id")
	body, err := h.Exec.GetChannel(c.Request.Context(), id)
	if err != nil {
		if executor.IsExtensionsDisabled(err) {
			writeError(c.Writer, http.StatusNotFound, "extensions_disabled", "DP-1 extensions are disabled on this deployment")
			return
		}
		st, code, msg := mapExecutorError(err)
		writeError(c.Writer, st, code, msg)
		return
	}
	if body == nil {
		h.Log.Error("get channel: nil body")
		writeError(c.Writer, http.StatusInternalServerError, "internal_error", "empty document")
		return
	}
	c.JSON(http.StatusOK, body)
}

// ReplaceChannel PUT /api/v1/channels/:id.
func (h *Handler) ReplaceChannel(c *gin.Context) {
	id := c.Param("id")
	var req models.ChannelReplaceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	body, err := h.Exec.ReplaceChannel(c.Request.Context(), id, &req)
	if err != nil {
		if executor.IsExtensionsDisabled(err) {
			writeError(c.Writer, http.StatusNotFound, "extensions_disabled", "DP-1 extensions are disabled on this deployment")
			return
		}
		st, code, msg := mapExecutorError(err)
		writeError(c.Writer, st, code, msg)
		return
	}
	if body == nil {
		h.Log.Error("replace channel: nil body")
		writeError(c.Writer, http.StatusInternalServerError, "internal_error", "empty document")
		return
	}
	c.JSON(http.StatusOK, body)
}

// UpdateChannel PATCH /api/v1/channels/:id.
func (h *Handler) UpdateChannel(c *gin.Context) {
	id := c.Param("id")
	var req models.ChannelUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	body, err := h.Exec.UpdateChannel(c.Request.Context(), id, &req)
	if err != nil {
		if executor.IsExtensionsDisabled(err) {
			writeError(c.Writer, http.StatusNotFound, "extensions_disabled", "DP-1 extensions are disabled on this deployment")
			return
		}
		st, code, msg := mapExecutorError(err)
		writeError(c.Writer, st, code, msg)
		return
	}
	if body == nil {
		h.Log.Error("update channel: nil body")
		writeError(c.Writer, http.StatusInternalServerError, "internal_error", "empty document")
		return
	}
	c.JSON(http.StatusOK, body)
}

// DeleteChannel DELETE /api/v1/channels/:id.
func (h *Handler) DeleteChannel(c *gin.Context) {
	id := c.Param("id")
	if err := h.Exec.DeleteChannel(c.Request.Context(), id); err != nil {
		if executor.IsExtensionsDisabled(err) {
			writeError(c.Writer, http.StatusNotFound, "extensions_disabled", "DP-1 extensions are disabled on this deployment")
			return
		}
		st, code, msg := mapExecutorError(err)
		writeError(c.Writer, st, code, msg)
		return
	}
	c.Status(http.StatusNoContent)
}

// GetChannelRegistry GET /api/v1/registry/channels.
func (h *Handler) GetChannelRegistry(c *gin.Context) {
	pubs, chans, err := h.Exec.GetChannelRegistry(c.Request.Context())
	if err != nil {
		st, code, msg := mapExecutorError(err)
		writeError(c.Writer, st, code, msg)
		return
	}

	// Group channels by publisher ID for efficient lookup.
	chansByPub := make(map[string][]string)
	for _, ch := range chans {
		pubID := ch.PublisherID.String()
		chansByPub[pubID] = append(chansByPub[pubID], ch.ChannelURL)
	}

	// Build response array.
	items := make([]models.RegistryItem, 0, len(pubs))
	for _, pub := range pubs {
		urls := chansByPub[pub.ID.String()]
		if urls == nil {
			urls = []string{}
		}
		items = append(items, models.RegistryItem{
			Name:        pub.Name,
			ChannelURLs: urls,
		})
	}

	c.JSON(http.StatusOK, items)
}

// ReplaceChannelRegistry PUT /api/v1/registry/channels.
func (h *Handler) ReplaceChannelRegistry(c *gin.Context) {
	var req models.RegistryUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}

	// Validate: must have at least one publisher.
	if len(req) == 0 {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", "registry must contain at least one publisher")
		return
	}

	// Validate each item's channel URLs format (regex check).
	for i, item := range req {
		if len(item.ChannelURLs) == 0 {
			writeError(c.Writer, http.StatusBadRequest, "bad_request", "each publisher must have at least one channel URL")
			return
		}
		for _, url := range item.ChannelURLs {
			if !isValidChannelURL(url) {
				writeError(c.Writer, http.StatusBadRequest, "bad_request", "channel URL must end with /api/v1/channels/{uuid} at item "+string(rune(i)))
				return
			}
		}
	}

	totalChannels, err := h.Exec.ReplaceChannelRegistry(c.Request.Context(), req)
	if err != nil {
		st, code, msg := mapExecutorError(err)
		writeError(c.Writer, st, code, msg)
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"success":        true,
		"message":        "Curated registry updated successfully",
		"items_count":    len(req),
		"total_channels": totalChannels,
	})
}
