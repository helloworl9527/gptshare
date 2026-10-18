// Package usercenter 实现统一用户中心（baochai.cc）与本兑换服务之间的绑定契约：
// 公开兑换接口上的 X-Bind-Ticket 绑定票据校验，以及只在本机直连可用的内部接口。
//
// 归属不信任浏览器回报：票据由用户中心签发，本服务只在真正扣码的事务里
// 记录「这次兑换是哪个登录用户做的」，用户中心再从内部接口拉取事件。
package usercenter

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const (
	TicketHeader  = "X-Bind-Ticket"
	TicketVersion = 1
	TicketService = "sharegpt"
	// ClockSkew 是票据过期判定允许的时钟偏差：exp > now-30s 视为未过期。
	ClockSkew = 30 * time.Second

	maxTicketLength = 2048
	nonceLength     = 22
	maxUserUIDBytes = 128
)

// ErrTicketInvalid 覆盖所有票据失败原因（格式、签名、版本、服务名、过期、重放）。
// 对外只暴露这一种错误，避免给伪造者提供逐项探测的信号。
var ErrTicketInvalid = errors.New("bind ticket invalid")

// Ticket 是校验通过的票据载荷。
type Ticket struct {
	UserUID string
	Nonce   string
	Exp     int64
}

type ticketPayload struct {
	V     *int    `json:"v"`
	Svc   *string `json:"svc"`
	UID   *string `json:"uid"`
	Nonce *string `json:"nonce"`
	Exp   *int64  `json:"exp"`
}

// Verifier 用本服务独立的票据密钥校验绑定票据。
type Verifier struct {
	key []byte
	now func() time.Time
}

func NewVerifier(key []byte) (*Verifier, error) {
	if len(key) != 32 {
		return nil, errors.New("bind ticket key must be exactly 32 bytes")
	}
	return &Verifier{key: append([]byte(nil), key...), now: func() time.Time { return time.Now().UTC() }}, nil
}

func (v *Verifier) SetNow(now func() time.Time) {
	if now != nil {
		v.now = now
	}
}

// Verify 校验 `<payload_b64url>.<sig_b64url>`：
// sig = HMAC-SHA256(key, payload_b64url 的 ASCII 字节)，常量时间比较；
// 载荷必须 v==1、svc=="sharegpt"、exp > now-30s，uid/nonce 格式合法。
// 防重放（nonce 唯一）不在这里判断，由扣码事务里的 used_bind_nonces 主键保证。
func (v *Verifier) Verify(raw string) (Ticket, error) {
	if v == nil || len(raw) == 0 || len(raw) > maxTicketLength {
		return Ticket{}, ErrTicketInvalid
	}
	payloadPart, sigPart, ok := strings.Cut(raw, ".")
	if !ok || payloadPart == "" || sigPart == "" || strings.Contains(sigPart, ".") {
		return Ticket{}, ErrTicketInvalid
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(sigPart)
	if err != nil || len(signature) != sha256.Size {
		return Ticket{}, ErrTicketInvalid
	}
	mac := hmac.New(sha256.New, v.key)
	mac.Write([]byte(payloadPart))
	if !hmac.Equal(mac.Sum(nil), signature) {
		return Ticket{}, ErrTicketInvalid
	}
	payloadJSON, err := base64.RawURLEncoding.Strict().DecodeString(payloadPart)
	if err != nil {
		return Ticket{}, ErrTicketInvalid
	}
	var payload ticketPayload
	decoder := json.NewDecoder(bytes.NewReader(payloadJSON))
	if err := decoder.Decode(&payload); err != nil || decoder.More() {
		return Ticket{}, ErrTicketInvalid
	}
	if payload.V == nil || *payload.V != TicketVersion ||
		payload.Svc == nil || *payload.Svc != TicketService ||
		payload.UID == nil || !validUserUID(*payload.UID) ||
		payload.Nonce == nil || !validNonce(*payload.Nonce) ||
		payload.Exp == nil {
		return Ticket{}, ErrTicketInvalid
	}
	if *payload.Exp <= v.now().Add(-ClockSkew).Unix() {
		return Ticket{}, ErrTicketInvalid
	}
	return Ticket{UserUID: *payload.UID, Nonce: *payload.Nonce, Exp: *payload.Exp}, nil
}

func validNonce(value string) bool {
	// 只校验长度与 base64url 字符集，不要求尾部填充位为零：
	// 用户中心可能直接生成 22 个随机字符而不是编码 16 字节。
	if len(value) != nonceLength {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

func validUserUID(value string) bool {
	if value == "" || len(value) > maxUserUIDBytes {
		return false
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '-' || r == '.' || r == ':':
		default:
			return false
		}
	}
	return true
}
