package httpapi

import (
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"chatgpt-monitor/internal/account"
	"github.com/gin-gonic/gin"
)

const (
	maxAllocationImportBody = 64 << 10
	maxAllocationBatchBody  = 16 << 10
	maxAllocationBatchSize  = 100
)

type allocationImportRequest struct {
	Label     string `json:"label" binding:"max=256"`
	Token     string `json:"token" binding:"required,max=16384"`
	TokenType string `json:"token_type" binding:"required,oneof=access_token refresh_token session_token access refresh session"`
}

type allocationBatchStatusRequest struct {
	ProviderAccountIDs []string `json:"provider_account_ids" binding:"required,min=1,max=100,dive,required,max=256"`
}

type allocationListItem struct {
	ProviderAccountID string    `json:"provider_account_id"`
	Email             *string   `json:"email,omitempty"`
	AuthExpiry        time.Time `json:"auth_expiry"`
	Status            string    `json:"status"`
	Plan              string    `json:"plan"`
}

type allocationStatusResponse struct {
	ProviderAccountID  string     `json:"provider_account_id"`
	Email              *string    `json:"email,omitempty"`
	Status             string     `json:"status"`
	Plan               string     `json:"plan"`
	AuthExpiry         time.Time  `json:"auth_expiry"`
	SubscriptionExpiry *time.Time `json:"subscription_expiry"`
	CheckedAt          time.Time  `json:"checked_at"`
}

type allocationBatchStatusItem struct {
	ProviderAccountID  string     `json:"provider_account_id"`
	Status             string     `json:"status,omitempty"`
	Plan               string     `json:"plan,omitempty"`
	SubscriptionExpiry *time.Time `json:"subscription_expiry,omitempty"`
	CheckedAt          *time.Time `json:"checked_at,omitempty"`
	Error              *itemError `json:"error,omitempty"`
}

type itemError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func registerAllocationRoutes(router *gin.Engine, service AccountService, cfg Config, logger *slog.Logger) {
	RegisterAllocationCompatibilityRoutes(router, service, cfg, logger)
}

// RegisterAllocationCompatibilityRoutes mounts the legacy allocation-facing
// monitor API. The modular monolith calls this only when its explicit temporary
// compatibility exception is enabled.
func RegisterAllocationCompatibilityRoutes(router *gin.Engine, service AccountService, cfg Config, logger *slog.Logger, middleware ...gin.HandlerFunc) {
	handlers := []gin.HandlerFunc{requireAllocationAPIKey(cfg.AllocationServiceAPIKey)}
	handlers = append(handlers, middleware...)
	group := router.Group("/api/v1/monitor/accounts", handlers...)
	group.GET("", func(c *gin.Context) {
		accounts, err := service.List(c.Request.Context())
		if err != nil {
			writeAccountError(c, err)
			return
		}
		if len(accounts) > maxAllocationBatchSize {
			accounts = accounts[:maxAllocationBatchSize]
		}
		items := make([]allocationListItem, 0, len(accounts))
		for _, item := range accounts {
			items = append(items, allocationListFromAccount(item))
		}
		logger.Info("allocation account list completed", "request_id", c.GetString("request_id"), "items", len(items))
		c.JSON(http.StatusOK, gin.H{"accounts": items, "request_id": c.GetString("request_id")})
	})
	group.POST("/import-for-allocation", func(c *gin.Context) {
		var request allocationImportRequest
		if !bindJSON(c, &request, maxAllocationImportBody) {
			return
		}
		input, ok := allocationTokenInput(request)
		if !ok {
			writeError(c, http.StatusUnprocessableEntity, "validation_failed", "request validation failed")
			return
		}
		result, err := service.ImportByToken(c.Request.Context(), &input)
		clearTokenInput(&input)
		request.Token = ""
		if err != nil {
			logger.Warn("allocation import failed", "request_id", c.GetString("request_id"), "error_code", allocationErrorCode(err))
			writeAccountError(c, err)
			return
		}
		logger.Info("allocation import completed", "request_id", c.GetString("request_id"), "status", result.Status)
		c.JSON(http.StatusCreated, allocationStatusFromAccount(result, time.Now().UTC()))
	})
	group.POST("/batch-status", func(c *gin.Context) {
		var request allocationBatchStatusRequest
		if !bindJSON(c, &request, maxAllocationBatchBody) {
			return
		}
		accounts, err := service.List(c.Request.Context())
		if err != nil {
			writeAccountError(c, err)
			return
		}
		now := time.Now().UTC()
		byProviderID := make(map[string]account.Account, len(accounts))
		for _, item := range accounts {
			byProviderID[item.ProviderAccountID] = item
		}
		items := make([]allocationBatchStatusItem, 0, len(request.ProviderAccountIDs))
		for _, id := range request.ProviderAccountIDs {
			id = strings.TrimSpace(id)
			if id == "" {
				items = append(items, allocationBatchStatusItem{ProviderAccountID: id, Error: &itemError{Code: "validation_failed", Message: "provider account id is invalid"}})
				continue
			}
			if result, ok := byProviderID[id]; ok {
				status := allocationStatusFromAccount(result, now)
				items = append(items, allocationBatchStatusItem{
					ProviderAccountID: status.ProviderAccountID, Status: status.Status, Plan: status.Plan,
					SubscriptionExpiry: status.SubscriptionExpiry, CheckedAt: &status.CheckedAt,
				})
				continue
			}
			items = append(items, allocationBatchStatusItem{ProviderAccountID: id, Error: &itemError{Code: "not_found", Message: "provider account not found"}})
		}
		logger.Info("allocation batch status completed", "request_id", c.GetString("request_id"), "items", len(items))
		c.JSON(http.StatusOK, gin.H{"items": items})
	})
	group.GET("/:provider_account_id/status", func(c *gin.Context) {
		providerID := strings.TrimSpace(c.Param("provider_account_id"))
		if providerID == "" || len(providerID) > 256 {
			writeError(c, http.StatusUnprocessableEntity, "validation_failed", "provider account id is invalid")
			return
		}
		accounts, err := service.List(c.Request.Context())
		if err != nil {
			writeAccountError(c, err)
			return
		}
		for _, item := range accounts {
			if item.ProviderAccountID == providerID {
				c.JSON(http.StatusOK, allocationStatusFromAccount(item, time.Now().UTC()))
				return
			}
		}
		writeError(c, http.StatusNotFound, "account_not_found", "account not found")
	})
}

func requireAllocationAPIKey(expected string) gin.HandlerFunc {
	return func(c *gin.Context) {
		const prefix = "Bearer "
		header := c.GetHeader("Authorization")
		scheme, token, ok := strings.Cut(header, " ")
		configured := validRuntimeAllocationAPIKey(expected)
		authenticated := false
		if ok && strings.EqualFold(scheme, strings.TrimSpace(prefix)) && token != "" && !strings.ContainsAny(token, " \t\r\n") && configured && len(token) == len(expected) {
			authenticated = subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1
		}
		if !authenticated {
			writeError(c, http.StatusUnauthorized, "unauthorized", "authentication required")
			c.Abort()
			return
		}
		c.Next()
	}
}

func validRuntimeAllocationAPIKey(value string) bool {
	if len(value) < 32 || strings.TrimSpace(value) != value || strings.ContainsAny(value, " \t\r\n") {
		return false
	}
	switch strings.ToLower(value) {
	case "change-me", "changeme", "default", "example", "sample", "password", "__replace_with_allocation_service_api_key__":
		return false
	}
	return true
}

func allocationTokenInput(request allocationImportRequest) (account.TokenInput, bool) {
	input := account.TokenInput{Label: request.Label}
	switch request.TokenType {
	case "access_token", "access":
		input.AccessToken = request.Token
	case "refresh_token", "refresh":
		input.RefreshToken = request.Token
	case "session_token", "session":
		input.SessionToken = request.Token
	default:
		return account.TokenInput{}, false
	}
	return input, true
}

func allocationListFromAccount(item account.Account) allocationListItem {
	return allocationListItem{
		ProviderAccountID: item.ProviderAccountID,
		Email:             item.Email,
		AuthExpiry:        item.AuthExpiry,
		Status:            item.Status,
		Plan:              item.Plan,
	}
}

func allocationStatusFromAccount(item account.Account, checkedAt time.Time) allocationStatusResponse {
	expiry := item.CurrentExpiry
	if expiry == nil {
		expiry = &item.AuthExpiry
	}
	return allocationStatusResponse{
		ProviderAccountID:  item.ProviderAccountID,
		Email:              item.Email,
		Status:             item.Status,
		Plan:               item.Plan,
		AuthExpiry:         item.AuthExpiry,
		SubscriptionExpiry: expiry,
		CheckedAt:          checkedAt,
	}
}

func allocationErrorCode(err error) string {
	var serviceErr *account.ServiceError
	if errors.As(err, &serviceErr) {
		return serviceErr.Code
	}
	return "internal_error"
}
