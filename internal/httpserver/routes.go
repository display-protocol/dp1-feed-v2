package httpserver

// Route registration: /health, /api/v1/* ; mutating routes (POST/PUT/PATCH/DELETE) use APIKeyAuth. Channel routes register only when extensions are enabled.

import (
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/display-protocol/dp1-feed-v2/internal/config"
)

// RegisterRoutes attaches all HTTP routes to the Gin engine.
func RegisterRoutes(r *gin.Engine, h *Handler, cfg *config.Config, log *zap.Logger) {
	r.GET("/health", h.Health)
	r.GET("/publisher", h.PublisherConsolePage)
	admin := r.Group("/admin", WriteAuth(cfg, h.PublisherAuth, log))
	{
		admin.GET("/intermission-notes", h.IntermissionAdminPage)
		admin.POST("/intermission-notes", h.IntermissionAdminSubmit)
	}

	v1 := r.Group("/api/v1")
	{
		v1.GET("", h.APIInfo)
		v1.GET("/health", h.HealthAPI)
		v1.POST("/publisher/register/options", h.BeginPublisherRegistration)
		v1.POST("/publisher/register/verify", h.FinishPublisherRegistration)
		v1.POST("/publisher/local-session", h.BeginLocalPublisherSession)
		v1.POST("/publisher/login/options", h.BeginPublisherLogin)
		v1.POST("/publisher/login/verify", h.FinishPublisherLogin)
		v1.POST("/publisher/logout", h.PublisherLogout)
		v1.GET("/publisher/me", h.RequirePublisherSession, h.GetPublisherMe)
		v1.POST("/publisher/proofs/wallet/challenge", h.RequirePublisherSession, h.BeginPublisherWalletProof)
		v1.POST("/publisher/proofs/wallet/verify", h.RequirePublisherSession, h.FinishPublisherWalletProof)
		v1.POST("/publisher/proofs/ens/verify", h.RequirePublisherSession, h.VerifyPublisherENSProof)

		v1.GET("/playlists", h.ListPlaylists)
		v1.GET("/playlists/:id", h.GetPlaylist)
		v1.POST("/playlists", WriteAuth(cfg, h.PublisherAuth, log), h.CreatePlaylist)
		v1.PUT("/playlists/:id", WriteAuth(cfg, h.PublisherAuth, log), h.ReplacePlaylist)
		v1.PATCH("/playlists/:id", WriteAuth(cfg, h.PublisherAuth, log), h.UpdatePlaylist)
		v1.DELETE("/playlists/:id", WriteAuth(cfg, h.PublisherAuth, log), h.DeletePlaylist)

		v1.GET("/playlist-groups", h.ListPlaylistGroups)
		v1.GET("/playlist-groups/:id", h.GetPlaylistGroup)
		v1.POST("/playlist-groups", WriteAuth(cfg, h.PublisherAuth, log), h.CreatePlaylistGroup)
		v1.PUT("/playlist-groups/:id", WriteAuth(cfg, h.PublisherAuth, log), h.ReplacePlaylistGroup)
		v1.PATCH("/playlist-groups/:id", WriteAuth(cfg, h.PublisherAuth, log), h.UpdatePlaylistGroup)
		v1.DELETE("/playlist-groups/:id", WriteAuth(cfg, h.PublisherAuth, log), h.DeletePlaylistGroup)

		if cfg.Extensions.Enabled {
			v1.GET("/channels", h.ListChannels)
			v1.GET("/channels/:id", h.GetChannel)
			v1.POST("/channels", WriteAuth(cfg, h.PublisherAuth, log), h.CreateChannel)
			v1.PUT("/channels/:id", WriteAuth(cfg, h.PublisherAuth, log), h.ReplaceChannel)
			v1.PATCH("/channels/:id", WriteAuth(cfg, h.PublisherAuth, log), h.UpdateChannel)
			v1.DELETE("/channels/:id", WriteAuth(cfg, h.PublisherAuth, log), h.DeleteChannel)
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
		v1.PUT("/registry/channels", WriteAuth(cfg, h.PublisherAuth, log), h.ReplaceChannelRegistry)
	}
}

// extensionsDisabled is bound to all /channels routes when cfg.Extensions.Enabled is false.
func extensionsDisabled(c *gin.Context) {
	c.JSON(404, gin.H{"error": "extensions_disabled", "message": "DP-1 extensions are disabled on this deployment"})
}
