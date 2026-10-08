package httpapi

import (
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	metricssvc "allocation-service/internal/metrics"
	"github.com/gin-gonic/gin"
)

func expiredCardsHandler(service *metricssvc.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		page, pageErr := strconv.Atoi(c.DefaultQuery("page", "1"))
		pageSize, sizeErr := strconv.Atoi(c.DefaultQuery("page_size", "20"))
		search := strings.TrimSpace(c.Query("search"))
		if pageErr != nil || sizeErr != nil || page < 1 || page > 1000000 || pageSize < 1 || pageSize > 100 || utf8.RuneCountInString(search) > 256 {
			writeError(c, http.StatusUnprocessableEntity, "validation_failed", "request validation failed")
			return
		}
		result, err := service.ExpiredCards(c.Request.Context(), page, pageSize, search)
		if err != nil {
			writeError(c, http.StatusInternalServerError, "internal_error", "request could not be processed")
			return
		}
		c.JSON(http.StatusOK, gin.H{"cards": result.Cards, "total": result.Total, "page": result.Page, "page_size": result.PageSize, "request_id": c.GetString("request_id")})
	}
}
