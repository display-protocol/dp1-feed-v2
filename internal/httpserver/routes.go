package httpserver

// Route registration: /health, /api/v1/* ; mutating routes (POST/PUT/PATCH/DELETE) use APIKeyAuth. Channel routes register only when extensions are enabled.

import (
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/display-protocol/dp1-feed-v2/internal/config"
)

// RegisterRoutes attaches all HTTP routes to the Gin engine.
// POST routes use SignatureOrAPIKeyAuth (dual auth: API key or signatures); PUT/PATCH/DELETE use APIKeyAuth only.
func RegisterRoutes(r *gin.Engine, h *Handler, cfg *config.Config, log *zap.Logger) {
	r.GET("/health", h.Health)

	v1 := r.Group("/api/v1")
	{
		v1.GET("", h.APIInfo)
		v1.GET("/health", h.HealthAPI)

		v1.GET("/playlists", h.ListPlaylists)
		v1.GET("/playlists/:id", h.GetPlaylist)
		v1.POST("/playlists", SignatureOrAPIKeyAuth(cfg.Auth.APIKey, log), h.CreatePlaylist)
		v1.PUT("/playlists/:id", APIKeyAuth(cfg.Auth.APIKey, log), h.ReplacePlaylist)
		v1.PATCH("/playlists/:id", APIKeyAuth(cfg.Auth.APIKey, log), h.UpdatePlaylist)
		v1.DELETE("/playlists/:id", APIKeyAuth(cfg.Auth.APIKey, log), h.DeletePlaylist)

		v1.GET("/playlist-groups", h.ListPlaylistGroups)
		v1.GET("/playlist-groups/:id", h.GetPlaylistGroup)
		v1.POST("/playlist-groups", SignatureOrAPIKeyAuth(cfg.Auth.APIKey, log), h.CreatePlaylistGroup)
		v1.PUT("/playlist-groups/:id", APIKeyAuth(cfg.Auth.APIKey, log), h.ReplacePlaylistGroup)
		v1.PATCH("/playlist-groups/:id", APIKeyAuth(cfg.Auth.APIKey, log), h.UpdatePlaylistGroup)
		v1.DELETE("/playlist-groups/:id", APIKeyAuth(cfg.Auth.APIKey, log), h.DeletePlaylistGroup)

		if cfg.Extensions.Enabled {
			v1.GET("/channels", h.ListChannels)
			v1.GET("/channels/:id", h.GetChannel)
			v1.POST("/channels", SignatureOrAPIKeyAuth(cfg.Auth.APIKey, log), h.CreateChannel)
			v1.PUT("/channels/:id", APIKeyAuth(cfg.Auth.APIKey, log), h.ReplaceChannel)
			v1.PATCH("/channels/:id", APIKeyAuth(cfg.Auth.APIKey, log), h.UpdateChannel)
			v1.DELETE("/channels/:id", APIKeyAuth(cfg.Auth.APIKey, log), h.DeleteChannel)
		} else {
			v1.GET("/channels", extensionsDisabled)
			v1.GET("/channels/:id", extensionsDisabled)
			v1.POST("/channels", extensionsDisabled)
			v1.PUT("/channels/:id", extensionsDisabled)
			v1.PATCH("/channels/:id", extensionsDisabled)
			v1.DELETE("/channels/:id", extensionsDisabled)
		}

		v1.GET("/playlist-items", h.ListPlaylistItems)
		v1.GET("/playlist-items/:id", h.GetPlaylistItem)

		v1.GET("/registry/channels", h.GetChannelRegistry)
		v1.PUT("/registry/channels", APIKeyAuth(cfg.Auth.APIKey, log), h.ReplaceChannelRegistry)
	}
}

// extensionsDisabled is bound to all /channels routes when cfg.Extensions.Enabled is false.
func extensionsDisabled(c *gin.Context) {
	c.JSON(404, gin.H{"error": "extensions_disabled", "message": "DP-1 extensions are disabled on this deployment"})
}
