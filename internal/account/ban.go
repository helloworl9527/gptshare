package account

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"allocation-service/accountsync"
	"chatgpt-monitor/internal/allocationsync"
)

func (s *Service) Ban(ctx context.Context, accountID int64) (BanResult, error) {
	if accountID <= 0 {
		return BanResult{}, &ServiceError{Kind: ErrorInvalid, Code: "account_id_invalid"}
	}
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return BanResult{}, internalError("transaction_begin")
	}
	defer tx.Rollback()
	var status, importedAt string
	var epochID int64
	if err := tx.QueryRowContext(ctx, `SELECT a.status,a.import_time,e.id FROM accounts a
		JOIN authorization_epochs e ON e.id=(SELECT id FROM authorization_epochs WHERE account_id=a.id ORDER BY started_at DESC,id DESC LIMIT 1)
		WHERE a.id=? AND a.deleted_at IS NULL`, accountID).Scan(&status, &importedAt, &epochID); err != nil {
		if err == sql.ErrNoRows {
			return BanResult{}, &ServiceError{Kind: ErrorNotFound, Code: "account_not_found"}
		}
		return BanResult{}, internalError("account_lookup")
	}
	already := status == "dead_banned"
	if !already {
		imported, err := time.Parse(time.RFC3339Nano, importedAt)
		if err != nil {
			return BanResult{}, internalError("account_import_time")
		}
		days := now.Sub(imported).Hours() / 24
		if days < 0 {
			days = 0
		}
		stamp := now.Format(time.RFC3339Nano)
		if _, err := tx.ExecContext(ctx, `UPDATE accounts SET status='dead_banned',dead_at=?,death_type='abnormal_ban',banned_survival_days=?,
			last_check_state='ok',last_check_error_code=NULL,next_retry_at=NULL,polling_paused=1,pause_reason='manual_admin_ban',
			pending_evidence_signature=NULL,pending_detected_at=NULL,updated_at=? WHERE id=?`, stamp, days, stamp, accountID); err != nil {
			return BanResult{}, internalError("account_ban")
		}
		if _, err := tx.ExecContext(ctx, `UPDATE authorization_epochs SET ended_at=?,terminal_status='dead_banned',dead_at=?,banned_survival_days=? WHERE id=? AND ended_at IS NULL`, stamp, stamp, days, epochID); err != nil {
			return BanResult{}, internalError("epoch_close")
		}
		signature := fmt.Sprintf("manual_admin_ban:%d:%d", accountID, now.UnixNano())
		if _, err := tx.ExecContext(ctx, `INSERT INTO status_change_log
			(account_id,epoch_id,at,field,from_value,to_value,evidence_code,evidence_level,evidence_signature,review_decision)
			VALUES (?,?,?,'status',?,'dead_banned','manual_admin_ban','live_verified',?,NULL)`, accountID, epochID, stamp, status, signature); err != nil {
			return BanResult{}, internalError("status_log_insert")
		}
		eventKey := "epoch:" + strconv.FormatInt(epochID, 10) + ":alive_to_dead_banned"
		if _, err := tx.ExecContext(ctx, `INSERT INTO alert_events(account_id,epoch_id,event_key,event_type,delivery_status,created_at,updated_at)
			VALUES (?,?,?,'abnormal_ban','pending',?,?) ON CONFLICT(event_key) DO NOTHING`, accountID, epochID, eventKey, stamp, stamp); err != nil {
			return BanResult{}, internalError("alert_insert")
		}
		if _, err := allocationsync.EnqueueAccountTx(ctx, tx, accountID, accountsync.EventAccountBanned, now); err != nil {
			return BanResult{}, internalError("allocation_sync_enqueue")
		}
	} else {
		var delivery sql.NullString
		_ = tx.QueryRowContext(ctx, `SELECT delivery_status FROM allocation_account_outbox WHERE account_id=? ORDER BY account_version DESC LIMIT 1`, accountID).Scan(&delivery)
		if !delivery.Valid || delivery.String == "failed" {
			if _, err := allocationsync.EnqueueAccountTx(ctx, tx, accountID, accountsync.EventAccountBanned, now); err != nil {
				return BanResult{}, internalError("allocation_sync_enqueue")
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return BanResult{}, internalError("transaction_commit")
	}
	result, err := s.Get(ctx, accountID)
	return BanResult{Account: result, AlreadyBanned: already}, err
}

func (s *Service) RetryAllocationSync(ctx context.Context, accountID int64) (Account, error) {
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Account{}, internalError("transaction_begin")
	}
	defer tx.Rollback()
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM accounts WHERE id=? AND deleted_at IS NULL`, accountID).Scan(&status); err != nil {
		if err == sql.ErrNoRows {
			return Account{}, &ServiceError{Kind: ErrorNotFound, Code: "account_not_found"}
		}
		return Account{}, internalError("account_lookup")
	}
	eventType := accountsync.EventAccountUpdated
	if status == "dead_banned" {
		eventType = accountsync.EventAccountBanned
	}
	if _, err := allocationsync.EnqueueAccountTx(ctx, tx, accountID, eventType, now); err != nil {
		return Account{}, internalError("allocation_sync_enqueue")
	}
	if err := tx.Commit(); err != nil {
		return Account{}, internalError("transaction_commit")
	}
	return s.Get(ctx, accountID)
}
