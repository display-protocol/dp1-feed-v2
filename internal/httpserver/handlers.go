package httpserver

// HTTP handlers: parse query/body, call executor, map errors to OpenAPI-style JSON (see ErrorResponse).

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/display-protocol/dp1-feed-v2/internal/executor"
	"github.com/display-protocol/dp1-feed-v2/internal/models"
	"github.com/display-protocol/dp1-feed-v2/internal/store"
)

// Document projections for list envelopes (stored bytes, verbatim).
func playlistDocuments(recs []store.PlaylistRecord) []json.RawMessage {
	return documents(recs, func(r *store.PlaylistRecord) json.RawMessage { return r.Raw })
}

func playlistGroupDocuments(recs []store.PlaylistGroupRecord) []json.RawMessage {
	return documents(recs, func(r *store.PlaylistGroupRecord) json.RawMessage { return r.Raw })
}

func channelDocuments(recs []store.ChannelRecord) []json.RawMessage {
	return documents(recs, func(r *store.ChannelRecord) json.RawMessage { return r.Raw })
}

// Handler carries the executor, logger, and build version for health/metadata responses.
type Handler struct {
	Exec    executor.Executor
	Log     *zap.Logger
	Version string
}

// bindDeleteRequest decodes a signed delete-intent body and captures the exact bytes in Raw. The
// executor verifies the signatures over Raw (§7.1 digest, signatures stripped), so the raw form — not
// the re-encoded struct — is what must be preserved. RequireSignatures has already restored the body.
//
// Decoding is strict, like every other write: encoding/json matches member names case-insensitively and
// ignores unknown ones, so a plain Unmarshal would accept {"Action":"delete"} or extra members and then
// execute the delete anyway. Deletion is the least forgiving thing this API does; it should be the last
// place to guess at what the client meant.
func bindDeleteRequest(c *gin.Context) (*models.SignedDeleteRequest, error) {
	raw, err := c.GetRawData()
	if err != nil {
		return nil, err
	}
	var req models.SignedDeleteRequest
	if err := decodeDocument(raw, &req); err != nil {
		return nil, err
	}
	req.Raw = raw
	return &req, nil
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
	chF := strings.TrimSpace(c.Query("channel"))
	pgF := strings.TrimSpace(c.Query("playlist-group"))
	if chF != "" && pgF != "" {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", "channel and playlist-group filters cannot be used together")
		return
	}

	pl, next, err := h.Exec.ListPlaylists(c.Request.Context(), limit, cursor, sortOrder, chF, pgF)
	if err != nil {
		st, code, msg := mapExecutorError(err)
		writeError(c.Writer, st, code, msg)
		return
	}
	writeDocumentList(c, playlistDocuments(pl), next)
}

// CreatePlaylist POST /api/v1/playlists.
func (h *Handler) CreatePlaylist(c *gin.Context) {
	var req models.PlaylistCreateRequest
	raw, err := bindDocument(c, &req)
	if err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	req.Raw = raw
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
	created(c, body.Raw)
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
	writeBytesIndividualGET(c, body.Raw)
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
	if err := writeJSONIndividualGET(c, body); err != nil {
		h.Log.Error("get playlist item: marshal response", zap.Error(err))
		writeError(c.Writer, http.StatusInternalServerError, "internal_error", "response encoding failed")
		return
	}
}

// ReplacePlaylist PUT /api/v1/playlists/:id.
func (h *Handler) ReplacePlaylist(c *gin.Context) {
	id := c.Param("id")
	var req models.PlaylistReplaceRequest
	raw, intent, err := bindSignedReplace(c, &req)
	if err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	req.Raw = raw
	body, err := h.Exec.ReplacePlaylist(c.Request.Context(), id, &req, intent)
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
	writeDocument(c, http.StatusOK, body.Raw)
}

// DeletePlaylist DELETE /api/v1/playlists/:id. Body is a signed delete-intent (see bindDeleteRequest).
func (h *Handler) DeletePlaylist(c *gin.Context) {
	id := c.Param("id")
	req, err := bindDeleteRequest(c)
	if err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := h.Exec.DeletePlaylist(c.Request.Context(), id, req); err != nil {
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
	writeDocumentList(c, playlistGroupDocuments(bodies), next)
}

// CreatePlaylistGroup POST /api/v1/playlist-groups.
func (h *Handler) CreatePlaylistGroup(c *gin.Context) {
	var req models.PlaylistGroupCreateRequest
	raw, err := bindDocument(c, &req)
	if err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	req.Raw = raw
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
	created(c, body.Raw)
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
	writeBytesIndividualGET(c, body.Raw)
}

// ReplacePlaylistGroup PUT /api/v1/playlist-groups/:id.
func (h *Handler) ReplacePlaylistGroup(c *gin.Context) {
	id := c.Param("id")
	var req models.PlaylistGroupReplaceRequest
	raw, intent, err := bindSignedReplace(c, &req)
	if err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	req.Raw = raw
	body, err := h.Exec.ReplacePlaylistGroup(c.Request.Context(), id, &req, intent)
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
	writeDocument(c, http.StatusOK, body.Raw)
}

// DeletePlaylistGroup DELETE /api/v1/playlist-groups/:id. Body is a signed delete-intent.
func (h *Handler) DeletePlaylistGroup(c *gin.Context) {
	id := c.Param("id")
	req, err := bindDeleteRequest(c)
	if err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := h.Exec.DeletePlaylistGroup(c.Request.Context(), id, req); err != nil {
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
	writeDocumentList(c, channelDocuments(bodies), next)
}

// CreateChannel POST /api/v1/channels.
func (h *Handler) CreateChannel(c *gin.Context) {
	var req models.ChannelCreateRequest
	raw, err := bindDocument(c, &req)
	if err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	req.Raw = raw
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
	created(c, body.Raw)
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
	writeBytesIndividualGET(c, body.Raw)
}

// ReplaceChannel PUT /api/v1/channels/:id.
func (h *Handler) ReplaceChannel(c *gin.Context) {
	id := c.Param("id")
	var req models.ChannelReplaceRequest
	raw, intent, err := bindSignedReplace(c, &req)
	if err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	req.Raw = raw
	body, err := h.Exec.ReplaceChannel(c.Request.Context(), id, &req, intent)
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
	writeDocument(c, http.StatusOK, body.Raw)
}

// DeleteChannel DELETE /api/v1/channels/:id. Body is a signed delete-intent.
func (h *Handler) DeleteChannel(c *gin.Context) {
	id := c.Param("id")
	req, err := bindDeleteRequest(c)
	if err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := h.Exec.DeleteChannel(c.Request.Context(), id, req); err != nil {
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
