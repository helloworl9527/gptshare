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
		return accountsync.Result{}, ErrAccountArchived
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
		status := eventAllocationStatus(event.Status, false, 0, capacity, event.SubscriptionExpiry, now)
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
		status := eventAllocationStatus(event.Status, credentialsComplete, allocations, capacity, event.SubscriptionExpiry, now)
		if _, err := tx.ExecContext(ctx, `UPDATE chatgpt_accounts SET display_username=?,account_expiry=?,monitor_status=?,status=?,
			monitor_sync_version=?,monitor_plan=?,updated_at=? WHERE id=?`, username, formatTime(event.SubscriptionExpiry.UTC()), event.Status,
			status, event.Version, event.Plan, formatTime(now), accountID); err != nil {
			return accountsync.Result{}, err
		}
	}
	result := accountsync.Result{Disposition: "applied", Created: created, Updated: !created}
	if event.Status == "dead_banned" {
		due, err := bannedAccountAllocations(ctx, tx, accountID, now)
		if err != nil {
			return accountsync.Result{}, err
		}
		for _, item := range due {
			_, err := replaceOneAllocation(ctx, tx, now, item)
			if errors.Is(err, ErrNoAccountCapacity) {
				result.Pending++
				if err := auditWithTx(ctx, tx, r.now, "replacement.failed", "card", item.cardID, map[string]any{
					"reason": "banned", "old_account_id": accountID,
				}); err != nil {
					return accountsync.Result{}, err
				}
				continue
			}
			if err != nil {
				return accountsync.Result{}, err
			}
			result.Migrated++
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO monitor_account_events(event_id,monitor_account_id,account_version,event_type,disposition,processed_at)
		VALUES (?,?,?,?,'applied',?)`, event.EventID, event.ProviderAccountID, event.Version, event.Type, formatTime(now)); err != nil {
		return accountsync.Result{}, err
	}
	if err := auditWithTx(ctx, tx, r.now, "monitor_account_event.applied", "account", accountID, map[string]any{
		"event_id": event.EventID, "event_type": event.Type, "version": event.Version, "migrated": result.Migrated, "pending": result.Pending,
	}); err != nil {
		return accountsync.Result{}, err
	}
	if err := tx.Commit(); err != nil {
		return accountsync.Result{}, err
	}
	return result, nil
}

func eventAllocationStatus(monitorStatus string, credentialsComplete bool, allocations, capacity int, expiry, now time.Time) string {
	switch monitorStatus {
	case "dead_banned":
		return "banned"
	case "dead_normal":
		return "expired"
	}
	if !expiry.After(now) {
		return "expired"
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
