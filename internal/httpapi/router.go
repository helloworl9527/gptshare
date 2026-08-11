package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"chatgpt-monitor/internal/auth"
	"chatgpt-monitor/internal/requestmeta"
	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
)

const (
	sessionCookie = "__Host-session"
	csrfCookie    = "__Host-csrf"
	csrfHeader    = "X-CSRF-Token"
)

type HealthChecker interface {
	Health(context.Context) error
}

type Config struct {
	Origin                  string
	TrustLoopbackProxy      bool
	Monitor                 MonitorService
	Settings                SettingsService
	AllocationServiceAPIKey string
}

type AdminBoundary struct {
	RequireSession gin.HandlerFunc
	RequireCSRF    gin.HandlerFunc
	RequireOrigin  gin.HandlerFunc
}

func NewRouter(health HealthChecker, manager *auth.Manager, accounts AccountService, cfg Config, logger *slog.Logger) *gin.Engine {
	gin.SetMode(gin.ReleaseMode)
	if logger == nil {
		logger = slog.Default()
	}
	router := gin.New()
	router.Use(requestID(), accessLog(logger), recovery(logger))
	router.GET("/healthz", healthHandler(health, logger))
	if accounts != nil {
		registerAllocationRoutes(router, accounts, cfg, logger)
	}
	if manager == nil {
		return router
	}

	RegisterUnifiedAdminRoutes(router, manager, accounts, cfg)
	return router
}

func RegisterUnifiedAdminRoutes(router *gin.Engine, manager *auth.Manager, accounts AccountService, cfg Config) {
	if router == nil || manager == nil {
		return
	}
	router.GET("/api/auth/csrf", csrfHandler(manager))
	router.POST("/api/auth/password", requireOrigin(cfg.Origin), requireDoubleSubmitCSRF(), passwordHandler(manager, cfg))
	router.POST("/api/auth/totp", requireOrigin(cfg.Origin), requireDoubleSubmitCSRF(), totpHandler(manager, cfg))
	router.POST("/api/auth/logout", requireSession(manager), requireOrigin(cfg.Origin), requireSessionCSRF(manager), logoutHandler(manager))
	router.GET("/api/me", requireSession(manager), meHandler())
	router.GET("/api/admin/config/security-boundaries", requireSession(manager), securityBoundariesHandler())
	if accounts != nil {
		registerAccountRoutes(router, manager, accounts, cfg)
	}
	if cfg.Monitor != nil {
		registerMonitorRoutes(router, manager, cfg.Monitor, cfg)
	}
	if cfg.Settings != nil {
		registerSettingsRoutes(router, manager, cfg.Settings, cfg)
	}
}

func UnifiedAdminBoundary(manager *auth.Manager, origin string) AdminBoundary {
	if manager == nil {
		return AdminBoundary{}
	}
	return AdminBoundary{
		RequireSession: requireSession(manager),
		RequireCSRF:    requireSessionCSRF(manager),
		RequireOrigin:  requireOrigin(origin),
	}
}

func securityBoundariesHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"groups": []gin.H{
				{"id": "unified_admin_auth", "purpose": "administrator authentication only", "configuration": []string{"ADMIN_PASSWORD_HASH", "ADMIN_TOTP_SECRET", "JWT_SIGNING_KEY", "RATE_LIMIT_KEY"}},
				{"id": "monitor_data_encryption", "purpose": "monitor credentials and temporary authorization session encryption only", "configuration": []string{"CREDENTIAL_MASTER_KEYS", "CREDENTIAL_ACTIVE_KEY_ID"}},
				{"id": "allocation_data_encryption", "purpose": "allocation credentials and card reveal encryption only", "configuration": []string{"ALLOCATION_CREDENTIAL_MASTER_KEYS", "ALLOCATION_CREDENTIAL_ACTIVE_KEY_ID"}},
			},
			"key_material_exposed": false,
			"request_id":           c.GetString("request_id"),
		})
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

type passwordRequest struct {
	Username string `json:"username" binding:"required,max=256"`
	Password string `json:"password" binding:"required,max=1024"`
}

func passwordHandler(manager *auth.Manager, cfg Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request passwordRequest
		if !bindJSON(c, &request, 4096) {
			return
		}
		challenge, err := manager.Password(c.Request.Context(), request.Username, request.Password, clientIP(c.Request, cfg.TrustLoopbackProxy), c.GetString("csrf_token"))
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

func totpHandler(manager *auth.Manager, cfg Config) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request totpRequest
		if !bindJSON(c, &request, 4096) {
			return
		}
		session, err := manager.TOTP(c.Request.Context(), request.Challenge, request.Code, clientIP(c.Request, cfg.TrustLoopbackProxy), c.GetString("csrf_token"))
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
		if len(actual) != len(expected) || subtle.ConstantTimeCompare([]byte(actual), []byte(expected)) != 1 {
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

func clientIP(request *http.Request, trustLoopbackProxy bool) string {
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		host = request.RemoteAddr
	}
	peer := net.ParseIP(host)
	if trustLoopbackProxy && peer != nil && peer.IsLoopback() {
		for _, header := range []string{"X-Real-IP", "X-Forwarded-For"} {
			candidate := strings.TrimSpace(request.Header.Get(header))
			if !strings.Contains(candidate, ",") && net.ParseIP(candidate) != nil {
				return candidate
			}
		}
	}
	if peer == nil {
		return "invalid"
	}
	return peer.String()
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

func writeError(c *gin.Context, status int, code, message string) {
	c.JSON(status, gin.H{"code": code, "message": message, "request_id": c.GetString("request_id")})
}

func setCSRFCookie(writer http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(writer, &http.Cookie{Name: csrfCookie, Value: token, Path: "/", Secure: true, HttpOnly: false, SameSite: http.SameSiteStrictMode, MaxAge: maxAge})
}

func randomToken() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
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
		c.Request = c.Request.WithContext(requestmeta.WithRequestID(c.Request.Context(), id))
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
