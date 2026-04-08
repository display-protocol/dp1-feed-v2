package httpserver

// HTTP handlers: parse query/body, call executor, map errors to OpenAPI-style JSON (see ErrorResponse).

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/display-protocol/dp1-feed-v2/internal/executor"
	"github.com/display-protocol/dp1-feed-v2/internal/models"
	"github.com/display-protocol/dp1-feed-v2/internal/publisherauth"
	"github.com/display-protocol/dp1-feed-v2/internal/store"
	"github.com/display-protocol/dp1-go/extension/identity"
)

// Handler carries the executor, logger, and build version for health/metadata responses.
type Handler struct {
	Exec               executor.Executor
	Log                *zap.Logger
	Version            string
	Publisher          publisherauth.Authorizer
	PublisherAuth      publisherauth.Service
	SessionCookieName  string
	CeremonyCookieName string
	SessionTTL         time.Duration
	CeremonyTTL        time.Duration
}

// channelURLPattern matches /api/v1/channels/{uuid} at the end of a URL.
var channelURLPattern = regexp.MustCompile(`^https?://.*\/api\/v1\/channels\/[0-9a-f-]{36}$`)

// isValidChannelURL checks if a URL matches the channel URL pattern.
func isValidChannelURL(url string) bool {
	return channelURLPattern.MatchString(url)
}

type beginPublisherRegistrationRequest struct {
	DisplayName string `json:"displayName"`
}

type beginLocalPublisherSessionRequest struct {
	DisplayName string `json:"displayName"`
}

type finishPublisherRegistrationRequest struct {
	Credential json.RawMessage `json:"credential"`
}

type finishPublisherLoginRequest struct {
	Credential json.RawMessage `json:"credential"`
}

type beginWalletProofRequest struct {
	Address string `json:"address"`
}

type finishWalletProofRequest struct {
	Signature string `json:"signature"`
}

type verifyENSProofRequest struct {
	Name string `json:"name"`
}

func (h *Handler) RequirePublisherSession(c *gin.Context) {
	if h.PublisherAuth == nil {
		c.AbortWithStatusJSON(http.StatusNotFound, ErrorResponse{Error: "not_found", Message: "publisher auth is not configured"})
		return
	}
	sessionToken, err := c.Cookie(h.SessionCookieName)
	if err != nil || strings.TrimSpace(sessionToken) == "" {
		c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorResponse{Error: "unauthorized", Message: "publisher session required"})
		return
	}
	principal, err := h.PublisherAuth.LookupSession(c.Request.Context(), sessionToken)
	if err != nil || principal == nil {
		c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorResponse{Error: "unauthorized", Message: "publisher session required"})
		return
	}
	c.Set(authPrincipalContextKey, authPrincipal{
		Kind:         principalPublisher,
		Name:         principal.DisplayName,
		PublisherKey: principal.PublisherKey,
		AccountID:    principal.AccountID.String(),
		ProofCount:   principal.ProofCount,
	})
	c.Next()
}

func (h *Handler) setSessionCookie(c *gin.Context, token string) {
	if c == nil {
		return
	}
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(h.SessionCookieName, token, int(h.SessionTTL.Seconds()), "/", "", false, true)
}

func (h *Handler) setCeremonyCookie(c *gin.Context, token string) {
	if c == nil {
		return
	}
	c.SetSameSite(http.SameSiteLaxMode)
	c.SetCookie(h.CeremonyCookieName, token, int(h.CeremonyTTL.Seconds()), "/", "", false, true)
}

func (h *Handler) clearSessionCookie(c *gin.Context) {
	if c == nil {
		return
	}
	c.SetCookie(h.SessionCookieName, "", -1, "/", "", false, true)
}

func (h *Handler) clearCeremonyCookie(c *gin.Context) {
	if c == nil {
		return
	}
	c.SetCookie(h.CeremonyCookieName, "", -1, "/", "", false, true)
}

func currentPublisherAccountID(c *gin.Context) (uuid.UUID, bool) {
	principal := currentPrincipal(c)
	if principal == nil || strings.TrimSpace(principal.AccountID) == "" {
		return uuid.UUID{}, false
	}
	id, err := uuid.Parse(principal.AccountID)
	if err != nil {
		return uuid.UUID{}, false
	}
	return id, true
}

func (h *Handler) authorizePublisherChannelWrite(c *gin.Context, channelRef string, nextPublisherKey string, requireReplacePublisher bool) bool {
	if !isPublisherPrincipal(c) {
		return true
	}
	principal, ok := requirePublisherPrincipal(c)
	if !ok {
		return false
	}
	if principal.ProofCount < 1 {
		writeForbidden(c, "publisher account must link at least one verified proof before publishing")
		return false
	}
	if h.Publisher == nil {
		writeForbidden(c, "publisher authorization is not configured")
		return false
	}
	allowed, err := h.Publisher.CanManageChannel(c.Request.Context(), channelRef, principal.PublisherKey)
	if err != nil {
		writeMappedError(c, err)
		return false
	}
	if !allowed {
		writeForbidden(c, "publisher does not control this channel")
		return false
	}
	if requireReplacePublisher && strings.TrimSpace(nextPublisherKey) == "" {
		writeForbidden(c, "publisher replacement must preserve publisher.key")
		return false
	}
	if strings.TrimSpace(nextPublisherKey) != "" && strings.TrimSpace(nextPublisherKey) != principal.PublisherKey {
		writeForbidden(c, "publisher key cannot be reassigned")
		return false
	}
	return true
}

func (h *Handler) authorizePublisherPlaylistWrite(c *gin.Context, playlistRef string) bool {
	if !isPublisherPrincipal(c) {
		return true
	}
	principal, ok := requirePublisherPrincipal(c)
	if !ok {
		return false
	}
	if principal.ProofCount < 1 {
		writeForbidden(c, "publisher account must link at least one verified proof before publishing")
		return false
	}
	if h.Publisher == nil {
		writeForbidden(c, "publisher authorization is not configured")
		return false
	}
	allowed, err := h.Publisher.CanManagePlaylist(c.Request.Context(), playlistRef, principal.PublisherKey)
	if err != nil {
		writeMappedError(c, err)
		return false
	}
	if !allowed {
		writeForbidden(c, "publisher does not control this playlist")
		return false
	}
	return true
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

// BeginPublisherRegistration starts a passkey registration ceremony for a new publisher account.
func (h *Handler) BeginPublisherRegistration(c *gin.Context) {
	if h.PublisherAuth == nil {
		writeError(c.Writer, http.StatusNotFound, "not_found", "publisher auth is not configured")
		return
	}
	var req beginPublisherRegistrationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	options, ceremonyToken, err := h.PublisherAuth.BeginRegistration(c.Request.Context(), req.DisplayName)
	if err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	h.setCeremonyCookie(c, ceremonyToken)
	c.JSON(http.StatusOK, options)
}

// BeginLocalPublisherSession creates or reuses a local publisher account and issues a browser session.
// This is intended only for local debug/testing flows where passkeys are getting in the way.
func (h *Handler) BeginLocalPublisherSession(c *gin.Context) {
	if h.PublisherAuth == nil {
		writeError(c.Writer, http.StatusNotFound, "not_found", "publisher auth is not configured")
		return
	}
	var req beginLocalPublisherSessionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	principal, sessionToken, err := h.PublisherAuth.BootstrapLocalSession(c.Request.Context(), req.DisplayName)
	if err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	h.setSessionCookie(c, sessionToken)
	c.JSON(http.StatusOK, gin.H{
		"publisherKey": principal.PublisherKey,
		"displayName":  principal.DisplayName,
		"proofCount":   principal.ProofCount,
	})
}

// FinishPublisherRegistration completes a passkey registration ceremony and creates a browser session.
func (h *Handler) FinishPublisherRegistration(c *gin.Context) {
	if h.PublisherAuth == nil {
		writeError(c.Writer, http.StatusNotFound, "not_found", "publisher auth is not configured")
		return
	}
	ceremonyToken, err := c.Cookie(h.CeremonyCookieName)
	if err != nil || strings.TrimSpace(ceremonyToken) == "" {
		writeError(c.Writer, http.StatusUnauthorized, "unauthorized", "publisher ceremony required")
		return
	}
	var req finishPublisherRegistrationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	principal, sessionToken, err := h.PublisherAuth.FinishRegistration(c.Request.Context(), ceremonyToken, req.Credential)
	if err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	h.clearCeremonyCookie(c)
	h.setSessionCookie(c, sessionToken)
	c.JSON(http.StatusOK, gin.H{
		"publisherKey": principal.PublisherKey,
		"displayName":  principal.DisplayName,
		"proofCount":   principal.ProofCount,
	})
}

// BeginPublisherLogin starts a discoverable passkey login ceremony.
func (h *Handler) BeginPublisherLogin(c *gin.Context) {
	if h.PublisherAuth == nil {
		writeError(c.Writer, http.StatusNotFound, "not_found", "publisher auth is not configured")
		return
	}
	options, ceremonyToken, err := h.PublisherAuth.BeginLogin(c.Request.Context())
	if err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	h.setCeremonyCookie(c, ceremonyToken)
	c.JSON(http.StatusOK, options)
}

// FinishPublisherLogin completes a passkey login ceremony and issues a browser session.
func (h *Handler) FinishPublisherLogin(c *gin.Context) {
	if h.PublisherAuth == nil {
		writeError(c.Writer, http.StatusNotFound, "not_found", "publisher auth is not configured")
		return
	}
	ceremonyToken, err := c.Cookie(h.CeremonyCookieName)
	if err != nil || strings.TrimSpace(ceremonyToken) == "" {
		writeError(c.Writer, http.StatusUnauthorized, "unauthorized", "publisher ceremony required")
		return
	}
	var req finishPublisherLoginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	principal, sessionToken, err := h.PublisherAuth.FinishLogin(c.Request.Context(), ceremonyToken, req.Credential)
	if err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	h.clearCeremonyCookie(c)
	h.setSessionCookie(c, sessionToken)
	c.JSON(http.StatusOK, gin.H{
		"publisherKey": principal.PublisherKey,
		"displayName":  principal.DisplayName,
		"proofCount":   principal.ProofCount,
	})
}

// PublisherLogout removes the browser publisher session.
func (h *Handler) PublisherLogout(c *gin.Context) {
	if h.PublisherAuth == nil {
		writeError(c.Writer, http.StatusNotFound, "not_found", "publisher auth is not configured")
		return
	}
	token, _ := c.Cookie(h.SessionCookieName)
	_ = h.PublisherAuth.DeleteSession(c.Request.Context(), token)
	h.clearSessionCookie(c)
	c.Status(http.StatusNoContent)
}

// GetPublisherMe returns the authenticated publisher account and linked proofs.
func (h *Handler) GetPublisherMe(c *gin.Context) {
	if h.PublisherAuth == nil {
		writeError(c.Writer, http.StatusNotFound, "not_found", "publisher auth is not configured")
		return
	}
	accountID, ok := currentPublisherAccountID(c)
	if !ok {
		writeError(c.Writer, http.StatusUnauthorized, "unauthorized", "publisher session required")
		return
	}
	account, err := h.PublisherAuth.GetAccount(c.Request.Context(), accountID)
	if err != nil {
		writeError(c.Writer, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	c.JSON(http.StatusOK, account)
}

// BeginPublisherWalletProof creates a wallet-signing challenge for the authenticated publisher account.
func (h *Handler) BeginPublisherWalletProof(c *gin.Context) {
	if h.PublisherAuth == nil {
		writeError(c.Writer, http.StatusNotFound, "not_found", "publisher auth is not configured")
		return
	}
	accountID, ok := currentPublisherAccountID(c)
	if !ok {
		writeError(c.Writer, http.StatusUnauthorized, "unauthorized", "publisher session required")
		return
	}
	var req beginWalletProofRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	message, ceremonyToken, err := h.PublisherAuth.BeginWalletProof(c.Request.Context(), accountID, req.Address)
	if err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	h.setCeremonyCookie(c, ceremonyToken)
	c.JSON(http.StatusOK, gin.H{"message": message})
}

// FinishPublisherWalletProof verifies the wallet signature and stores the linked proof.
func (h *Handler) FinishPublisherWalletProof(c *gin.Context) {
	if h.PublisherAuth == nil {
		writeError(c.Writer, http.StatusNotFound, "not_found", "publisher auth is not configured")
		return
	}
	accountID, ok := currentPublisherAccountID(c)
	if !ok {
		writeError(c.Writer, http.StatusUnauthorized, "unauthorized", "publisher session required")
		return
	}
	ceremonyToken, err := c.Cookie(h.CeremonyCookieName)
	if err != nil || strings.TrimSpace(ceremonyToken) == "" {
		writeError(c.Writer, http.StatusUnauthorized, "unauthorized", "publisher ceremony required")
		return
	}
	var req finishWalletProofRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	proof, err := h.PublisherAuth.FinishWalletProof(c.Request.Context(), accountID, ceremonyToken, req.Signature)
	if err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	h.clearCeremonyCookie(c)
	c.JSON(http.StatusOK, proof)
}

// VerifyPublisherENSProof resolves an ENS name and links it if it matches a verified wallet proof.
func (h *Handler) VerifyPublisherENSProof(c *gin.Context) {
	if h.PublisherAuth == nil {
		writeError(c.Writer, http.StatusNotFound, "not_found", "publisher auth is not configured")
		return
	}
	accountID, ok := currentPublisherAccountID(c)
	if !ok {
		writeError(c.Writer, http.StatusUnauthorized, "unauthorized", "publisher session required")
		return
	}
	var req verifyENSProofRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	proof, err := h.PublisherAuth.VerifyENSProof(c.Request.Context(), accountID, req.Name)
	if err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	c.JSON(http.StatusOK, proof)
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
	c.JSON(http.StatusOK, NewListResponse(pl, next))
}

// CreatePlaylist POST /api/v1/playlists.
func (h *Handler) CreatePlaylist(c *gin.Context) {
	var req models.PlaylistCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c.Writer, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if isPublisherPrincipal(c) {
		principal, ok := requirePublisherPrincipal(c)
		if !ok {
			return
		}
		if principal.ProofCount < 1 {
			writeForbidden(c, "publisher account must link at least one verified proof before publishing")
			return
		}
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
	if !h.authorizePublisherPlaylistWrite(c, id) {
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
	if !h.authorizePublisherPlaylistWrite(c, id) {
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
	if !requireOperator(c) {
		return
	}
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
	if !requireOperator(c) {
		return
	}
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
	if !requireOperator(c) {
		return
	}
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
	if !requireOperator(c) {
		return
	}
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
	if !requireOperator(c) {
		return
	}
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
	if isPublisherPrincipal(c) {
		principal, ok := requirePublisherPrincipal(c)
		if !ok {
			return
		}
		if principal.ProofCount < 1 {
			writeForbidden(c, "publisher account must link at least one verified proof before publishing")
			return
		}
		if req.Publisher == nil || strings.TrimSpace(req.Publisher.Key) == "" {
			req.Publisher = &identity.Entity{Name: principal.Name, Key: principal.PublisherKey}
		} else if strings.TrimSpace(req.Publisher.Key) != principal.PublisherKey {
			writeForbidden(c, "publisher key does not match authenticated publisher")
			return
		}
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
	nextPublisherKey := ""
	if req.Publisher != nil {
		nextPublisherKey = req.Publisher.Key
	}
	if !h.authorizePublisherChannelWrite(c, id, nextPublisherKey, true) {
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
	nextPublisherKey := ""
	if req.Publisher != nil {
		nextPublisherKey = req.Publisher.Key
	}
	if !h.authorizePublisherChannelWrite(c, id, nextPublisherKey, false) {
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
	if !h.authorizePublisherChannelWrite(c, id, "", false) {
		return
	}
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
	if !requireOperator(c) {
		return
	}
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
