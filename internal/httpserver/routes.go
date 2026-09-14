package httpserver

// Route registration: /health, /api/v1/* . All mutating routes (POST/PUT/DELETE) are gated by
// RequireSignatures — there is no API key and no PATCH.
// Channel routes register only when extensions are enabled.

import (
	"net/http"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/display-protocol/dp1-feed-v2/internal/config"
)

// RegisterRoutes attaches all HTTP routes to the Gin engine. Reads are public; every mutating route
// requires a signed body (RequireSignatures). PUT is owner-bound (owners may be added, never removed),
// DELETE takes a signed delete-intent; both are enforced in the executor. See docs/api_design.md.
func RegisterRoutes(r *gin.Engine, h *Handler, cfg *config.Config, log *zap.Logger) {
	// Unmatched paths answer in the documented error shape rather than gin's plain-text default.
	//
	// Every other response this API produces is {"error","message"}, so a client parsing errors uniformly
	// hit a wall exactly where it least expects one. It matters more now that a documented endpoint has
	// been removed: a caller still requesting /api/v1/registry/channels should get the same machine-
	// readable not_found every other missing resource returns, not a bare string.
	r.NoRoute(func(c *gin.Context) {
		writeError(c.Writer, http.StatusNotFound, "not_found", "no such endpoint")
	})
	// No NoMethod handler on purpose: gin only consults one when HandleMethodNotAllowed is set, which it
	// is not, so a wrong method already falls through to NoRoute above. Registering one would be dead code,
	// and turning the flag on would change wrong-method responses from 404 to 405 across the whole API —
	// a separate decision from removing an endpoint.
	r.GET("/health", h.Health)

	v1 := r.Group("/api/v1")
	{
		v1.GET("", h.APIInfo)
		v1.GET("/health", h.HealthAPI)

		// Every mutation carries the same aggregate deadline. Group and channel writes need it to bound
		// reference resolution (a document naming many slow-but-reachable hosts would otherwise hold the
		// handler for the sum of every fetch); channel writes and playlist replace/delete need it because
		// they notify — a playlist write emits channel.updated for every channel that lists the playlist,
		// and the executor refuses a notified mutation without a deadline. Applying it uniformly, including
		// to routes that today neither fetch nor notify, is deliberate: "which routes carry the deadline"
		// used to be a per-route decision, and that is exactly how groups were missed once and playlists
		// were missed again. The budget is write_timeout - response_write_reserve, so the deadline lands
		// one reserve ahead of the socket write timeout it shadows.
		mutationDeadline := RequestDeadline(cfg.Server.WriteTimeout - cfg.Server.ResponseWriteReserve)

		v1.GET("/playlists", h.ListPlaylists)
		v1.GET("/playlists/:id", h.GetPlaylist)
		v1.POST("/playlists", mutationDeadline, RequireSignatures(log), h.CreatePlaylist)
		v1.PUT("/playlists/:id", mutationDeadline, RequireSignatures(log), h.ReplacePlaylist)
		v1.DELETE("/playlists/:id", mutationDeadline, RequireSignatures(log), h.DeletePlaylist)

		v1.GET("/playlist-groups", h.ListPlaylistGroups)
		v1.GET("/playlist-groups/:id", h.GetPlaylistGroup)
		v1.POST("/playlist-groups", mutationDeadline, RequireSignatures(log), h.CreatePlaylistGroup)
		v1.PUT("/playlist-groups/:id", mutationDeadline, RequireSignatures(log), h.ReplacePlaylistGroup)
		v1.DELETE("/playlist-groups/:id", mutationDeadline, RequireSignatures(log), h.DeletePlaylistGroup)

		if cfg.Extensions.Enabled {
			v1.GET("/channels", h.ListChannels)
			v1.GET("/channels/:id", h.GetChannel)
			v1.POST("/channels", mutationDeadline, RequireSignatures(log), h.CreateChannel)
			v1.PUT("/channels/:id", mutationDeadline, RequireSignatures(log), h.ReplaceChannel)
			v1.DELETE("/channels/:id", mutationDeadline, RequireSignatures(log), h.DeleteChannel)
		} else {
			v1.GET("/channels", extensionsDisabled)
			v1.GET("/channels/:id", extensionsDisabled)
			v1.POST("/channels", extensionsDisabled)
			v1.PUT("/channels/:id", extensionsDisabled)
			v1.DELETE("/channels/:id", extensionsDisabled)
		}

		v1.GET("/playlist-items", h.ListPlaylistItems)
		v1.GET("/playlist-items/:id", h.GetPlaylistItem)
	}
}

// extensionsDisabled is bound to all /channels routes when cfg.Extensions.Enabled is false.
func extensionsDisabled(c *gin.Context) {
	c.JSON(404, gin.H{"error": "extensions_disabled", "message": "DP-1 extensions are disabled on this deployment"})
}
