package vitalsapp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"
	"reflect"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

type HealthChecker interface {
	Health(context.Context) error
}

type Background interface {
	BackgroundStatus() string
}

type RouteRegistrar interface {
	RegisterPublicRoutes(*gin.Engine)
}

type App struct {
	engine *gin.Engine
}

func New(monitor, allocation HealthChecker, background Background, allocationRoutes RouteRegistrar, logger *slog.Logger) *App {
	gin.SetMode(gin.ReleaseMode)
	if logger == nil {
		logger = slog.Default()
	}
	engine := gin.New()
	engine.RedirectTrailingSlash = false
	engine.Use(requestID(), securityHeaders(), recovery(logger), accessLog(logger))
	engine.GET("/health", healthHandler(monitor, allocation, background, logger))
	if allocationRoutes != nil {
		allocationRoutes.RegisterPublicRoutes(engine)
	}
	engine.GET("/api/monitor/ping", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "request_id": c.GetString("request_id")})
	})
	return &App{engine: engine}
}

func (a *App) Handler() http.Handler { return a.engine }

func (a *App) Engine() *gin.Engine { return a.engine }

func healthHandler(monitor, allocation HealthChecker, background Background, logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		monitorStatus := databaseStatus(ctx, monitor)
		allocationStatus := databaseStatus(ctx, allocation)
		backgroundStatus := "ok"
		if background != nil && background.BackgroundStatus() == "degraded" {
			backgroundStatus = "degraded"
		}
		status := "ok"
		httpStatus := http.StatusOK
		if monitorStatus != "ok" || allocationStatus != "ok" {
			status = "degraded"
			httpStatus = http.StatusServiceUnavailable
			logger.Error("vitals health check failed", "request_id", c.GetString("request_id"), "error_code", "database_unavailable")
		}
		c.JSON(httpStatus, gin.H{
			"status":     status,
			"process":    "ok",
			"databases":  gin.H{"monitor": monitorStatus, "allocation": allocationStatus},
			"background": backgroundStatus,
			"request_id": c.GetString("request_id"),
		})
	}
}

func databaseStatus(ctx context.Context, checker HealthChecker) string {
	if checker == nil || checker.Health(ctx) != nil {
		return "unavailable"
	}
	return "ok"
}

func requestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		var value [16]byte
		if _, err := rand.Read(value[:]); err != nil {
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"code": "request_id_unavailable", "message": "request could not be processed"})
			return
		}
		id := hex.EncodeToString(value[:])
		c.Set("request_id", id)
		c.Header("X-Request-ID", id)
		c.Next()
	}
}

func securityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Referrer-Policy", "no-referrer")
		c.Header("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		c.Header("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; font-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'self'; form-action 'self'")
		c.Next()
	}
}

func recovery(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if recovered := recover(); recovered != nil {
				logger.Error("http handler panic recovered", "request_id", c.GetString("request_id"), "error_code", "http_handler_panic", "panic_type", panicType(recovered))
				c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{"code": "internal_error", "message": "request could not be processed", "request_id": c.GetString("request_id")})
			}
		}()
		c.Next()
	}
}

func panicType(value any) string {
	kind := reflect.TypeOf(value)
	if kind == nil {
		return "unknown"
	}
	return kind.String()
}

func accessLog(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		started := time.Now()
		c.Next()
		logger.Info("http request completed", "request_id", c.GetString("request_id"), "method", c.Request.Method, "path", c.FullPath(), "status", c.Writer.Status(), "duration_ms", time.Since(started).Milliseconds())
	}
}

type CompatLimiter struct {
	mu       sync.Mutex
	window   time.Time
	requests int
	limit    int
}

func NewCompatLimiter(limit int) *CompatLimiter {
	if limit <= 0 {
		limit = 60
	}
	return &CompatLimiter{limit: limit}
}

func (l *CompatLimiter) Middleware(logger *slog.Logger, consumer string, expiresAt time.Time) gin.HandlerFunc {
	return func(c *gin.Context) {
		now := time.Now().UTC()
		if !now.Before(expiresAt) {
			logger.Warn("monitor compatibility API exception expired", "request_id", c.GetString("request_id"), "consumer", consumer, "error_code", "compat_http_expired")
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		l.mu.Lock()
		if l.window.IsZero() || now.Sub(l.window) >= time.Minute {
			l.window = now
			l.requests = 0
		}
		l.requests++
		allowed := l.requests <= l.limit
		l.mu.Unlock()
		if !allowed {
			c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{"code": "rate_limited", "message": "too many requests", "request_id": c.GetString("request_id")})
			return
		}
		logger.Info("monitor compatibility API accessed", "request_id", c.GetString("request_id"), "consumer", consumer, "expires_at", expiresAt.Format(time.RFC3339), "error_code", "compat_http_access")
		c.Next()
	}
}
