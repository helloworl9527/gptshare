package httpapi

import (
	"database/sql"
	"errors"
	"net/http"

	metricssvc "allocation-service/internal/metrics"
	"github.com/gin-gonic/gin"
)

// passwordChangeDueHandler lists accounts whose passenger subscription has
// already expired while their own GPT subscription still has more than three
// days left — the operator's cue to change those passwords.
func passwordChangeDueHandler(service *metricssvc.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		accounts, err := service.PasswordChangeDue(c.Request.Context())
		if err != nil {
			writeError(c, http.StatusInternalServerError, "internal_error", "request could not be processed")
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"accounts":   accounts,
			"total":      len(accounts),
			"request_id": c.GetString("request_id"),
		})
	}
}

// clearPasswordChangeHandler marks an account as already handled so the flag
// stops listing it until a later card expiry re-arms the condition.
func clearPasswordChangeHandler(service *metricssvc.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := accountIDParam(c)
		if !ok {
			return
		}
		if err := service.MarkPasswordChangeCleared(c.Request.Context(), id); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(c, http.StatusNotFound, "not_found", "account not found")
				return
			}
			writeError(c, http.StatusInternalServerError, "internal_error", "request could not be processed")
			return
		}
		c.JSON(http.StatusOK, gin.H{"id": id, "cleared": true, "request_id": c.GetString("request_id")})
	}
}