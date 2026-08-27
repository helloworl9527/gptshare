package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"allocation-service/accountsync"
	accountsvc "allocation-service/internal/account"
	allocatorsvc "allocation-service/internal/allocator"
	"allocation-service/internal/auth"
	cardsvc "allocation-service/internal/card"
	metricssvc "allocation-service/internal/metrics"
	"allocation-service/internal/models"
	"allocation-service/internal/repository"
	userquerysvc "allocation-service/internal/userquery"
	"allocation-service/monitorfacade"
	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
)

//go:embed static/user.html
var userPageHTML string

//go:embed static/user.css
var userPageCSS string

//go:embed static/user.js
var userPageJS string

//go:embed static/admin/*
var adminStatic embed.FS

//go:embed static/admin/index.html
var adminPageHTML string

const (
	sessionCookie = "__Host-allocation-session"
	csrfCookie    = "__Host-allocation-csrf"
	csrfHeader    = "X-CSRF-Token"
)

type HealthChecker interface {
	Health(context.Context) error
}

type Config struct {
	Origin             string
	Accounts           *accountsvc.Service
	Cards              *cardsvc.Service
	Allocator          *allocatorsvc.Service
	UserQuery          *userquerysvc.Service
	Metrics            *metricssvc.Service
	AccountEventSink   accountsync.Sink
	AccountEventAPIKey string
}

type AdminBoundary struct {
	RequireSession gin.HandlerFunc
	RequireCSRF    gin.HandlerFunc
	RequireOrigin  gin.HandlerFunc
}

func NewRouter(health HealthChecker, manager *auth.Manager, cfg Config, logger *slog.Logger) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	if logger == nil {
		logger = slog.Default()
	}
	router := gin.New()
	router.RedirectTrailingSlash = false
	router.Use(requestID(), securityHeaders(), accessLog(logger), recovery(logger))
	router.GET("/health", healthHandler(health, logger))
	if cfg.AccountEventSink != nil {
		router.POST("/api/internal/v1/monitor-account-events", monitorAccountEventHandler(cfg.AccountEventSink, cfg.AccountEventAPIKey))
	}
	RegisterPublicRoutes(router, cfg)
	registerStandaloneAdminRoutes(router, manager, cfg)
	return router
}

func monitorAccountEventHandler(sink accountsync.Sink, apiKey string) gin.HandlerFunc {
	return func(c *gin.Context) {
		authorization := c.GetHeader("Authorization")
		provided := strings.TrimPrefix(authorization, "Bearer ")
		if !strings.HasPrefix(authorization, "Bearer ") || len(apiKey) < 32 || len(provided) != len(apiKey) || subtle.ConstantTimeCompare([]byte(provided), []byte(apiKey)) != 1 {
			writeError(c, http.StatusUnauthorized, "unauthorized", "authentication required")
			return
		}
		body := http.MaxBytesReader(c.Writer, c.Request.Body, 32<<10)
		decoder := json.NewDecoder(body)
		decoder.DisallowUnknownFields()
		var event accountsync.Event
		if err := decoder.Decode(&event); err != nil {
			writeError(c, http.StatusBadRequest, "invalid_event", "event payload is invalid")
			return
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			writeError(c, http.StatusBadRequest, "invalid_event", "event payload is invalid")
			return
		}
		if err := event.Validate(); err != nil {
			writeError(c, http.StatusUnprocessableEntity, "invalid_event", "event payload is invalid")
			return
		}
		result, err := sink.ApplyMonitorAccountEvent(c.Request.Context(), event)
		if err != nil {
			writeError(c, http.StatusInternalServerError, "event_processing_failed", "event could not be processed")
			return
		}
		c.JSON(http.StatusOK, result)
	}
}

// RegisterPublicRoutes mounts only the public UI and card APIs on an existing
// engine. The modular monolith uses this to keep a single Gin engine while the
// allocation module retains ownership of its handlers and embedded assets.
func RegisterPublicRoutes(router *gin.Engine, cfg Config) {
	router.GET("/", userPageHandler())
	router.GET("/static/user.css", userAssetHandler("text/css; charset=utf-8", userPageCSS))
	router.GET("/static/user.js", userAssetHandler("application/javascript; charset=utf-8", userPageJS))
	router.GET("/admin", adminPageHandler())
	router.GET("/admin/*path", adminPageHandler())
	RegisterPublicAPIRoutes(router, cfg)
}

func RegisterPublicAPIRoutes(router *gin.Engine, cfg Config) {
	if cfg.Cards != nil {
		router.GET("/api/cards/:code/status", userCardStatusHandler(cfg.Cards))
	}
	if cfg.Allocator != nil {
		router.POST("/api/redeem", redeemCardHandler(cfg.Allocator))
	}
	if cfg.UserQuery != nil {
		router.POST("/api/cards/query", userQueryHandler(cfg.UserQuery))
	}
}

func registerStandaloneAdminRoutes(router *gin.Engine, manager *auth.Manager, cfg Config) {
	if manager == nil {
		return
	}
	router.GET("/api/auth/csrf", csrfHandler(manager))
	router.POST("/api/auth/password", requireOrigin(cfg.Origin), requireDoubleSubmitCSRF(), passwordHandler(manager))
	router.POST("/api/auth/totp", requireOrigin(cfg.Origin), requireDoubleSubmitCSRF(), totpHandler(manager))
	router.POST("/api/auth/logout", requireOrigin(cfg.Origin), requireSession(manager), requireSessionCSRF(manager), logoutHandler(manager))
	router.GET("/api/me", requireSession(manager), meHandler())
	registerProtectedAdminRoutes(router, cfg, AdminBoundary{
		RequireSession: requireSession(manager),
		RequireCSRF:    requireSessionCSRF(manager),
		RequireOrigin:  requireOrigin(cfg.Origin),
	})
}

func RegisterAdminRoutes(router *gin.Engine, cfg Config, boundary AdminBoundary) error {
	if router == nil || boundary.RequireSession == nil || boundary.RequireCSRF == nil || boundary.RequireOrigin == nil {
		return errors.New("complete unified admin boundary is required")
	}
	registerProtectedAdminRoutes(router, cfg, boundary)
	return nil
}

func registerProtectedAdminRoutes(router *gin.Engine, cfg Config, boundary AdminBoundary) {
	if cfg.Accounts != nil {
		router.GET("/api/admin/accounts/:id/credentials/reveal", noStore(), boundary.RequireSession, boundary.RequireCSRF, revealAccountCredentialsHandler(cfg.Accounts))
	}
	admin := router.Group("/api/admin", boundary.RequireSession)
	admin.GET("/ping", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "request_id": c.GetString("request_id")})
	})
	if cfg.Metrics != nil {
		admin.GET("/dashboard", dashboardMetricsHandler(cfg.Metrics))
	}
	if cfg.Accounts != nil {
		admin.GET("/accounts", listAccountsHandler(cfg.Accounts))
		admin.POST("/accounts", boundary.RequireOrigin, boundary.RequireCSRF, createAccountHandler(cfg.Accounts))
		admin.POST("/accounts/pull-monitor", boundary.RequireOrigin, boundary.RequireCSRF, pullMonitorAccountsHandler(cfg.Accounts))
		admin.GET("/account-settings", accountSettingsHandler(cfg.Accounts))
		admin.PUT("/account-settings", boundary.RequireOrigin, boundary.RequireCSRF, updateAccountSettingsHandler(cfg.Accounts))
		admin.POST("/accounts/apply-default-capacity", boundary.RequireOrigin, boundary.RequireCSRF, applyDefaultCapacityHandler(cfg.Accounts))
		admin.POST("/accounts/sync-status", boundary.RequireOrigin, boundary.RequireCSRF, syncAllAccountStatusHandler(cfg.Accounts))
		admin.GET("/accounts/:id", getAccountHandler(cfg.Accounts))
		admin.PUT("/accounts/:id", boundary.RequireOrigin, boundary.RequireCSRF, updateAccountHandler(cfg.Accounts))
		admin.DELETE("/accounts/:id", boundary.RequireOrigin, boundary.RequireCSRF, deleteAccountHandler(cfg.Accounts))
		admin.POST("/accounts/:id/sync-status", boundary.RequireOrigin, boundary.RequireCSRF, syncAccountStatusHandler(cfg.Accounts))
	}
	if cfg.Cards != nil {
		admin.GET("/cards", listCardsHandler(cfg.Cards))
		admin.POST("/cards/generate", boundary.RequireOrigin, boundary.RequireCSRF, generateCardsHandler(cfg.Cards))
		admin.POST("/cards/export", boundary.RequireOrigin, boundary.RequireCSRF, exportCardsHandler(cfg.Cards))
		admin.POST("/cards/expire-due", boundary.RequireOrigin, boundary.RequireCSRF, expireDueCardsHandler(cfg.Cards))
		admin.GET("/cards/:id/reveal", boundary.RequireCSRF, revealCardHandler(cfg.Cards))
		admin.POST("/cards/:id/revoke", boundary.RequireOrigin, boundary.RequireCSRF, revokeCardHandler(cfg.Cards))
		admin.POST("/cards/:id/extend", boundary.RequireOrigin, boundary.RequireCSRF, extendCardHandler(cfg.Cards))
	}
	if cfg.Allocator != nil {
		admin.GET("/allocations", listAllocationsHandler(cfg.Allocator))
	}
}

func healthHandler(health HealthChecker, logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := c.GetString("request_id")
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		if err := health.Health(ctx); err != nil {
			logger.Error("health check degraded", "request_id", requestID, "error_code", "db_unavailable")
			c.JSON(http.StatusServiceUnavailable, gin.H{"status": "degraded", "db": false, "request_id": requestID})
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok", "db": true, "request_id": requestID})
	}
}

func userPageHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(http.StatusOK, userPageHTML)
	}
}

func userAssetHandler(contentType, body string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Content-Type", contentType)
		c.String(http.StatusOK, body)
	}
}

func adminPageHandler() gin.HandlerFunc {
	adminFiles, err := fs.Sub(adminStatic, "static/admin")
	if err != nil {
		panic(err)
	}
	return func(c *gin.Context) {
		requestPath := strings.TrimPrefix(c.Request.URL.Path, "/admin")
		if requestPath == "" || requestPath == "/" {
			c.Header("Content-Type", "text/html; charset=utf-8")
			c.String(http.StatusOK, adminPageHTML)
			return
		}
		if strings.HasPrefix(requestPath, "/assets/") {
			c.FileFromFS(strings.TrimPrefix(requestPath, "/"), http.FS(adminFiles))
			return
		}
		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(http.StatusOK, adminPageHTML)
	}
}

func writeError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"code": code, "message": message, "request_id": c.GetString("request_id")})
}

func writeAuthError(c *gin.Context, err error) {
	var limited *auth.RateLimitError
	if errors.As(err, &limited) {
		seconds := int(limited.RetryAfter.Seconds())
		if seconds < 1 {
			seconds = 1
		}
		c.Header("Retry-After", strconv.Itoa(seconds))
		writeError(c, http.StatusTooManyRequests, "rate_limited", "too many authentication attempts")
		return
	}
	if errors.Is(err, auth.ErrInvalidCredentials) || errors.Is(err, auth.ErrInvalidChallenge) || errors.Is(err, auth.ErrTOTPReplay) {
		writeError(c, http.StatusUnauthorized, "invalid_credentials", "authentication failed")
		return
	}
	writeError(c, http.StatusInternalServerError, "internal_error", "request could not be processed")
}

func writeAccountError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, accountsvc.ErrValidation), errors.Is(err, repository.ErrAccountExpiryTooLong), errors.Is(err, repository.ErrCapacityTooSmall):
		writeError(c, http.StatusUnprocessableEntity, "validation_failed", "request validation failed")
	case errors.Is(err, accountsvc.ErrNotFound):
		writeError(c, http.StatusNotFound, "not_found", "account not found")
	case errors.Is(err, accountsvc.ErrCredentialsUnavailable):
		writeError(c, http.StatusConflict, "account_credentials_unavailable", "account credentials are unavailable")
	case errors.Is(err, repository.ErrAccountAllocated):
		writeError(c, http.StatusConflict, "account_allocated", "allocated account cannot be deleted")
	case errors.Is(err, repository.ErrAccountReplacementUnavailable):
		writeError(c, http.StatusConflict, "account_replacement_unavailable", "replacement account capacity is unavailable")
	case monitorFaultIs(err, monitorfacade.FaultContractChanged):
		writeError(c, http.StatusServiceUnavailable, "phase_one_contract_changed", "phase one monitor response contract changed")
	case monitorFaultIs(err, monitorfacade.FaultTimeout):
		writeError(c, http.StatusServiceUnavailable, "phase_one_monitor_timeout", "phase one monitor timed out")
	case errors.Is(err, monitorfacade.ErrUnavailable):
		writeError(c, http.StatusServiceUnavailable, "phase_one_monitor_unavailable", "phase one monitor is unavailable")
	default:
		writeError(c, http.StatusInternalServerError, "internal_error", "request could not be processed")
	}
}

func monitorFaultIs(err error, want monitorfacade.FaultKind) bool {
	kind, ok := monitorfacade.FaultKindOf(err)
	return ok && kind == want
}

func writeCardError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, cardsvc.ErrValidation):
		writeError(c, http.StatusUnprocessableEntity, "validation_failed", "request validation failed")
	case errors.Is(err, cardsvc.ErrDurationLimit):
		writeError(c, http.StatusUnprocessableEntity, "card_duration_limit_exceeded", "card validity cannot exceed 90 days from redemption")
	case errors.Is(err, cardsvc.ErrNotFound):
		writeError(c, http.StatusNotFound, "not_found", "card not found")
	case errors.Is(err, cardsvc.ErrConflict), errors.Is(err, repository.ErrCardStateConflict):
		writeError(c, http.StatusConflict, "card_state_conflict", "card state does not allow this operation")
	default:
		writeError(c, http.StatusInternalServerError, "internal_error", "request could not be processed")
	}
}

func writeAllocatorError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, allocatorsvc.ErrInvalidCode):
		writeError(c, http.StatusUnprocessableEntity, "validation_failed", "request validation failed")
	case errors.Is(err, allocatorsvc.ErrNotFound):
		writeError(c, http.StatusNotFound, "not_found", "card not found")
	case errors.Is(err, allocatorsvc.ErrUnavailable):
		writeError(c, http.StatusConflict, "card_not_redeemable", "card is not redeemable")
	case errors.Is(err, allocatorsvc.ErrNoCapacity):
		writeError(c, http.StatusConflict, "no_account_capacity", "no account capacity")
	default:
		writeError(c, http.StatusInternalServerError, "internal_error", "request could not be processed")
	}
}

type createAccountRequest struct {
	DisplayUsername    string `json:"display_username" binding:"max=256"`
	DisplayPassword    string `json:"display_password" binding:"required,max=2048"`
	DisplayTOTPSecret  string `json:"display_2fa_secret" binding:"required,max=2048"`
	PickupAddress      string `json:"pickup_address" binding:"max=2048"`
	SourceURL          string `json:"source_url" binding:"max=2048"`
	AccountExpiry      string `json:"account_expiry" binding:"max=64"`
	MaxConcurrentUsers int    `json:"max_concurrent_users" binding:"min=0,max=1000"`
	SyncMonitor        bool   `json:"sync_monitor"`
	MonitorToken       string `json:"monitor_token" binding:"required,max=4096"`
	MonitorTokenType   string `json:"monitor_token_type" binding:"max=64"`
}

type updateAccountRequest struct {
	DisplayUsername    string  `json:"display_username" binding:"required,max=256"`
	DisplayPassword    string  `json:"display_password" binding:"max=2048"`
	DisplayTOTPSecret  string  `json:"display_2fa_secret" binding:"max=2048"`
	PickupAddress      *string `json:"pickup_address" binding:"omitempty,max=2048"`
	SourceURL          *string `json:"source_url" binding:"omitempty,max=2048"`
	AccountExpiry      string  `json:"account_expiry" binding:"required"`
	MaxConcurrentUsers int     `json:"max_concurrent_users" binding:"required,min=1,max=1000"`
	Status             string  `json:"status" binding:"required,max=32"`
	MonitorStatus      string  `json:"monitor_status" binding:"required,max=32"`
	MonitorAccountID   string  `json:"monitor_account_id" binding:"max=256"`
}

type accountSettingsRequest struct {
	DefaultAccountCapacity int `json:"default_account_capacity" binding:"required,min=1,max=1000"`
}

type generateCardsRequest struct {
	Quantity     int `json:"quantity" binding:"required,min=1,max=1000"`
	DurationDays int `json:"duration_days" binding:"required,min=1,max=90"`
}

type exportCardsRequest struct {
	Quantity     int    `json:"quantity" binding:"required,min=1,max=1000"`
	DurationDays int    `json:"duration_days" binding:"required,min=1,max=90"`
	Format       string `json:"format" binding:"required,oneof=csv txt"`
}

type extendCardRequest struct {
	Days int `json:"days" binding:"required,min=1,max=90"`
}

type redeemCardRequest struct {
	Code string `json:"code" binding:"required,len=14"`
}

type userQueryRequest struct {
	Code          string `json:"code" binding:"required,max=32"`
	CaptchaID     int64  `json:"captcha_id"`
	CaptchaAnswer string `json:"captcha_answer" binding:"max=16"`
}

func listAccountsHandler(service *accountsvc.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := service.ListWithWarnings(c.Request.Context())
		if err != nil {
			writeAccountError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"accounts": serializeAccounts(result.Accounts), "warnings": result.Warnings, "request_id": c.GetString("request_id")})
	}
}

func getAccountHandler(service *accountsvc.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := accountIDParam(c)
		if !ok {
			return
		}
		account, err := service.Get(c.Request.Context(), id)
		if err != nil {
			writeAccountError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"account": serializeAccount(account), "request_id": c.GetString("request_id")})
	}
}

func revealAccountCredentialsHandler(service *accountsvc.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		id, ok := accountIDParam(c)
		if !ok {
			return
		}
		credentials, err := service.RevealCredentials(c.Request.Context(), id)
		if err != nil {
			writeAccountError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"account_id": credentials.AccountID,
			"password":   credentials.Password,
			"totp": gin.H{
				"secret":    credentials.TOTPSecret,
				"period":    30,
				"digits":    6,
				"algorithm": "SHA1",
			},
			"server_time": time.Now().UTC().Format(time.RFC3339Nano),
			"request_id":  c.GetString("request_id"),
		})
	}
}

func createAccountHandler(service *accountsvc.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request createAccountRequest
		if !bindJSON(c, &request, 16*1024) {
			return
		}
		pickupAddress := request.PickupAddress
		if strings.TrimSpace(pickupAddress) == "" {
			// Keep accepting the old field while callers migrate to pickup_address.
			pickupAddress = request.SourceURL
		}
		result, err := service.Create(c.Request.Context(), accountsvc.CreateInput{
			DisplayUsername:    request.DisplayUsername,
			DisplayPassword:    request.DisplayPassword,
			DisplayTOTPSecret:  request.DisplayTOTPSecret,
			SourceURL:          pickupAddress,
			MaxConcurrentUsers: request.MaxConcurrentUsers,
			SyncMonitor:        true,
			MonitorToken:       request.MonitorToken,
			MonitorTokenType:   request.MonitorTokenType,
		})
		if err != nil {
			writeAccountError(c, err)
			return
		}
		c.JSON(http.StatusCreated, gin.H{"account": serializeAccount(result.Account), "warnings": result.Warnings, "request_id": c.GetString("request_id")})
	}
}

func pullMonitorAccountsHandler(service *accountsvc.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request struct{}
		if !bindJSON(c, &request, 1024) {
			return
		}
		result, err := service.PullFromMonitor(c.Request.Context())
		if err != nil {
			writeAccountError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"accounts":   serializeAccounts(result.Accounts),
			"created":    result.Created,
			"updated":    result.Updated,
			"skipped":    result.Skipped,
			"failed":     result.Failed,
			"errors":     serializePullSyncErrors(result.Errors),
			"total":      result.Total,
			"request_id": c.GetString("request_id"),
		})
	}
}

func serializePullSyncErrors(items []accountsvc.PullSyncError) []gin.H {
	result := make([]gin.H, 0, len(items))
	for _, item := range items {
		result = append(result, gin.H{"monitor_account_id": item.MonitorAccountID, "code": item.Code})
	}
	return result
}

func updateAccountHandler(service *accountsvc.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := accountIDParam(c)
		if !ok {
			return
		}
		var request updateAccountRequest
		if !bindJSON(c, &request, 8192) {
			return
		}
		expiry, err := parseAPITime(request.AccountExpiry)
		if err != nil {
			writeAccountError(c, accountsvc.ErrValidation)
			return
		}
		status := request.Status
		if status == "pending_credentials" && strings.TrimSpace(request.DisplayPassword) != "" && strings.TrimSpace(request.DisplayTOTPSecret) != "" {
			status = "available"
		}
		pickupAddress := request.SourceURL
		if request.PickupAddress != nil {
			pickupAddress = request.PickupAddress
		}
		account, err := service.Update(c.Request.Context(), id, accountsvc.UpdateInput{
			DisplayUsername:    request.DisplayUsername,
			DisplayPassword:    request.DisplayPassword,
			DisplayTOTPSecret:  request.DisplayTOTPSecret,
			SourceURL:          pickupAddress,
			AccountExpiry:      expiry,
			MaxConcurrentUsers: request.MaxConcurrentUsers,
			Status:             status,
			MonitorStatus:      request.MonitorStatus,
			MonitorAccountID:   request.MonitorAccountID,
		})
		if err != nil {
			writeAccountError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"account": serializeAccount(account), "request_id": c.GetString("request_id")})
	}
}

func accountSettingsHandler(service *accountsvc.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		settings, err := service.CapacitySettings(c.Request.Context())
		if err != nil {
			writeAccountError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"settings": serializeAccountSettings(settings), "request_id": c.GetString("request_id")})
	}
}

func updateAccountSettingsHandler(service *accountsvc.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request accountSettingsRequest
		if !bindJSON(c, &request, 4096) {
			return
		}
		settings, err := service.SetDefaultCapacity(c.Request.Context(), request.DefaultAccountCapacity)
		if err != nil {
			writeAccountError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"settings": serializeAccountSettings(settings), "request_id": c.GetString("request_id")})
	}
}

func applyDefaultCapacityHandler(service *accountsvc.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := service.ApplyDefaultCapacity(c.Request.Context())
		if err != nil {
			writeAccountError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"default_account_capacity": result.DefaultAccountCapacity,
			"updated_accounts":         result.UpdatedAccounts,
			"request_id":               c.GetString("request_id"),
		})
	}
}

func deleteAccountHandler(service *accountsvc.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := accountIDParam(c)
		if !ok {
			return
		}
		result, err := service.Delete(c.Request.Context(), id)
		if err != nil {
			writeAccountError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"archived":             result.Archived,
			"replaced_allocations": result.ReplacedAllocations,
			"closed_allocations":   result.ClosedAllocations,
			"request_id":           c.GetString("request_id"),
		})
	}
}

func syncAccountStatusHandler(service *accountsvc.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := accountIDParam(c)
		if !ok {
			return
		}
		result, err := service.SyncStatus(c.Request.Context(), id)
		if err != nil {
			writeAccountError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"account": serializeAccount(result.Account), "warnings": result.Warnings, "request_id": c.GetString("request_id")})
	}
}

func syncAllAccountStatusHandler(service *accountsvc.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		result, err := service.SyncAll(c.Request.Context())
		if err != nil {
			writeAccountError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"accounts":   serializeAccounts(result.Accounts),
			"warnings":   result.Warnings,
			"total":      result.Total,
			"ok":         result.OK,
			"failed":     result.Failed,
			"request_id": c.GetString("request_id"),
		})
	}
}

func dashboardMetricsHandler(service *metricssvc.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		metrics, err := service.Dashboard(c.Request.Context())
		if err != nil {
			writeError(c, http.StatusInternalServerError, "internal_error", "request could not be processed")
			return
		}
		c.JSON(http.StatusOK, gin.H{"dashboard": serializeInventoryMetrics(metrics), "request_id": c.GetString("request_id")})
	}
}

func listCardsHandler(service *cardsvc.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		duration := 0
		if raw := c.Query("duration_days"); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil {
				writeCardError(c, cardsvc.ErrValidation)
				return
			}
			duration = parsed
		}
		cards, err := service.List(c.Request.Context(), repository.CardFilter{Status: c.Query("status"), DurationDays: duration})
		if err != nil {
			writeCardError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"cards": serializeCards(cards), "request_id": c.GetString("request_id")})
	}
}

func listAllocationsHandler(service *allocatorsvc.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		allocations, err := service.ListActive(c.Request.Context())
		if err != nil {
			writeError(c, http.StatusInternalServerError, "internal_error", "request could not be processed")
			return
		}
		history, err := service.ListReplacementHistory(c.Request.Context(), replacementHistoryLimit)
		if err != nil {
			writeError(c, http.StatusInternalServerError, "internal_error", "request could not be processed")
			return
		}
		out := make([]gin.H, 0, len(allocations))
		for _, allocation := range allocations {
			out = append(out, serializeAdminAllocation(allocation))
		}
		replacements := make([]gin.H, 0, len(history))
		for _, entry := range history {
			replacements = append(replacements, serializeReplacementHistory(entry))
		}
		c.JSON(http.StatusOK, gin.H{"allocations": out, "replacements": replacements, "request_id": c.GetString("request_id")})
	}
}

func generateCardsHandler(service *cardsvc.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request generateCardsRequest
		if !bindJSON(c, &request, 4096) {
			return
		}
		result, err := service.Generate(c.Request.Context(), request.Quantity, request.DurationDays)
		if err != nil {
			writeCardError(c, err)
			return
		}
		c.JSON(http.StatusCreated, gin.H{"cards": serializeGeneratedCards(result.Cards), "request_id": c.GetString("request_id")})
	}
}

func exportCardsHandler(service *cardsvc.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request exportCardsRequest
		if !bindJSON(c, &request, 4096) {
			return
		}
		contentType, filename, body, err := service.Export(c.Request.Context(), request.Quantity, request.DurationDays, cardsvc.ExportFormat(request.Format))
		if err != nil {
			writeCardError(c, err)
			return
		}
		c.Header("Content-Type", contentType)
		c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
		c.String(http.StatusOK, body)
	}
}

func revealCardHandler(service *cardsvc.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := idParam(c, "id", "card not found")
		if !ok {
			return
		}
		result, err := service.Reveal(c.Request.Context(), id)
		switch {
		case err == nil:
			c.JSON(http.StatusOK, gin.H{
				"card": gin.H{
					"id":                  result.Card.ID,
					"code_suffix":         result.Card.CodeSuffix,
					"plaintext_available": true,
				},
				"code":       result.Code,
				"request_id": c.GetString("request_id"),
			})
		case errors.Is(err, cardsvc.ErrCodeUnavailable):
			c.JSON(http.StatusOK, gin.H{
				"card": gin.H{
					"id":                  result.Card.ID,
					"code_suffix":         result.Card.CodeSuffix,
					"plaintext_available": false,
				},
				"message":    "明文不可用(旧批次)",
				"request_id": c.GetString("request_id"),
			})
		default:
			writeCardError(c, err)
		}
	}
}

func revokeCardHandler(service *cardsvc.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := idParam(c, "id", "card not found")
		if !ok {
			return
		}
		card, err := service.Revoke(c.Request.Context(), id)
		if err != nil {
			writeCardError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"card": serializeCard(card), "request_id": c.GetString("request_id")})
	}
}

func extendCardHandler(service *cardsvc.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		id, ok := idParam(c, "id", "card not found")
		if !ok {
			return
		}
		var request extendCardRequest
		if !bindJSON(c, &request, 4096) {
			return
		}
		card, err := service.Extend(c.Request.Context(), id, request.Days)
		if err != nil {
			writeCardError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"card": serializeCard(card), "request_id": c.GetString("request_id")})
	}
}

func expireDueCardsHandler(service *cardsvc.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		count, err := service.ExpireDue(c.Request.Context())
		if err != nil {
			writeCardError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"expired": count, "request_id": c.GetString("request_id")})
	}
}

func userCardStatusHandler(service *cardsvc.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		card, err := service.UserView(c.Request.Context(), c.Param("code"))
		if err != nil {
			writeCardError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"card": serializeUserCard(card), "request_id": c.GetString("request_id")})
	}
}

func redeemCardHandler(service *allocatorsvc.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request redeemCardRequest
		if !bindJSON(c, &request, 4096) {
			return
		}
		result, err := service.Redeem(c.Request.Context(), strings.ToUpper(request.Code))
		if err != nil {
			writeAllocatorError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{
			"allocation": serializeAllocation(result.Allocation),
			"card":       serializeUserCard(result.Card),
			"account": gin.H{
				"id":               result.Account.ID,
				"display_username": result.Account.DisplayUsername,
				"monitor_status":   result.Account.MonitorStatus,
			},
			"warnings":    result.Warnings,
			"duration_ms": result.Elapsed.Milliseconds(),
			"request_id":  c.GetString("request_id"),
		})
	}
}

func userQueryHandler(service *userquerysvc.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request userQueryRequest
		if !bindJSON(c, &request, 4096) {
			return
		}
		result, err := service.Query(c.Request.Context(), userquerysvc.QueryInput{
			Code:          request.Code,
			ClientIP:      clientIP(c.Request),
			CaptchaID:     request.CaptchaID,
			CaptchaAnswer: request.CaptchaAnswer,
		})
		switch {
		case err == nil:
			c.JSON(http.StatusOK, gin.H{"result": serializeUserQuery(result), "request_id": c.GetString("request_id")})
		case errors.Is(err, userquerysvc.ErrCaptchaRequired):
			writeCaptchaRequired(c, result)
		case errors.Is(err, userquerysvc.ErrCaptchaInvalid):
			writeError(c, http.StatusForbidden, "captcha_invalid", "captcha validation failed")
		case errors.Is(err, userquerysvc.ErrInvalidQuery):
			writeError(c, http.StatusNotFound, "query_not_available", "card query is not available")
		default:
			writeError(c, http.StatusInternalServerError, "internal_error", "request could not be processed")
		}
	}
}

func writeCaptchaRequired(c *gin.Context, result userquerysvc.QueryResult) {
	if result.Captcha == nil {
		writeError(c, http.StatusForbidden, "captcha_required", "captcha is required")
		return
	}
	c.JSON(http.StatusForbidden, gin.H{
		"code":       "captcha_required",
		"message":    "captcha is required",
		"request_id": c.GetString("request_id"),
		"captcha": gin.H{
			"id":         result.Captcha.ID,
			"question":   result.Captcha.Question,
			"expires_at": result.Captcha.ExpiresAt.UTC().Format(time.RFC3339Nano),
		},
	})
}

type passwordRequest struct {
	Username string `json:"username" binding:"required,max=256"`
	Password string `json:"password" binding:"required,max=1024"`
}

func passwordHandler(manager *auth.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request passwordRequest
		if !bindJSON(c, &request, 4096) {
			return
		}
		challenge, err := manager.Password(c.Request.Context(), request.Username, request.Password, clientIP(c.Request), c.GetString("csrf_token"))
		if err != nil {
			writeAuthError(c, err)
			return
		}
		c.JSON(http.StatusOK, gin.H{"challenge": challenge, "expires_in": 120})
	}
}

type totpRequest struct {
	Challenge string `json:"challenge" binding:"required,max=128"`
	Code      string `json:"code" binding:"required,len=6,numeric"`
}

func totpHandler(manager *auth.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request totpRequest
		if !bindJSON(c, &request, 4096) {
			return
		}
		session, err := manager.TOTP(c.Request.Context(), request.Challenge, request.Code, clientIP(c.Request), c.GetString("csrf_token"))
		if err != nil {
			writeAuthError(c, err)
			return
		}
		csrfToken, err := randomToken()
		if err != nil || manager.BindCSRF(c.Request.Context(), session.JTI, csrfToken) != nil {
			_ = manager.Logout(c.Request.Context(), session.JTI)
			writeError(c, http.StatusInternalServerError, "internal_error", "request could not be processed")
			return
		}
		http.SetCookie(c.Writer, &http.Cookie{Name: sessionCookie, Value: session.Token, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, Expires: session.ExpiresAt, MaxAge: int(time.Until(session.ExpiresAt).Seconds())})
		setCSRFCookie(c.Writer, csrfToken, int(time.Until(session.ExpiresAt).Seconds()))
		c.Status(http.StatusNoContent)
	}
}

func csrfHandler(manager *auth.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, err := randomToken()
		if err != nil {
			writeError(c, http.StatusInternalServerError, "internal_error", "request could not be processed")
			return
		}
		setCSRFCookie(c.Writer, token, 600)
		if sessionText, err := c.Cookie(sessionCookie); err == nil {
			if principal, err := manager.Authenticate(c.Request.Context(), sessionText); err == nil {
				if err := manager.BindCSRF(c.Request.Context(), principal.JTI, token); err != nil {
					writeError(c, http.StatusInternalServerError, "internal_error", "request could not be processed")
					return
				}
			}
		}
		c.JSON(http.StatusOK, gin.H{"csrf_token": token})
	}
}

func logoutHandler(manager *auth.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		principal := c.MustGet("principal").(*auth.Principal)
		if err := manager.Logout(c.Request.Context(), principal.JTI); err != nil {
			writeError(c, http.StatusUnauthorized, "unauthorized", "authentication required")
			return
		}
		expired := time.Unix(1, 0)
		http.SetCookie(c.Writer, &http.Cookie{Name: sessionCookie, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, Expires: expired, MaxAge: -1})
		http.SetCookie(c.Writer, &http.Cookie{Name: csrfCookie, Path: "/", Secure: true, HttpOnly: false, SameSite: http.SameSiteStrictMode, Expires: expired, MaxAge: -1})
		c.Status(http.StatusNoContent)
	}
}

func meHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		principal := c.MustGet("principal").(*auth.Principal)
		c.JSON(http.StatusOK, gin.H{"username": principal.Username})
	}
}

func requireSession(manager *auth.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		token, err := c.Cookie(sessionCookie)
		if err != nil {
			writeError(c, http.StatusUnauthorized, "unauthorized", "authentication required")
			c.Abort()
			return
		}
		principal, err := manager.Authenticate(c.Request.Context(), token)
		if err != nil {
			writeError(c, http.StatusUnauthorized, "unauthorized", "authentication required")
			c.Abort()
			return
		}
		c.Set("principal", principal)
		c.Next()
	}
}

func requireOrigin(expected string) gin.HandlerFunc {
	return func(c *gin.Context) {
		actual := c.GetHeader("Origin")
		if actual == "" {
			if referer := c.GetHeader("Referer"); referer != "" {
				if parsed, err := url.Parse(referer); err == nil {
					actual = parsed.Scheme + "://" + parsed.Host
				}
			}
		}
		if expected == "" || len(actual) != len(expected) || subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) != 1 {
			writeError(c, http.StatusForbidden, "csrf_rejected", "request origin rejected")
			c.Abort()
			return
		}
		c.Next()
	}
}

func requireDoubleSubmitCSRF() gin.HandlerFunc {
	return func(c *gin.Context) {
		cookie, err := c.Cookie(csrfCookie)
		header := c.GetHeader(csrfHeader)
		if err != nil || len(cookie) < 32 || len(cookie) != len(header) || subtle.ConstantTimeCompare([]byte(cookie), []byte(header)) != 1 {
			writeError(c, http.StatusForbidden, "csrf_rejected", "CSRF validation failed")
			c.Abort()
			return
		}
		c.Set("csrf_token", header)
		c.Next()
	}
}

func requireSessionCSRF(manager *auth.Manager) gin.HandlerFunc {
	return func(c *gin.Context) {
		cookie, err := c.Cookie(csrfCookie)
		header := c.GetHeader(csrfHeader)
		principal, ok := c.Get("principal")
		if err != nil || !ok || len(cookie) != len(header) || subtle.ConstantTimeCompare([]byte(cookie), []byte(header)) != 1 || !manager.VerifyCSRF(c.Request.Context(), principal.(*auth.Principal).JTI, header) {
			writeError(c, http.StatusForbidden, "csrf_rejected", "CSRF validation failed")
			c.Abort()
			return
		}
		c.Next()
	}
}

func bindJSON(c *gin.Context, target any, limit int64) bool {
	mediaType, _, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(c, http.StatusUnsupportedMediaType, "content_type_required", "Content-Type must be application/json")
		return false
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(c, http.StatusUnprocessableEntity, "validation_failed", "request validation failed")
		return false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		writeError(c, http.StatusUnprocessableEntity, "validation_failed", "request validation failed")
		return false
	}
	if err := binding.Validator.ValidateStruct(target); err != nil {
		writeError(c, http.StatusUnprocessableEntity, "validation_failed", "request validation failed")
		return false
	}
	return true
}

func randomToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func setCSRFCookie(writer http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(writer, &http.Cookie{Name: csrfCookie, Value: token, Path: "/", Secure: true, HttpOnly: false, SameSite: http.SameSiteStrictMode, MaxAge: maxAge})
}

func clientIP(request *http.Request) string {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		host = request.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer == nil {
		return "invalid"
	}
	return peer.String()
}

func accountIDParam(c *gin.Context) (int64, bool) {
	return idParam(c, "id", "account not found")
}

func idParam(c *gin.Context, name, message string) (int64, bool) {
	id, err := strconv.ParseInt(c.Param(name), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusNotFound, "not_found", message)
		return 0, false
	}
	return id, true
}

func parseAPITime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err == nil {
		return parsed.UTC(), nil
	}
	parsed, err = time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, err
	}
	return parsed.UTC(), nil
}

func serializeAccounts(accounts []models.Account) []gin.H {
	out := make([]gin.H, 0, len(accounts))
	for _, account := range accounts {
		out = append(out, serializeAccount(account))
	}
	return out
}

func serializeAccount(account models.Account) gin.H {
	body := gin.H{
		"id":                   account.ID,
		"display_username":     account.DisplayUsername,
		"account_expiry":       account.AccountExpiry.UTC().Format(time.RFC3339Nano),
		"max_concurrent_users": account.MaxConcurrentUsers,
		"current_allocations":  account.CurrentAllocations,
		"monitor_status":       account.MonitorStatus,
		"status":               account.Status,
	}
	if account.SourceURL != "" {
		body["pickup_address"] = account.SourceURL
		body["source_url"] = account.SourceURL
	}
	if account.MonitorAccountID != "" {
		body["monitor_account_id"] = account.MonitorAccountID
	}
	if account.LastAllocatedAt != nil {
		body["last_allocated_at"] = account.LastAllocatedAt.UTC().Format(time.RFC3339Nano)
	}
	return body
}

func serializeAccountSettings(settings repository.AccountCapacitySettings) gin.H {
	return gin.H{"default_account_capacity": settings.DefaultAccountCapacity}
}

func serializeInventoryMetrics(metrics repository.InventoryMetrics) gin.H {
	body := gin.H{
		"capacity":                 metrics.Capacity,
		"used":                     metrics.Used,
		"available_capacity":       metrics.AvailableCapacity,
		"eligible_accounts":        metrics.EligibleAccounts,
		"allocatable_accounts":     metrics.AllocatableAccounts,
		"blocked_capacity":         metrics.BlockedCapacity,
		"blocked_breakdown":        serializeBlockedCapacity(metrics.BlockedBreakdown),
		"unused_cards":             metrics.UnusedCards,
		"redeemed_last_7_days":     metrics.RedeemedLast7Days,
		"daily_redemption_rate":    metrics.DailyRedemptionRate,
		"recommended_account_add":  metrics.RecommendedAccountAdd,
		"average_account_capacity": metrics.AverageAccountCapacity,
		"warning_level":            metrics.WarningLevel,
		"warning_label":            metrics.WarningLabel,
		"days_to_exhaust_label":    metrics.ExhaustionWindow,
		"thresholds":               gin.H{"safe_gt_days": 15, "notice_min_days": 7, "notice_max_days": 15, "urgent_lt_days": 7, "exhausted_capacity": 0},
	}
	if metrics.DaysToExhaust != nil {
		body["days_to_exhaust"] = *metrics.DaysToExhaust
	} else {
		body["days_to_exhaust"] = nil
	}
	return body
}

func serializeBlockedCapacity(buckets []repository.BlockedCapacityBucket) []gin.H {
	out := make([]gin.H, 0, len(buckets))
	for _, bucket := range buckets {
		out = append(out, gin.H{"reason": bucket.Status, "accounts": bucket.Accounts, "free_slots": bucket.FreeSlots})
	}
	return out
}

func serializeGeneratedCards(cards []cardsvc.GeneratedCard) []gin.H {
	out := make([]gin.H, 0, len(cards))
	for _, card := range cards {
		out = append(out, gin.H{
			"id":            card.ID,
			"code":          card.Code,
			"code_suffix":   card.CodeSuffix,
			"duration_days": card.DurationDays,
			"status":        card.Status,
		})
	}
	return out
}

func serializeCards(cards []models.Card) []gin.H {
	out := make([]gin.H, 0, len(cards))
	for _, card := range cards {
		out = append(out, serializeCard(card))
	}
	return out
}

func serializeCard(card models.Card) gin.H {
	body := gin.H{
		"id":                  card.ID,
		"code_suffix":         card.CodeSuffix,
		"duration_days":       card.DurationDays,
		"status":              card.Status,
		"plaintext_available": card.PlaintextAvailable,
		"created_at":          card.CreatedAt.UTC().Format(time.RFC3339Nano),
		"updated_at":          card.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	if card.RedeemedAt != nil {
		body["redeemed_at"] = card.RedeemedAt.UTC().Format(time.RFC3339Nano)
	}
	if card.ExpiresAt != nil {
		body["expires_at"] = card.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	if card.RevokedAt != nil {
		body["revoked_at"] = card.RevokedAt.UTC().Format(time.RFC3339Nano)
	}
	return body
}

func serializeUserCard(card models.Card) gin.H {
	body := gin.H{
		"code_suffix": card.CodeSuffix,
		"status":      card.Status,
	}
	if card.ExpiresAt != nil {
		body["valid_until"] = card.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	return body
}

func serializeAllocation(allocation models.Allocation) gin.H {
	body := gin.H{
		"id":               allocation.ID,
		"card_id":          allocation.CardID,
		"account_id":       allocation.AccountID,
		"allocated_at":     allocation.AllocatedAt.UTC().Format(time.RFC3339Nano),
		"valid_until":      allocation.ValidUntil.UTC().Format(time.RFC3339Nano),
		"allocation_state": allocation.AllocationState,
		"active":           allocation.Active,
	}
	if allocation.GraceUntil != nil {
		body["grace_until"] = allocation.GraceUntil.UTC().Format(time.RFC3339Nano)
	}
	return body
}

// replacementHistoryLimit 限制后台替换历史一次返回的条数，避免长期运行后响应无限膨胀。
const replacementHistoryLimit = 200

func serializeReplacementHistory(entry repository.ReplacementHistoryEntry) gin.H {
	body := gin.H{
		"id":               entry.ID,
		"card_id":          entry.CardID,
		"code_suffix":      entry.CodeSuffix,
		"old_account_id":   entry.OldAccountID,
		"old_account_name": entry.OldAccountName,
		"old_account_gone": entry.OldAccountGone,
		"new_account_id":   entry.NewAccountID,
		"new_account_name": entry.NewAccountName,
		"new_account_gone": entry.NewAccountGone,
		"reason":           entry.Reason,
		"operator":         entry.Operator,
		"detected_at":      entry.DetectedAt.UTC().Format(time.RFC3339Nano),
		"replaced_at":      entry.ReplacedAt.UTC().Format(time.RFC3339Nano),
	}
	if entry.GraceUntil != nil {
		body["grace_until"] = entry.GraceUntil.UTC().Format(time.RFC3339Nano)
	}
	return body
}

func serializeAdminAllocation(view allocatorsvc.AdminAllocation) gin.H {
	body := gin.H{
		"id":               view.Allocation.ID,
		"allocation_state": view.Allocation.AllocationState,
		"active":           view.Allocation.Active,
		"allocated_at":     view.Allocation.AllocatedAt.UTC().Format(time.RFC3339Nano),
		"valid_until":      view.Allocation.ValidUntil.UTC().Format(time.RFC3339Nano),
		"card_id":          view.Card.ID,
		"code_suffix":      view.Card.CodeSuffix,
		"duration_days":    view.Card.DurationDays,
		"account_id":       view.Account.ID,
		"display_username": view.Account.DisplayUsername,
		"account_expiry":   view.Account.AccountExpiry.UTC().Format(time.RFC3339Nano),
	}
	if view.Allocation.GraceUntil != nil {
		body["grace_until"] = view.Allocation.GraceUntil.UTC().Format(time.RFC3339Nano)
	}
	return body
}

func serializeUserQuery(result userquerysvc.QueryResult) gin.H {
	view := result.View
	views := result.Views
	if len(views) == 0 {
		views = []repository.UserAllocationView{view}
	}
	body := gin.H{
		"account": gin.H{
			"display_username": view.Account.DisplayUsername,
			"password":         view.Credentials.Password,
		},
		"totp": gin.H{
			"secret":    view.Credentials.TOTPSecret,
			"period":    30,
			"digits":    6,
			"algorithm": "SHA1",
		},
		"card": gin.H{
			"code_suffix":   view.Card.CodeSuffix,
			"duration_days": view.Card.DurationDays,
			"valid_until":   view.Allocation.ValidUntil.UTC().Format(time.RFC3339Nano),
			"status":        view.Card.Status,
		},
		"allocation": serializeAllocation(view.Allocation),
		"accounts":   serializeUserQueryAccounts(views),
		"replacement_notice": gin.H{
			"state": view.Allocation.AllocationState,
		},
		"duration_ms": result.Elapsed.Milliseconds(),
	}
	if view.Account.SourceURL != "" {
		body["account"].(gin.H)["pickup_address"] = view.Account.SourceURL
	}
	if view.Allocation.GraceUntil != nil {
		body["replacement_notice"] = gin.H{
			"state":       view.Allocation.AllocationState,
			"grace_until": view.Allocation.GraceUntil.UTC().Format(time.RFC3339Nano),
		}
	}
	return body
}

func serializeUserQueryAccounts(views []repository.UserAllocationView) []gin.H {
	out := make([]gin.H, 0, len(views))
	for _, view := range views {
		item := gin.H{
			"allocation": serializeAllocation(view.Allocation),
			"account": gin.H{
				"display_username": view.Account.DisplayUsername,
				"password":         view.Credentials.Password,
			},
			"totp": gin.H{
				"secret":    view.Credentials.TOTPSecret,
				"period":    30,
				"digits":    6,
				"algorithm": "SHA1",
			},
		}
		if view.Account.SourceURL != "" {
			item["account"].(gin.H)["pickup_address"] = view.Account.SourceURL
		}
		out = append(out, item)
	}
	return out
}

func securityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Referrer-Policy", "no-referrer")
		c.Header("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; base-uri 'self'")
		c.Header("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		c.Header("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		c.Next()
	}
}

func noStore() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		c.Next()
	}
}

func recovery(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if recovered := recover(); recovered != nil {
				requestID := c.GetString("request_id")
				logger.Error("http handler panic", "request_id", requestID, "error_code", "internal_error")
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"code": "internal_error", "message": "request could not be processed", "request_id": requestID})
			}
		}()
		c.Next()
	}
}

func requestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		var value [16]byte
		if _, err := rand.Read(value[:]); err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"code": "internal_error", "message": "request could not be processed", "request_id": "unavailable"})
			return
		}
		id := hex.EncodeToString(value[:])
		c.Set("request_id", id)
		c.Header("X-Request-ID", id)
		c.Next()
	}
}

func accessLog(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		started := time.Now()
		c.Next()
		logger.Info("http request", "request_id", c.GetString("request_id"), "method", c.Request.Method, "path", c.FullPath(), "status", c.Writer.Status(), "duration_ms", time.Since(started).Milliseconds())
	}
}
