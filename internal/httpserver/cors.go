package httpserver

import (
	"strings"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"

	"github.com/display-protocol/dp1-feed-v2/internal/config"
)

// newCORSMiddleware returns gin-contrib/cors configured from cfg.CORS. When AllowOrigins is empty or exactly
// "*", all origins are allowed. Otherwise only listed origins are permitted. Authorization is included in
// allowed preflight headers so browsers may send Bearer API keys cross-origin.
func newCORSMiddleware(cfg *config.Config) gin.HandlerFunc {
	cc := cors.DefaultConfig()
	origins := cfg.CORS.AllowOrigins
	wildcard := len(origins) == 0 || (len(origins) == 1 && strings.TrimSpace(origins[0]) == "*")
	if wildcard {
		cc.AllowAllOrigins = true
	} else {
		cc.AllowOrigins = origins
	}
	cc.AddAllowHeaders("Authorization")
	return cors.New(cc)
}
