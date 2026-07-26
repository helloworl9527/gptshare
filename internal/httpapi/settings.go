package httpapi

import (
	"context"
	"errors"
	"net/http"

	"chatgpt-monitor/internal/auth"
	"chatgpt-monitor/internal/notify"
	"github.com/gin-gonic/gin"
)

type SettingsService interface {
	Get(context.Context) (notify.Settings, error)
	Update(context.Context, notify.Update, string) (notify.Settings, error)
	DeleteSecret(context.Context, string, string) error
}

func registerSettingsRoutes(router *gin.Engine, manager *auth.Manager, service SettingsService, cfg Config) {
	router.GET("/api/settings", requireSession(manager), func(c *gin.Context) {
		settings, err := service.Get(c.Request.Context())
		if err != nil {
			writeError(c, http.StatusInternalServerError, "settings_read_failed", "settings could not be read")
			return
		}
		c.JSON(http.StatusOK, settings)
	})
	router.PUT("/api/settings", requireSession(manager), requireOrigin(cfg.Origin), requireSessionCSRF(manager), func(c *gin.Context) {
		var request notify.Update
		if !bindJSON(c, &request, 16*1024) {
			return
		}
		principal := c.MustGet("principal").(*auth.Principal)
		settings, err := service.Update(c.Request.Context(), request, principal.Username)
		if err != nil {
			var invalid *notify.SettingsError
			if errors.As(err, &invalid) {
				writeError(c, http.StatusUnprocessableEntity, invalid.Code, "settings validation failed")
				return
			}
			writeError(c, http.StatusInternalServerError, "settings_update_failed", "settings could not be updated")
			return
		}
		c.JSON(http.StatusOK, settings)
	})
	router.DELETE("/api/settings/channels/:channel/secret", requireSession(manager), requireOrigin(cfg.Origin), requireSessionCSRF(manager), func(c *gin.Context) {
		principal := c.MustGet("principal").(*auth.Principal)
		if err := service.DeleteSecret(c.Request.Context(), c.Param("channel"), principal.Username); err != nil {
			var invalid *notify.SettingsError
			if errors.As(err, &invalid) {
				writeError(c, http.StatusNotFound, invalid.Code, "channel not found")
				return
			}
			writeError(c, http.StatusInternalServerError, "settings_update_failed", "settings could not be updated")
			return
		}
		c.Status(http.StatusNoContent)
	})
}
