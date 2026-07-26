package httpapi

import (
	"context"
	"errors"
	"net/http"

	"chatgpt-monitor/internal/auth"
	"chatgpt-monitor/internal/monitor"
	"github.com/gin-gonic/gin"
)

type MonitorService interface {
	RefreshNow(context.Context, int64) (monitor.Run, bool, error)
	GetRun(context.Context, string) (monitor.Run, error)
}

func registerMonitorRoutes(router *gin.Engine, manager *auth.Manager, service MonitorService, cfg Config) {
	router.POST("/api/accounts/:id/refresh", requireSession(manager), requireOrigin(cfg.Origin), requireSessionCSRF(manager), func(c *gin.Context) {
		id, ok := accountID(c)
		if !ok {
			return
		}
		var request struct{}
		if !bindJSON(c, &request, 1024) {
			return
		}
		run, completed, err := service.RefreshNow(c.Request.Context(), id)
		if err != nil {
			var conflict *monitor.ConflictError
			var missing *monitor.NotFoundError
			var paused *monitor.PausedError
			switch {
			case errors.As(err, &conflict):
				c.JSON(http.StatusConflict, gin.H{"code": "refresh_in_progress", "message": "account refresh already running", "request_id": c.GetString("request_id"), "run_id": conflict.RunID})
			case errors.As(err, &missing):
				writeError(c, http.StatusNotFound, "account_not_found", "account not found")
			case errors.As(err, &paused):
				writeError(c, http.StatusConflict, "evidence_review_required", "account polling is paused for evidence review")
			default:
				writeError(c, http.StatusInternalServerError, "refresh_failed", "account refresh could not be started")
			}
			return
		}
		if completed {
			c.JSON(http.StatusOK, run)
			return
		}
		c.JSON(http.StatusAccepted, run)
	})
	router.GET("/api/poll-runs/:id", requireSession(manager), func(c *gin.Context) {
		run, err := service.GetRun(c.Request.Context(), c.Param("id"))
		if err != nil {
			var missing *monitor.NotFoundError
			if errors.As(err, &missing) {
				writeError(c, http.StatusNotFound, "poll_run_not_found", "poll run not found")
				return
			}
			writeError(c, http.StatusInternalServerError, "poll_run_failed", "poll run could not be read")
			return
		}
		c.JSON(http.StatusOK, run)
	})
}
