package httpapi

import (
	"errors"
	"net"
	"net/http"
	"os"

	cardsvc "allocation-service/internal/card"
	"allocation-service/internal/repository"
	"github.com/gin-gonic/gin"
)

type cardActionRequest struct {
	Action        string `json:"action" binding:"max=16"`
	OriginalCode  string `json:"original_code" binding:"max=32"`
	NewCode       string `json:"new_code" binding:"max=32"`
	CaptchaID     int64  `json:"captcha_id"`
	CaptchaAnswer string `json:"captcha_answer" binding:"max=16"`
}

func cardActionHandler(service *cardsvc.Service, action string, preview bool, allowedOrigin string) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		if os.Getenv("PUBLIC_CARD_ACTIONS_ENABLED") == "false" {
			writeError(c, http.StatusServiceUnavailable, "card_actions_unavailable", "续费与不同账号兑换暂时关闭，普通查询不受影响")
			return
		}
		var input cardActionRequest
		if !bindJSON(c, &input, 4096) {
			return
		}
		if action != "" {
			if input.Action != "" && input.Action != action {
				writeError(c, 422, "card_action_invalid", "兑换码或操作不可用，请检查后重试")
				return
			}
			input.Action = action
		}
		if c.GetHeader("Sec-Fetch-Site") == "cross-site" {
			writeError(c, 403, "origin_not_allowed", "请求来源不允许")
			return
		}
		// Pages submits through a fixed server-side proxy without an Origin header.
		if origin := c.GetHeader("Origin"); origin != "" && origin != allowedOrigin && origin != "https://"+c.Request.Host && origin != "http://"+c.Request.Host {
			writeError(c, 403, "origin_not_allowed", "请求来源不允许")
			return
		}
		result, err := service.PerformAction(c.Request.Context(), cardsvc.ActionInput{Action: input.Action, OriginalCode: input.OriginalCode, NewCode: input.NewCode, Preview: preview, ClientIP: cardActionClientIP(c.Request), CaptchaID: input.CaptchaID, CaptchaAnswer: input.CaptchaAnswer})
		switch {
		case err == nil:
			operation := result.Operation
			// Hide database IDs and account details from the public action endpoints.
			c.JSON(200, gin.H{"operation": gin.H{"action": operation.Action, "mode": operation.Mode, "added_days": operation.AddedDays, "duration_days": operation.DurationDays, "status": operation.Status, "original_status": operation.OriginalStatus, "before_expires_at": operation.BeforeExpiresAt, "expires_at": operation.ExpiresAt, "account_changed": operation.AccountChanged, "replayed": operation.Replayed}, "preview": preview, "request_id": c.GetString("request_id")})
		case errors.Is(err, repository.ErrCaptchaRequired) && result.Captcha != nil:
			c.JSON(403, gin.H{"code": "captcha_required", "message": "需要完成验证码后继续", "captcha": gin.H{"id": result.Captcha.ID, "question": result.Captcha.Question, "expires_at": result.Captcha.ExpiresAt}, "request_id": c.GetString("request_id")})
		case errors.Is(err, repository.ErrCaptchaInvalid):
			writeError(c, 403, "captcha_invalid", "验证码无效或已过期，请重新预览")
		case errors.Is(err, repository.ErrNoAccountCapacity):
			message := "暂无可分配账号，新兑换码未消耗"
			if input.Action == "different" {
				message = "暂无不同账号可分配，新兑换码未消耗"
			}
			writeError(c, 409, "no_account_capacity", message)
		case errors.Is(err, cardsvc.ErrValidation):
			writeError(c, 422, "card_action_invalid", "兑换码或操作不可用，请检查后重试")
		case errors.Is(err, repository.ErrCardDurationLimit):
			writeError(c, 422, "card_duration_invalid", "卡密时长超出可表示范围")
		default:
			writeError(c, http.StatusInternalServerError, "internal_error", "操作未完成，请使用相同兑换码重试")
		}
	}
}

// Only the local reverse proxy is trusted to supply its sanitized X-Real-IP.
// Do not trust arbitrary forwarded headers from public peers.
func cardActionClientIP(request *http.Request) string {
	peer := net.ParseIP(clientIP(request))
	if peer != nil && peer.IsLoopback() {
		if original := net.ParseIP(request.Header.Get("X-Real-IP")); original != nil {
			return original.String()
		}
	}
	return clientIP(request)
}
