package repository

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

var ErrInvalidCardAction = errors.New("invalid card action")
var ErrCardActionConflict = errors.New("card already used for another action")

type CardActionResult struct {
	Action          string     `json:"action"`
	Mode            string     `json:"mode"`
	CardID          int64      `json:"card_id"`
	TargetCardID    int64      `json:"target_card_id"`
	SourceCardID    int64      `json:"source_card_id"`
	AddedDays       int        `json:"added_days"`
	DurationDays    int        `json:"duration_days"`
	Status          string     `json:"status"`
	OriginalStatus  string     `json:"original_status"`
	BeforeExpiresAt *time.Time `json:"before_expires_at,omitempty"`
	ExpiresAt       *time.Time `json:"expires_at,omitempty"`
	AccountChanged  bool       `json:"account_changed"`
	Replayed        bool       `json:"replayed"`
}

func cardTimeAfterDays(base time.Time, days int) (time.Time, error) {
	// Bound by representable RFC3339 dates, rather than a business duration cap.
	if days < 1 || int64(days) > (time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC).Unix()-base.Unix())/86400 {
		return time.Time{}, ErrCardDurationLimit
	}
	return base.AddDate(0, 0, days), nil
}

func selectCardCandidate(ctx context.Context, tx *sql.Tx, now, expiry time.Time, monitorAvailable bool, excludedID, cardID int64, preferPrevious bool) (int64, error) {
	row := tx.QueryRowContext(ctx, `SELECT id FROM chatgpt_accounts
        WHERE `+allocatableAccountClause+`
          AND (?=0 OR lower(trim(display_username))!=(SELECT lower(trim(display_username)) FROM chatgpt_accounts WHERE id=?))
          AND NOT EXISTS (
            SELECT 1 FROM allocations a JOIN cards c ON c.id=a.card_id
              JOIN chatgpt_accounts occupied ON occupied.id=a.account_id
            WHERE c.group_id=(SELECT group_id FROM cards WHERE id=?) AND c.id!=?
              AND c.status='redeemed' AND datetime(c.expires_at)>datetime(?)
              AND a.active=1 AND datetime(a.valid_until)>datetime(?)
              AND (a.allocation_state='primary' OR (a.allocation_state='grace' AND datetime(a.grace_until)>datetime(?)))
              AND lower(trim(occupied.display_username))=lower(trim(chatgpt_accounts.display_username))
          )
        ORDER BY
          CASE WHEN ?=1 AND monitor_status!='dead_normal' AND id=(SELECT account_id FROM allocations WHERE card_id=? ORDER BY id DESC LIMIT 1) THEN 0 ELSE 1 END,
          CASE WHEN ?=0 THEN 1 ELSE CASE monitor_status WHEN 'alive' THEN 0 WHEN 'unknown' THEN 1 WHEN 'unknown_monitor' THEN 1 WHEN 'dead_normal' THEN 2 ELSE 3 END END,
          max(0,julianday(account_expiry)-julianday(?)),
          abs((julianday(account_expiry)-julianday(?))*86400),
          current_allocations,last_allocated_at IS NOT NULL,last_allocated_at,id LIMIT 1`,
		formatTime(now), excludedID, excludedID, cardID, cardID, formatTime(now), formatTime(now), formatTime(now), boolInt(preferPrevious), cardID, boolInt(monitorAvailable), formatTime(expiry), formatTime(expiry))
	var id int64
	err := row.Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNoAccountCapacity
	}
	return id, err
}

func ensureRenewedAccount(ctx context.Context, tx *sql.Tx, cardID int64, expiry, now time.Time, monitorAvailable bool) error {
	var allocationID, accountID int64
	var usable bool
	err := tx.QueryRowContext(ctx, `SELECT a.id,a.account_id,
      (acct.archived_at IS NULL AND acct.status IN ('available','full') AND acct.monitor_status NOT IN ('dead_banned','dead_normal') AND datetime(acct.account_expiry)>datetime(?))
      FROM allocations a JOIN chatgpt_accounts acct ON acct.id=a.account_id WHERE a.card_id=? AND a.active=1 AND a.allocation_state='primary'`, formatTime(now), cardID).Scan(&allocationID, &accountID, &usable)
	if err == nil && usable {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		_, err = replaceOneAllocation(ctx, tx, now, replacementDueAllocation{allocationID: allocationID, cardID: cardID, oldAccountID: accountID, cardExpiresAt: expiry, reason: ReplacementReasonExpired})
		return err
	}
	next, err := selectCardCandidate(ctx, tx, now, expiry, monitorAvailable, 0, cardID, true)
	if err != nil {
		return err
	}
	if err = reserveAccountCapacity(ctx, tx, next, now); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO allocations(card_id,account_id,allocated_at,valid_until,allocation_state,active,created_at,updated_at) VALUES (?,?,?,?,'primary',1,?,?)`, cardID, next, formatTime(now), formatTime(expiry), formatTime(now), formatTime(now))
	return err
}

type actionCard struct {
	id                int64
	days              int
	status            string
	group             sql.NullInt64
	redeemed, expires sql.NullString
}

func readActionCard(ctx context.Context, tx *sql.Tx, hash []byte) (actionCard, error) {
	var c actionCard
	err := tx.QueryRowContext(ctx, `SELECT id,duration_days,status,group_id,redeemed_at,expires_at FROM cards WHERE code_hash=?`, hash).Scan(&c.id, &c.days, &c.status, &c.group, &c.redeemed, &c.expires)
	return c, err
}

// Preview exercises exactly the same transaction and rolls it back. No credentials
// are loaded and no card/group/capacity changes survive a preview or failure.
func (r *Repository) ApplyCardAction(ctx context.Context, action string, targetHash, sourceHash []byte, preview, monitorAvailable bool) (CardActionResult, error) {
	var result CardActionResult
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		result, err = r.applyCardAction(ctx, action, targetHash, sourceHash, preview, monitorAvailable)
		if err == nil || !sqliteBusy(err) {
			return result, err
		}
		select {
		case <-ctx.Done():
			return CardActionResult{}, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 20 * time.Millisecond):
		}
	}
	return result, err
}

func sqliteBusy(err error) bool {
	type coded interface{ Code() int }
	var value coded
	return errors.As(err, &value) && (value.Code()&255 == 5 || value.Code()&255 == 6)
}

func (r *Repository) applyCardAction(ctx context.Context, action string, targetHash, sourceHash []byte, preview, monitorAvailable bool) (CardActionResult, error) {
	if action != "renew" && action != "different" {
		return CardActionResult{}, ErrInvalidCardAction
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return CardActionResult{}, err
	}
	defer tx.Rollback()
	target, err := readActionCard(ctx, tx, targetHash)
	if err != nil {
		return CardActionResult{}, err
	}
	source, err := readActionCard(ctx, tx, sourceHash)
	if err != nil {
		return CardActionResult{}, err
	}
	if target.id == source.id {
		return CardActionResult{}, ErrInvalidCardAction
	}
	var priorTarget int64
	var priorAction, priorJSON string
	err = tx.QueryRowContext(ctx, `SELECT target_card_id,action,result_json FROM card_operations WHERE source_card_id=?`, source.id).Scan(&priorTarget, &priorAction, &priorJSON)
	if err == nil {
		if priorTarget != target.id || priorAction != action {
			return CardActionResult{}, ErrCardActionConflict
		}
		var result CardActionResult
		if err = json.Unmarshal([]byte(priorJSON), &result); err != nil {
			return CardActionResult{}, err
		}
		result.Replayed = true
		return result, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return CardActionResult{}, err
	}
	if source.status != "unused" || (target.status != "unused" && target.status != "redeemed" && target.status != "expired") {
		return CardActionResult{}, ErrCardStateConflict
	}
	if action == "different" && !target.redeemed.Valid {
		return CardActionResult{}, ErrInvalidCardAction
	}
	now := r.now().UTC()
	result := CardActionResult{Action: action, CardID: target.id, TargetCardID: target.id, SourceCardID: source.id, AddedDays: source.days, OriginalStatus: target.status}
	if target.expires.Valid {
		t, e := parseTime(target.expires.String)
		if e != nil {
			return result, e
		}
		result.BeforeExpiresAt = &t
		if target.status == "redeemed" && !t.After(now) {
			result.OriginalStatus = "expired"
		}
	}
	if action == "renew" {
		var previousAccount sql.NullInt64
		if err = tx.QueryRowContext(ctx, `SELECT account_id FROM allocations WHERE card_id=? ORDER BY id DESC LIMIT 1`, target.id).Scan(&previousAccount); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return result, err
		}
		extended, e := extendCardWithTx(ctx, tx, target.id, source.days, monitorAvailable, now)
		if e != nil {
			return result, e
		}
		result.Mode = extended.Mode
		update, e := tx.ExecContext(ctx, `UPDATE cards SET status='consumed',updated_at=? WHERE id=? AND status='unused'`, formatTime(now), source.id)
		if e != nil {
			return result, e
		}
		if n, _ := update.RowsAffected(); n != 1 {
			return result, ErrCardStateConflict
		}
		var currentAccount sql.NullInt64
		if e = tx.QueryRowContext(ctx, `SELECT account_id FROM allocations WHERE card_id=? AND active=1 AND allocation_state='primary'`, target.id).Scan(&currentAccount); e != nil && !errors.Is(e, sql.ErrNoRows) {
			return result, e
		}
		result.AccountChanged = currentAccount.Valid && (!previousAccount.Valid || currentAccount.Int64 != previousAccount.Int64)
	} else {
		group := target.group
		if !group.Valid {
			inserted, e := tx.ExecContext(ctx, `INSERT INTO card_groups(created_at) VALUES (?)`, formatTime(now))
			if e != nil {
				return result, e
			}
			group.Int64, e = inserted.LastInsertId()
			if e != nil {
				return result, e
			}
			group.Valid = true
			if _, e = tx.ExecContext(ctx, `UPDATE cards SET group_id=? WHERE id=?`, group.Int64, target.id); e != nil {
				return result, e
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE cards SET group_id=? WHERE id=? AND status='unused'`, group.Int64, source.id); err != nil {
			return result, err
		}
		expiry, e := cardTimeAfterDays(now, source.days)
		if e != nil {
			return result, e
		}
		accountID, e := selectCardCandidate(ctx, tx, now, expiry, monitorAvailable, 0, source.id, false)
		if e != nil {
			return result, e
		}
		if e = reserveAccountCapacity(ctx, tx, accountID, now); e != nil {
			return result, e
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO allocations(card_id,account_id,allocated_at,valid_until,allocation_state,active,created_at,updated_at) VALUES (?,?,?,?,'primary',1,?,?)`, source.id, accountID, formatTime(now), formatTime(expiry), formatTime(now), formatTime(now)); e != nil {
			return result, e
		}
		update, e := tx.ExecContext(ctx, `UPDATE cards SET status='redeemed',redeemed_at=?,expires_at=?,updated_at=? WHERE id=? AND status='unused'`, formatTime(now), formatTime(expiry), formatTime(now), source.id)
		if e != nil {
			return result, e
		}
		if n, _ := update.RowsAffected(); n != 1 {
			return result, ErrCardStateConflict
		}
		result.CardID = source.id
		result.Mode = "different"
	}
	var expires sql.NullString
	if err = tx.QueryRowContext(ctx, `SELECT status,duration_days,expires_at FROM cards WHERE id=?`, result.CardID).Scan(&result.Status, &result.DurationDays, &expires); err != nil {
		return result, err
	}
	if expires.Valid {
		t, e := parseTime(expires.String)
		if e != nil {
			return result, e
		}
		result.ExpiresAt = &t
	}
	body, err := json.Marshal(result)
	if err != nil {
		return result, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO card_operations(action,target_card_id,source_card_id,added_days,before_expires_at,after_expires_at,result_json,created_at) VALUES (?,?,?,?,?,?,?,?)`, action, target.id, source.id, source.days, nullableTime(result.BeforeExpiresAt), nullableTime(result.ExpiresAt), string(body), formatTime(now)); err != nil {
		return result, err
	}
	if err = auditWithTx(ctx, tx, nil, "cards."+action, "card", target.id, map[string]any{"source_card_id": source.id, "added_days": source.days}); err != nil {
		return result, err
	}
	if !preview {
		err = tx.Commit()
	}
	return result, err
}

type CardOperation struct {
	Action          string  `json:"action"`
	TargetCardID    int64   `json:"target_card_id"`
	SourceCardID    int64   `json:"source_card_id"`
	TargetSuffix    string  `json:"target_suffix"`
	SourceSuffix    string  `json:"source_suffix"`
	AddedDays       int     `json:"added_days"`
	BeforeExpiresAt *string `json:"before_expires_at"`
	AfterExpiresAt  *string `json:"after_expires_at"`
	CreatedAt       string  `json:"created_at"`
}

func (r *Repository) CardOperations(ctx context.Context, id int64) ([]CardOperation, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT o.action,o.target_card_id,o.source_card_id,t.code_suffix,s.code_suffix,o.added_days,o.before_expires_at,o.after_expires_at,o.created_at FROM card_operations o JOIN cards t ON t.id=o.target_card_id JOIN cards s ON s.id=o.source_card_id WHERE o.target_card_id=? OR o.source_card_id=? ORDER BY o.id DESC LIMIT 100`, id, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []CardOperation{}
	for rows.Next() {
		var item CardOperation
		if err = rows.Scan(&item.Action, &item.TargetCardID, &item.SourceCardID, &item.TargetSuffix, &item.SourceSuffix, &item.AddedDays, &item.BeforeExpiresAt, &item.AfterExpiresAt, &item.CreatedAt); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}
