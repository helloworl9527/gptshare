package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"chatgpt-monitor/internal/account"
	"chatgpt-monitor/internal/auth"
	"github.com/gin-gonic/gin"
)

type AccountService interface {
	ImportByToken(context.Context, *account.TokenInput) (account.Account, error)
	ReauthorizeByToken(context.Context, int64, *account.TokenInput) (account.Account, error)
	Delete(context.Context, int64) error
	List(context.Context) ([]account.Account, error)
	Get(context.Context, int64) (account.Account, error)
	StartDeviceImport(context.Context, string) (account.DeviceStart, error)
	StartDeviceReauthorization(context.Context, int64) (account.DeviceStart, error)
	PollDevice(context.Context, string) (account.DevicePoll, error)
}

type OAuthAccountService interface {
	StartOAuthImport(context.Context, string) (account.OAuthStart, error)
	StartOAuthReauthorization(context.Context, int64) (account.OAuthStart, error)
	CompleteOAuth(context.Context, string, string) (account.Account, error)
}

type BatchAccountService interface {
	ImportTokenBatch(context.Context, *account.BatchTokenInput) (account.BatchTokenResult, error)
}

func registerAccountRoutes(router *gin.Engine, manager *auth.Manager, service AccountService, cfg Config) {
	router.GET("/api/accounts", requireSession(manager), func(c *gin.Context) {
		accounts, err := service.List(c.Request.Context())
		if err != nil {
			writeAccountError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"accounts": accounts})
	})
	router.GET("/api/accounts/:id", requireSession(manager), func(c *gin.Context) {
		id, ok := accountID(c)
		if !ok {
			return
		}
		result, err := service.Get(c.Request.Context(), id)
		if err != nil {
			writeAccountError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	router.POST("/api/accounts/import/token", requireSession(manager), requireOrigin(cfg.Origin), requireSessionCSRF(manager), func(c *gin.Context) {
		var input account.TokenInput
		if !bindJSON(c, &input, 64<<10) {
			return
		}
		result, err := service.ImportByToken(c.Request.Context(), &input)
		clearTokenInput(&input)
		if err != nil {
			writeAccountError(c, err)
			return
		}
		c.JSON(http.StatusCreated, result)
	})
	if batchService, ok := service.(BatchAccountService); ok {
		router.POST("/api/accounts/import/token/batch", requireSession(manager), requireOrigin(cfg.Origin), requireSessionCSRF(manager), func(c *gin.Context) {
			var input account.BatchTokenInput
			if !bindJSON(c, &input, 1<<20) {
				return
			}
			result, err := batchService.ImportTokenBatch(c.Request.Context(), &input)
			if err != nil {
				writeAccountError(c, err)
				return
			}
			c.JSON(http.StatusOK, result)
		})
	}
	router.POST("/api/accounts/:id/reauthorize/token", requireSession(manager), requireOrigin(cfg.Origin), requireSessionCSRF(manager), func(c *gin.Context) {
		id, ok := accountID(c)
		if !ok {
			return
		}
		var input account.TokenInput
		if !bindJSON(c, &input, 64<<10) {
			return
		}
		result, err := service.ReauthorizeByToken(c.Request.Context(), id, &input)
		clearTokenInput(&input)
		if err != nil {
			writeAccountError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	router.POST("/api/accounts/import/device/start", requireSession(manager), requireOrigin(cfg.Origin), requireSessionCSRF(manager), func(c *gin.Context) {
		var request struct {
			Label string `json:"label" binding:"max=256"`
		}
		if !bindJSON(c, &request, 4096) {
			return
		}
		result, err := service.StartDeviceImport(c.Request.Context(), request.Label)
		if err != nil {
			writeAccountError(c, err)
			return
		}
		c.JSON(http.StatusCreated, result)
	})
	router.POST("/api/accounts/:id/reauthorize/device/start", requireSession(manager), requireOrigin(cfg.Origin), requireSessionCSRF(manager), func(c *gin.Context) {
		id, ok := accountID(c)
		if !ok {
			return
		}
		var request struct{}
		if !bindJSON(c, &request, 1024) {
			return
		}
		result, err := service.StartDeviceReauthorization(c.Request.Context(), id)
		if err != nil {
			writeAccountError(c, err)
			return
		}
		c.JSON(http.StatusCreated, result)
	})
	router.POST("/api/accounts/import/device/:id/poll", requireSession(manager), requireOrigin(cfg.Origin), requireSessionCSRF(manager), func(c *gin.Context) {
		var request struct{}
		if !bindJSON(c, &request, 1024) {
			return
		}
		result, err := service.PollDevice(c.Request.Context(), c.Param("id"))
		if err != nil {
			writeAccountError(c, err)
			return
		}
		c.JSON(http.StatusOK, result)
	})
	if oauthService, ok := service.(OAuthAccountService); ok {
		router.POST("/api/accounts/import/oauth/start", requireSession(manager), requireOrigin(cfg.Origin), requireSessionCSRF(manager), func(c *gin.Context) {
			var request struct {
				Label string `json:"label" binding:"max=256"`
			}
			if !bindJSON(c, &request, 4096) {
				return
			}
			result, err := oauthService.StartOAuthImport(c.Request.Context(), request.Label)
			if err != nil {
				writeAccountError(c, err)
				return
			}
			c.JSON(http.StatusCreated, result)
		})
		router.POST("/api/accounts/:id/reauthorize/oauth/start", requireSession(manager), requireOrigin(cfg.Origin), requireSessionCSRF(manager), func(c *gin.Context) {
			id, ok := accountID(c)
			if !ok {
				return
			}
			var request struct{}
			if !bindJSON(c, &request, 1024) {
				return
			}
			result, err := oauthService.StartOAuthReauthorization(c.Request.Context(), id)
			if err != nil {
				writeAccountError(c, err)
				return
			}
			c.JSON(http.StatusCreated, result)
		})
		router.POST("/api/accounts/oauth/:id/complete", requireSession(manager), requireOrigin(cfg.Origin), requireSessionCSRF(manager), func(c *gin.Context) {
			var request struct {
				CallbackURL string `json:"callback_url" binding:"required,max=8192"`
			}
			if !bindJSON(c, &request, 9<<10) {
				return
			}
			result, err := oauthService.CompleteOAuth(c.Request.Context(), c.Param("id"), request.CallbackURL)
			request.CallbackURL = ""
			if err != nil {
				writeAccountError(c, err)
				return
			}
			c.JSON(http.StatusOK, result)
		})
	}
	router.DELETE("/api/accounts/:id", requireSession(manager), requireOrigin(cfg.Origin), requireSessionCSRF(manager), func(c *gin.Context) {
		id, ok := accountID(c)
		if !ok {
			return
		}
		if err := service.Delete(c.Request.Context(), id); err != nil {
			writeAccountError(c, err)
			return
		}
		c.Status(http.StatusNoContent)
	})
}

func accountID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusUnprocessableEntity, "validation_failed", "account id is invalid")
		return 0, false
	}
	return id, true
}

func writeAccountError(c *gin.Context, err error) {
	var serviceErr *account.ServiceError
	if !errors.As(err, &serviceErr) {
		writeError(c, http.StatusInternalServerError, "internal_error", "request could not be processed")
		return
	}
	switch serviceErr.Kind {
	case account.ErrorInvalid:
		writeError(c, http.StatusUnprocessableEntity, serviceErr.Code, "credential validation failed")
	case account.ErrorDuplicate:
		writeError(c, http.StatusConflict, serviceErr.Code, "account already exists")
	case account.ErrorNotFound:
		writeError(c, http.StatusNotFound, serviceErr.Code, "account not found")
	case account.ErrorUnavailable:
		writeError(c, http.StatusServiceUnavailable, serviceErr.Code, "credential provider temporarily unavailable")
	default:
		writeError(c, http.StatusInternalServerError, "internal_error", "request could not be processed")
	}
}

func clearTokenInput(input *account.TokenInput) {
	input.AccessToken = ""
	input.RefreshToken = ""
	input.SessionToken = ""
}
