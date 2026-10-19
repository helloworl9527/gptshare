package usercenter

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"allocation-service/internal/repository"
)

const (
	TokenHeader = "X-UC-Token"

	DefaultEventLimit = 200
	MaxEventLimit     = 500
	MaxStatusRefs     = 500

	// expiringWindow 内到期的权益在面板上显示为 expiring。
	expiringWindow = 3 * 24 * time.Hour
	// nonceRetention 是 nonce 过期后继续保留的时间，必须大于 ClockSkew：
	// 票据在 exp+30s 之前仍会被接受，nonce 不能提前被清理。
	nonceRetention = 5 * time.Minute
)

var ErrUnknownRef = errors.New("entitlement ref is not known")

type Repository interface {
	ListRedemptionEvents(context.Context, int64, int) ([]repository.RedemptionEvent, error)
	BoundEntitlements(context.Context, []int64) (map[int64]repository.BoundEntitlement, error)
	BoundCardCode(context.Context, int64) (string, error)
}

type Config struct {
	// TicketKey 是本服务独立的 32 字节票据 HMAC 密钥。
	TicketKey []byte
	// InternalToken 是 X-UC-Token 的共享密钥（原样比较）。
	InternalToken string
	// DetailOrigin 是 detail-link 生成的公开兑换页 origin，例如 https://gpt.baochai.cc。
	DetailOrigin string
}

type Service struct {
	repo         Repository
	verifier     *Verifier
	tokenDigest  [sha256.Size]byte
	detailOrigin string
	now          func() time.Time
}

type Event struct {
	ID             int64           `json:"id"`
	EventUID       string          `json:"event_uid"`
	Kind           string          `json:"kind"`
	CodeHash       string          `json:"code_hash"`
	EntitlementRef string          `json:"entitlement_ref"`
	UserUID        string          `json:"user_uid"`
	SucceededAt    string          `json:"succeeded_at"`
	Detail         json.RawMessage `json:"detail"`
}

type StatusItem struct {
	State    string            `json:"state"`
	Title    string            `json:"title"`
	ExpireAt string            `json:"expire_at,omitempty"`
	Display  map[string]string `json:"display"`
}

func NewService(repo Repository, cfg Config) (*Service, error) {
	if repo == nil {
		return nil, errors.New("user center repository is required")
	}
	verifier, err := NewVerifier(cfg.TicketKey)
	if err != nil {
		return nil, err
	}
	if len(cfg.InternalToken) < 32 {
		return nil, errors.New("user center internal token must be at least 32 characters")
	}
	origin, err := url.Parse(cfg.DetailOrigin)
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.Path != "" || origin.RawQuery != "" || origin.Fragment != "" || origin.User != nil {
		return nil, errors.New("user center detail origin must be an HTTPS origin")
	}
	return &Service{
		repo:         repo,
		verifier:     verifier,
		tokenDigest:  sha256.Sum256([]byte(cfg.InternalToken)),
		detailOrigin: origin.Scheme + "://" + origin.Host,
		now:          func() time.Time { return time.Now().UTC() },
	}, nil
}

func (s *Service) SetNow(now func() time.Time) {
	if now != nil {
		s.now = now
		s.verifier.SetNow(now)
	}
}

func (s *Service) VerifyTicket(raw string) (Ticket, error) {
	return s.verifier.Verify(raw)
}

// AuthorizeToken 常量时间比较 X-UC-Token；先取摘要再比较，长度也不泄露。
func (s *Service) AuthorizeToken(value string) bool {
	if value == "" {
		return false
	}
	digest := sha256.Sum256([]byte(value))
	return subtle.ConstantTimeCompare(digest[:], s.tokenDigest[:]) == 1
}

// Events 返回 id > after 的事件（升序）与下一页游标。
func (s *Service) Events(ctx context.Context, after int64, limit int) ([]Event, int64, error) {
	if limit <= 0 {
		limit = DefaultEventLimit
	}
	if limit > MaxEventLimit {
		limit = MaxEventLimit
	}
	if after < 0 {
		after = 0
	}
	rows, err := s.repo.ListRedemptionEvents(ctx, after, limit)
	if err != nil {
		return nil, 0, err
	}
	events := make([]Event, 0, len(rows))
	next := after
	for _, row := range rows {
		detail := json.RawMessage(row.Detail)
		if !json.Valid(detail) {
			detail = json.RawMessage(`{}`)
		}
		events = append(events, Event{
			ID: row.ID, EventUID: row.EventUID, Kind: row.Kind, CodeHash: row.CodeHash,
			EntitlementRef: row.EntitlementRef, UserUID: row.UserUID, SucceededAt: row.SucceededAt, Detail: detail,
		})
		next = row.ID
	}
	return events, next, nil
}

// Status 只返回认识的 ref；display 只放脱敏信息。
func (s *Service) Status(ctx context.Context, refs []string) (map[string]StatusItem, error) {
	ids := make([]int64, 0, len(refs))
	for _, ref := range refs {
		if id, ok := repository.ParseCardEntitlementRef(ref); ok {
			ids = append(ids, id)
		}
	}
	items := make(map[string]StatusItem, len(ids))
	if len(ids) == 0 {
		return items, nil
	}
	bound, err := s.repo.BoundEntitlements(ctx, ids)
	if err != nil {
		return nil, err
	}
	now := s.now()
	for id, entitlement := range bound {
		state := entitlementState(entitlement, now)
		item := StatusItem{
			State:   state,
			Title:   "ChatGPT 共享账号 · " + strconv.Itoa(entitlement.DurationDays) + " 天",
			Display: map[string]string{},
		}
		if entitlement.ExpiresAt != nil {
			item.ExpireAt = entitlement.ExpiresAt.In(displayZone).Format(time.DateOnly)
		}
		if (state == "active" || state == "expiring") && entitlement.CurrentUsername != "" {
			item.Display["当前账号"] = MaskUsername(entitlement.CurrentUsername)
		}
		items[repository.CardEntitlementRef(id)] = item
	}
	return items, nil
}

// DetailLink 按权益查出卡密，放在 URL 片段里（不进路径 / 查询参数，也就不进服务端日志）。
func (s *Service) DetailLink(ctx context.Context, ref string) (string, error) {
	id, ok := repository.ParseCardEntitlementRef(ref)
	if !ok {
		return "", ErrUnknownRef
	}
	code, err := s.repo.BoundCardCode(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrUnknownRef
	}
	if err != nil {
		return "", err
	}
	return s.detailOrigin + "/#code=" + url.QueryEscape(code), nil
}

// NonceCleanupCutoff 返回可安全清理的 nonce exp 上限（unix 秒）：
// exp 早于它的票据已经不可能再通过校验，删掉 nonce 也不会被重放。
func NonceCleanupCutoff(now time.Time) int64 {
	return now.Add(-nonceRetention).Unix()
}

func entitlementState(entitlement repository.BoundEntitlement, now time.Time) string {
	switch entitlement.Status {
	case "expired", "revoked":
		return "expired"
	case "redeemed":
		if entitlement.ExpiresAt == nil {
			return "unknown"
		}
		if !entitlement.ExpiresAt.After(now) {
			return "expired"
		}
		if entitlement.ExpiresAt.Sub(now) <= expiringWindow {
			return "expiring"
		}
		return "active"
	default:
		return "unknown"
	}
}

// MaskUsername 把共享账号名脱敏成 "share-07＠***.com"：保留本地部分与顶级域，
// 隐去域名主体；全角＠避免被当成可点击邮箱。非邮箱形式只保留前几个字符。
func MaskUsername(username string) string {
	username = strings.TrimSpace(username)
	local, domain, ok := strings.Cut(username, "@")
	if ok && local != "" {
		tld := ""
		if index := strings.LastIndex(domain, "."); index >= 0 && index < len(domain)-1 {
			tld = domain[index:]
		}
		return local + "＠***" + tld
	}
	runes := []rune(username)
	keep := len(runes) / 2
	if keep > 4 {
		keep = 4
	}
	return string(runes[:keep]) + "***"
}

var displayZone = time.FixedZone("UTC+8", 8*60*60)
