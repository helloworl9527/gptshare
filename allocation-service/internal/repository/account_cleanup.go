package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// PrepareExpiredAccountCleanup retires the pool entry before the monitor removes
// its credentials. The retained archived row rejects late sync events and cannot
// receive a new allocation. Even grace allocations block cleanup.
func (r *Repository) PrepareExpiredAccountCleanup(ctx context.Context, providerID string, now time.Time) (bool, error) {
	if providerID == "" {
		return false, nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var id int64
	var archived sql.NullString
	var active int
	var expired bool
	err = tx.QueryRowContext(ctx, `SELECT id,archived_at,
		(SELECT count(*) FROM allocations WHERE account_id=chatgpt_accounts.id AND active=1),
		COALESCE(julianday(account_expiry)<=julianday(?),0)
		FROM chatgpt_accounts WHERE monitor_account_id=?`, formatTime(now), providerID).Scan(&id, &archived, &active, &expired)
	if errors.Is(err, sql.ErrNoRows) {
		// Retain a tombstone even for monitor-only accounts. A facade snapshot
		// taken before deletion or a replayed event must not recreate a live row.
		stamp := formatTime(now)
		result, err := tx.ExecContext(ctx, `INSERT INTO chatgpt_accounts
			(display_username,display_password_secret,display_password_key_id,display_2fa_secret,display_2fa_key_id,
			account_expiry,max_concurrent_users,monitor_account_id,monitor_status,status,created_at,updated_at,archived_at)
			VALUES (?,x'','',x'','',?,1,?,'dead_normal','disabled',?,?,?)`, providerID, stamp, providerID, stamp, stamp, stamp)
		if err != nil {
			return false, err
		}
		id, err = result.LastInsertId()
		if err != nil {
			return false, err
		}
		if err := auditWithTx(ctx, tx, func() time.Time { return now }, "account.retired", "account", id, map[string]any{"reason": RetireReasonExpired, "operator": "system"}); err != nil {
			return false, err
		}
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return true, nil
	}
	if err != nil || active != 0 {
		return false, err
	}
	if archived.Valid {
		return true, nil
	}
	// A future pool expiry may represent a renewal not yet observed by the
	// monitor. Do not retire it based only on an old terminal status.
	if !expired {
		return false, nil
	}
	retired, err := archiveRetiredAccount(ctx, tx, func() time.Time { return now }, id, RetireReasonExpired, now)
	if err != nil || !retired {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
