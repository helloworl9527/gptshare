package repository

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"allocation-service/accountsync"
)

func (r *Repository) ApplyMonitorAccountEvent(ctx context.Context, event accountsync.Event) (accountsync.Result, error) {
	if err := event.Validate(); err != nil {
		return accountsync.Result{}, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return accountsync.Result{}, err
	}
	defer tx.Rollback()
	var processed int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM monitor_account_events WHERE event_id=?`, event.EventID).Scan(&processed); err != nil {
		return accountsync.Result{}, err
	}
	if processed != 0 {
		return accountsync.Result{Disposition: "duplicate"}, tx.Commit()
	}
	now := r.now().UTC()
	var accountID, currentVersion int64
	var archived sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT id,monitor_sync_version,archived_at FROM chatgpt_accounts WHERE monitor_account_id=?`, event.ProviderAccountID).
		Scan(&accountID, &currentVersion, &archived)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return accountsync.Result{}, err
	}
	if accountID != 0 && event.Version <= currentVersion {
		if _, err := tx.ExecContext(ctx, `INSERT INTO monitor_account_events(event_id,monitor_account_id,account_version,event_type,disposition,processed_at)
			VALUES (?,?,?,?,'stale',?)`, event.EventID, event.ProviderAccountID, event.Version, event.Type, formatTime(now)); err != nil {
			return accountsync.Result{}, err
		}
		if err := tx.Commit(); err != nil {
			return accountsync.Result{}, err
		}
		return accountsync.Result{Disposition: "stale"}, nil
	}
	if archived.Valid {
		// 账号已下线（人工或封禁自动下线）。这里必须成功返回而不是报错，
		// 否则 outbox 会把后续事件当成投递失败并无限重试。
		if _, err := tx.ExecContext(ctx, `INSERT INTO monitor_account_events(event_id,monitor_account_id,account_version,event_type,disposition,processed_at)
			VALUES (?,?,?,?,'ignored_archived',?)`, event.EventID, event.ProviderAccountID, event.Version, event.Type, formatTime(now)); err != nil {
			return accountsync.Result{}, err
		}
		if err := tx.Commit(); err != nil {
			return accountsync.Result{}, err
		}
		return accountsync.Result{Disposition: "ignored_archived"}, nil
	}
	created := accountID == 0
	username := strings.TrimSpace(event.Email)
	if username == "" {
		username = event.ProviderAccountID
	}
	if created {
		if r.credentials == nil {
			return accountsync.Result{}, errors.New("allocation credential keyring is required")
		}
		capacity, err := r.defaultAccountCapacityTx(ctx, tx)
		if err != nil {
			return accountsync.Result{}, err
		}
		status := eventAllocationStatus(event.Status, event.Suspected, false, 0, capacity, event.SubscriptionExpiry, now)
		insert, err := tx.ExecContext(ctx, `INSERT INTO chatgpt_accounts
			(display_username,display_password_secret,display_password_key_id,display_2fa_secret,display_2fa_key_id,
			 account_expiry,max_concurrent_users,monitor_account_id,monitor_status,status,created_at,updated_at,monitor_sync_version,monitor_plan)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, username, []byte{}, r.credentials.ActiveKeyID(), []byte{}, r.credentials.ActiveKeyID(),
			formatTime(event.SubscriptionExpiry.UTC()), capacity, event.ProviderAccountID, event.Status, status,
			formatTime(now), formatTime(now), event.Version, event.Plan)
		if err != nil {
			return accountsync.Result{}, err
		}
		accountID, err = insert.LastInsertId()
		if err != nil {
			return accountsync.Result{}, err
		}
	} else {
		var credentialsComplete bool
		var allocations, capacity int
		if err := tx.QueryRowContext(ctx, `SELECT length(display_password_secret)>0 AND length(display_2fa_secret)>0,current_allocations,max_concurrent_users
			FROM chatgpt_accounts WHERE id=?`, accountID).Scan(&credentialsComplete, &allocations, &capacity); err != nil {
			return accountsync.Result{}, err
		}
		status := eventAllocationStatus(event.Status, event.Suspected, credentialsComplete, allocations, capacity, event.SubscriptionExpiry, now)
		if _, err := tx.ExecContext(ctx, `UPDATE chatgpt_accounts SET display_username=?,account_expiry=?,monitor_status=?,status=?,
			monitor_sync_version=?,monitor_plan=?,updated_at=? WHERE id=?`, username, formatTime(event.SubscriptionExpiry.UTC()), event.Status,
			status, event.Version, event.Plan, formatTime(now), accountID); err != nil {
			return accountsync.Result{}, err
		}
	}
	result := accountsync.Result{Disposition: "applied", Created: created, Updated: !created}
	if event.Status == "dead_banned" {
		if err := closeBannedGraceAllocations(ctx, tx, now); err != nil {
			return accountsync.Result{}, err
		}
		due, err := bannedAccountAllocations(ctx, tx, accountID, now)
		if err != nil {
			return accountsync.Result{}, err
		}
		for _, item := range due {
			_, err := replaceOneAllocationIsolated(ctx, tx, now, item)
			if errors.Is(err, errReplacementAborted) {
				return accountsync.Result{}, err
			}
			if err != nil {
				result.Pending++
				if err := auditWithTx(ctx, tx, r.now, "replacement.failed", "card", item.cardID, map[string]any{
					"reason": "banned", "old_account_id": accountID, "error_code": replacementErrorCode(err),
				}); err != nil {
					return accountsync.Result{}, err
				}
				continue
			}
			result.Migrated++
		}
		// 顾客全部迁走后自动下线；还有人没迁走就先留着，等每小时换号扫描补齐再下线。
		retired, err := archiveRetiredAccount(ctx, tx, r.now, accountID, RetireReasonBanned, now)
		if err != nil {
			return accountsync.Result{}, err
		}
		result.Retired = retired
	}
	if event.Status == "dead_normal" {
		// 订阅正常到期：没有顾客在用就直接下线，还有顾客的等换号扫描迁走后再下线。
		retired, err := archiveRetiredAccount(ctx, tx, r.now, accountID, RetireReasonExpired, now)
		if err != nil {
			return accountsync.Result{}, err
		}
		result.Retired = retired
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO monitor_account_events(event_id,monitor_account_id,account_version,event_type,disposition,processed_at)
		VALUES (?,?,?,?,'applied',?)`, event.EventID, event.ProviderAccountID, event.Version, event.Type, formatTime(now)); err != nil {
		return accountsync.Result{}, err
	}
	if err := auditWithTx(ctx, tx, r.now, "monitor_account_event.applied", "account", accountID, map[string]any{
		"event_id": event.EventID, "event_type": event.Type, "version": event.Version,
		"migrated": result.Migrated, "pending": result.Pending, "retired": result.Retired,
	}); err != nil {
		return accountsync.Result{}, err
	}
	if err := tx.Commit(); err != nil {
		return accountsync.Result{}, err
	}
	return result, nil
}

func eventAllocationStatus(monitorStatus string, suspected, credentialsComplete bool, allocations, capacity int, expiry, now time.Time) string {
	switch monitorStatus {
	case "dead_banned":
		return "banned"
	case "dead_normal":
		return "expired"
	}
	if !expiry.After(now) {
		return "expired"
	}
	// 疑似封禁：退出分配池但不动存量顾客，等人工确认。
	if suspected {
		return "suspected"
	}
	if !credentialsComplete {
		return "pending_credentials"
	}
	if allocations >= capacity {
		return "full"
	}
	return "available"
}

func bannedAccountAllocations(ctx context.Context, tx *sql.Tx, accountID int64, now time.Time) ([]replacementDueAllocation, error) {
	rows, err := tx.QueryContext(ctx, `SELECT a.id,a.card_id,a.account_id,c.expires_at
		FROM allocations a JOIN cards c ON c.id=a.card_id
		WHERE a.account_id=? AND a.active=1 AND a.allocation_state='primary' AND c.status='redeemed' AND datetime(c.expires_at)>datetime(?)
		ORDER BY a.id`, accountID, formatTime(now))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []replacementDueAllocation
	for rows.Next() {
		var item replacementDueAllocation
		var expiry string
		if err := rows.Scan(&item.allocationID, &item.cardID, &item.oldAccountID, &expiry); err != nil {
			return nil, err
		}
		item.cardExpiresAt, err = parseTime(expiry)
		if err != nil {
			return nil, err
		}
		item.reason = "banned"
		result = append(result, item)
	}
	return result, rows.Err()
}
