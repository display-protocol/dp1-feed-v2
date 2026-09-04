package httpserver

// Route registration: /health, /api/v1/* . All mutating routes (POST/PUT/DELETE) are gated by
// RequireSignatures — there is no API key and no PATCH. The registry is read-only over the API.
// Channel routes register only when extensions are enabled.

import (
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/display-protocol/dp1-feed-v2/internal/config"
)

// RegisterRoutes attaches all HTTP routes to the Gin engine. Reads are public; every mutating route
// requires a signed body (RequireSignatures). PUT is owner-bound and owner-immutable, DELETE takes a
// signed delete-intent; both are enforced in the executor. See docs/api_design.md.
func RegisterRoutes(r *gin.Engine, h *Handler, cfg *config.Config, log *zap.Logger) {
	r.GET("/health", h.Health)

	v1 := r.Group("/api/v1")
	{
		v1.GET("", h.APIInfo)
		v1.GET("/health", h.HealthAPI)

		v1.GET("/playlists", h.ListPlaylists)
		v1.GET("/playlists/:id", h.GetPlaylist)
		v1.POST("/playlists", RequireSignatures(log), h.CreatePlaylist)
		v1.PUT("/playlists/:id", RequireSignatures(log), h.ReplacePlaylist)
		v1.DELETE("/playlists/:id", RequireSignatures(log), h.DeletePlaylist)

		// Reference-resolving mutations carry an aggregate deadline. Group and channel writes resolve every
		// playlist URI the document names, so their work is bounded by that budget as well as by the
		// per-fetch timeout and the reference cap: without it, a document naming many slow-but-reachable
		// hosts holds the handler for the sum of every fetch. Channel routes needed this already because
		// notification delivery requires a deadline; groups fan out the same way and were missing it.
		referenceMutationDeadline := RequestDeadline(cfg.Server.WriteTimeout - cfg.Server.ResponseWriteReserve)

		v1.GET("/playlist-groups", h.ListPlaylistGroups)
		v1.GET("/playlist-groups/:id", h.GetPlaylistGroup)
		v1.POST("/playlist-groups", referenceMutationDeadline, RequireSignatures(log), h.CreatePlaylistGroup)
		v1.PUT("/playlist-groups/:id", referenceMutationDeadline, RequireSignatures(log), h.ReplacePlaylistGroup)
		v1.DELETE("/playlist-groups/:id", RequireSignatures(log), h.DeletePlaylistGroup)

		if cfg.Extensions.Enabled {
			channelMutationDeadline := referenceMutationDeadline
			v1.GET("/channels", h.ListChannels)
			v1.GET("/channels/:id", h.GetChannel)
			v1.POST("/channels", channelMutationDeadline, RequireSignatures(log), h.CreateChannel)
			v1.PUT("/channels/:id", channelMutationDeadline, RequireSignatures(log), h.ReplaceChannel)
			v1.DELETE("/channels/:id", channelMutationDeadline, RequireSignatures(log), h.DeleteChannel)
		} else {
			v1.GET("/channels", extensionsDisabled)
			v1.GET("/channels/:id", extensionsDisabled)
			v1.POST("/channels", extensionsDisabled)
			v1.PUT("/channels/:id", extensionsDisabled)
			v1.DELETE("/channels/:id", extensionsDisabled)
		}

		v1.GET("/playlist-items", h.ListPlaylistItems)
		v1.GET("/playlist-items/:id", h.GetPlaylistItem)

		v1.GET("/registry/channels", h.GetChannelRegistry)
	}
}

// extensionsDisabled is bound to all /channels routes when cfg.Extensions.Enabled is false.
func extensionsDisabled(c *gin.Context) {
	c.JSON(404, gin.H{"error": "extensions_disabled", "message": "DP-1 extensions are disabled on this deployment"})
}
