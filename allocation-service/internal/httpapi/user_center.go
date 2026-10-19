package httpapi

import (
	"errors"
	"net"
	"net/http"
	"strconv"

	allocatorsvc "allocation-service/internal/allocator"
	"allocation-service/internal/usercenter"
	"github.com/gin-gonic/gin"
)

const userCenterInternalPrefix = "/internal/uc"

// forwardedHeaders 中任一出现都说明请求经过了反向代理，不是本机直连。
var forwardedHeaders = []string{"X-Real-IP", "X-Forwarded-For", "Forwarded", "X-Forwarded-Host", "X-Forwarded-Proto", "CF-Connecting-IP", "True-Client-IP"}

// bindTicketFromRequest 解析可选的 X-Bind-Ticket。没带这个头 = 匿名兑换（nil, true）；
// 带了但校验失败、或本服务未启用票据功能时返回 false，调用方必须拒绝而不是降级成匿名兑换。
func bindTicketFromRequest(c *gin.Context, service *usercenter.Service) (*allocatorsvc.Binding, bool) {
	values := c.Request.Header.Values(usercenter.TicketHeader)
	if len(values) == 0 {
		return nil, true
	}
	if len(values) != 1 || service == nil {
		return nil, false
	}
	ticket, err := service.VerifyTicket(values[0])
	if err != nil {
		return nil, false
	}
	return &allocatorsvc.Binding{UserUID: ticket.UserUID, Nonce: ticket.Nonce, Exp: ticket.Exp}, true
}

func writeTicketInvalid(c *gin.Context) {
	c.JSON(http.StatusBadRequest, gin.H{
		"ok": false,
		"error": gin.H{
			"code":    "ticket_invalid",
			"message": "登录状态已过期，请刷新页面后重试",
		},
	})
}

// RegisterUserCenterInternalRoutes 注册只在本机直连可用的内部接口。
// 未通过鉴权（无令牌 / 令牌错误 / 带转发头 / 非回环来源）一律 404，不暴露接口存在。
func RegisterUserCenterInternalRoutes(router *gin.Engine, service *usercenter.Service) {
	if service == nil {
		return
	}
	internal := router.Group(userCenterInternalPrefix, requireLocalUserCenter(service), noStore())
	internal.GET("/events", userCenterEventsHandler(service))
	internal.POST("/status", userCenterStatusHandler(service))
	internal.POST("/detail-link", userCenterDetailLinkHandler(service))
}

func requireLocalUserCenter(service *usercenter.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		if !directLoopbackRequest(c.Request) || !service.AuthorizeToken(c.GetHeader(usercenter.TokenHeader)) {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		c.Next()
	}
}

func directLoopbackRequest(request *http.Request) bool {
	for _, name := range forwardedHeaders {
		if len(request.Header.Values(name)) > 0 {
			return false
		}
	}
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func userCenterEventsHandler(service *usercenter.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		after, ok := optionalInt64Query(c, "after", 0)
		if !ok || after < 0 {
			writeError(c, http.StatusBadRequest, "validation_failed", "after must be a non-negative integer")
			return
		}
		limit, ok := optionalInt64Query(c, "limit", usercenter.DefaultEventLimit)
		if !ok || limit < 1 {
			writeError(c, http.StatusBadRequest, "validation_failed", "limit must be a positive integer")
			return
		}
		if limit > usercenter.MaxEventLimit {
			limit = usercenter.MaxEventLimit
		}
		events, next, err := service.Events(c.Request.Context(), after, int(limit))
		if err != nil {
			writeError(c, http.StatusInternalServerError, "internal_error", "request could not be processed")
			return
		}
		c.JSON(http.StatusOK, gin.H{"events": events, "next_after": next})
	}
}

type userCenterStatusRequest struct {
	Refs []string `json:"refs" binding:"required,max=500,dive,max=64"`
}

func userCenterStatusHandler(service *usercenter.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request userCenterStatusRequest
		if !bindJSON(c, &request, 64*1024) {
			return
		}
		items, err := service.Status(c.Request.Context(), request.Refs)
		if err != nil {
			writeError(c, http.StatusInternalServerError, "internal_error", "request could not be processed")
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": items})
	}
}

type userCenterDetailLinkRequest struct {
	Ref string `json:"ref" binding:"required,max=64"`
}

func userCenterDetailLinkHandler(service *usercenter.Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		var request userCenterDetailLinkRequest
		if !bindJSON(c, &request, 4096) {
			return
		}
		link, err := service.DetailLink(c.Request.Context(), request.Ref)
		switch {
		case errors.Is(err, usercenter.ErrUnknownRef):
			writeError(c, http.StatusNotFound, "not_found", "entitlement not found")
		case err != nil:
			writeError(c, http.StatusInternalServerError, "internal_error", "request could not be processed")
		default:
			c.JSON(http.StatusOK, gin.H{"url": link})
		}
	}
}

func optionalInt64Query(c *gin.Context, name string, fallback int64) (int64, bool) {
	raw, present := c.GetQuery(name)
	if !present || raw == "" {
		return fallback, true
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}
