package repository

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrBindNonceReused 表示绑定票据的 nonce 已在之前的扣码事务里用过（重放）。
var ErrBindNonceReused = errors.New("bind ticket nonce already used")

const (
	RedemptionKindNew   = "new"
	RedemptionKindRenew = "renew"

	cardEntitlementPrefix = "card:"
)

// displayZone 用于事件 detail / 状态里给用户看的日期（YYYY-MM-DD）。
// 用户在中国，按北京时间取日期；固定偏移不依赖容器内的 tzdata。
var displayZone = time.FixedZone("UTC+8", 8*60*60)

// RedemptionBinding 是一次带有效票据的兑换要在扣码事务里落下的归属信息。
type RedemptionBinding struct {
	UserUID string
	Nonce   string
	// NonceExp 是票据的 exp（unix 秒），过期 nonce 由后台任务清理。
	NonceExp int64
	// CodeHashHex 是 sha256(规范化后的卡密) 的十六进制，与卡表里带域前缀的 code_hash 不同。
	CodeHashHex string
}

type RedemptionEvent struct {
	ID             int64
	EventUID       string
	Kind           string
	CodeHash       string
	EntitlementRef string
	UserUID        string
	SucceededAt    string
	Detail         string
}

// BoundEntitlement 是一张已绑定到用户中心的卡的当前状态（不含任何凭证）。
type BoundEntitlement struct {
	CardID          int64
	DurationDays    int
	Status          string
	ExpiresAt       *time.Time
	CurrentUsername string
}

// CardEntitlementRef 返回卡对应的权益引用。权益永远落在客户自己的卡上，
// 不同客户分到同一个共享账号时各自独立，不能合并。
func CardEntitlementRef(cardID int64) string {
	return cardEntitlementPrefix + strconv.FormatInt(cardID, 10)
}

// ParseCardEntitlementRef 解析 "card:<id>"；格式不对返回 false。
func ParseCardEntitlementRef(ref string) (int64, bool) {
	raw, ok := strings.CutPrefix(ref, cardEntitlementPrefix)
	if !ok || raw == "" || len(raw) > 19 || strings.TrimLeft(raw, "0123456789") != "" || (len(raw) > 1 && raw[0] == '0') {
		return 0, false
	}
	id, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || id <= 0 {
		return 0, false
	}
	return id, true
}

func consumeBindNonce(ctx context.Context, tx *sql.Tx, binding *RedemptionBinding) error {
	if binding == nil {
		return nil
	}
	if binding.UserUID == "" || binding.Nonce == "" || binding.CodeHashHex == "" {
		return errors.New("redemption binding is incomplete")
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO used_bind_nonces(nonce,exp) VALUES (?,?) ON CONFLICT(nonce) DO NOTHING`, binding.Nonce, binding.NonceExp)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected != 1 {
		return ErrBindNonceReused
	}
	return nil
}

func insertRedemptionEvent(ctx context.Context, tx *sql.Tx, binding *RedemptionBinding, kind string, cardID int64, durationDays int, expiresAt, now time.Time) error {
	if binding == nil {
		return nil
	}
	eventUID, err := newUUID4()
	if err != nil {
		return err
	}
	detail, err := json.Marshal(struct {
		Plan     string `json:"plan"`
		ExpireAt string `json:"expire_at"`
	}{Plan: strconv.Itoa(durationDays) + "天", ExpireAt: expiresAt.In(displayZone).Format(time.DateOnly)})
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO redemption_events
		(event_uid,kind,code_hash,entitlement_ref,user_uid,succeeded_at,detail)
		VALUES (?,?,?,?,?,?,?)`,
		eventUID, kind, binding.CodeHashHex, CardEntitlementRef(cardID), binding.UserUID, now.UTC().Format("2006-01-02T15:04:05Z"), string(detail))
	return err
}

// ListRedemptionEvents 按 id 升序返回 id > after 的事件。
func (r *Repository) ListRedemptionEvents(ctx context.Context, after int64, limit int) ([]RedemptionEvent, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("limit must be positive")
	}
	rows, err := r.db.QueryContext(ctx, `SELECT id,event_uid,kind,code_hash,entitlement_ref,user_uid,succeeded_at,detail
		FROM redemption_events WHERE id > ? ORDER BY id ASC LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]RedemptionEvent, 0)
	for rows.Next() {
		var event RedemptionEvent
		if err := rows.Scan(&event.ID, &event.EventUID, &event.Kind, &event.CodeHash, &event.EntitlementRef, &event.UserUID, &event.SucceededAt, &event.Detail); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

// BoundEntitlements 只返回出现在兑换事件里的卡；未绑定或不存在的卡直接略过，
// 这样持有内部令牌的一方也无法借此枚举任意卡。
func (r *Repository) BoundEntitlements(ctx context.Context, cardIDs []int64) (map[int64]BoundEntitlement, error) {
	result := make(map[int64]BoundEntitlement, len(cardIDs))
	for _, cardID := range cardIDs {
		if _, seen := result[cardID]; seen {
			continue
		}
		item, err := r.boundEntitlement(ctx, cardID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		result[cardID] = item
	}
	return result, nil
}

func (r *Repository) boundEntitlement(ctx context.Context, cardID int64) (BoundEntitlement, error) {
	item := BoundEntitlement{CardID: cardID}
	var expiresAt sql.NullString
	if err := r.db.QueryRowContext(ctx, `SELECT c.duration_days,c.status,c.expires_at FROM cards c
		WHERE c.id=? AND EXISTS (SELECT 1 FROM redemption_events e WHERE e.entitlement_ref=?)`,
		cardID, CardEntitlementRef(cardID)).Scan(&item.DurationDays, &item.Status, &expiresAt); err != nil {
		return BoundEntitlement{}, err
	}
	if expiresAt.Valid {
		parsed, err := parseTime(expiresAt.String)
		if err != nil {
			return BoundEntitlement{}, err
		}
		item.ExpiresAt = &parsed
	}
	var username string
	err := r.db.QueryRowContext(ctx, `SELECT acct.display_username FROM allocations a
		JOIN chatgpt_accounts acct ON acct.id=a.account_id
		WHERE a.card_id=? AND a.active=1 AND a.allocation_state IN ('primary','grace')
		ORDER BY CASE a.allocation_state WHEN 'primary' THEN 0 ELSE 1 END, a.id DESC
		LIMIT 1`, cardID).Scan(&username)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return BoundEntitlement{}, err
	}
	item.CurrentUsername = username
	return item, nil
}

// BoundCardCode 解密一张已绑定卡的明文卡密；未绑定、不存在或没有加密存档时返回 sql.ErrNoRows。
func (r *Repository) BoundCardCode(ctx context.Context, cardID int64) (string, error) {
	var bound int
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM redemption_events WHERE entitlement_ref=?`, CardEntitlementRef(cardID)).Scan(&bound); err != nil {
		return "", err
	}
	if bound == 0 {
		return "", sql.ErrNoRows
	}
	revealed, err := r.RevealCardCode(ctx, cardID)
	if err != nil {
		return "", err
	}
	if !revealed.Available || revealed.Code == "" {
		return "", sql.ErrNoRows
	}
	return revealed.Code, nil
}

// PurgeExpiredBindNonces 删除 exp 早于 cutoff（unix 秒）的 nonce。
// 调用方必须保证 cutoff 落在票据的时钟偏差窗口之外，否则过期前被删的 nonce 可被重放。
func (r *Repository) PurgeExpiredBindNonces(ctx context.Context, cutoff int64) (int64, error) {
	result, err := r.db.ExecContext(ctx, `DELETE FROM used_bind_nonces WHERE exp < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func newUUID4() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(value[:])
	return encoded[0:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:32], nil
}
