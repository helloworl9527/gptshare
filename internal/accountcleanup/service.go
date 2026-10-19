// Package accountcleanup removes expired accounts from the monitor list, not
// from history. The pool guard must run before encrypted credentials are erased.
package accountcleanup

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"log/slog"
	"time"
)

const (
	Interval = 7 * 24 * time.Hour
	jobName  = "expired-account-cleanup"
	// Never confuse credential auth_expiry with subscription current_expiry.
	expiredClause = `((current_expiry IS NOT NULL AND julianday(current_expiry)<=julianday(?)) OR (current_expiry IS NULL AND status='dead_normal'))`
)

type AllocationGuard interface {
	PrepareExpiredAccountCleanup(context.Context, string, time.Time) (bool, error)
}

type Service struct {
	db     *sql.DB
	guard  AllocationGuard
	logger *slog.Logger
	now    func() time.Time
}

type Result struct {
	Ran     bool
	Removed int
	Skipped int
}

func New(db *sql.DB, guard AllocationGuard, logger *slog.Logger) (*Service, error) {
	if db == nil || guard == nil {
		return nil, errors.New("account_cleanup_dependencies_missing")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{db: db, guard: guard, logger: logger, now: time.Now}, nil
}

// Run polls only the durable schedule; it does not clean on every startup or
// every tick. Missed runs are caught up after restart, failed runs retry hourly.
func (s *Service) Run(ctx context.Context) error {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		result, err := s.RunDue(ctx)
		if err != nil {
			if ctx.Err() == nil {
				s.logger.Error("expired account cleanup failed", "task", jobName, "error_code", "account_cleanup_failed")
			}
		} else if result.Ran {
			s.logger.Info("expired account cleanup completed", "task", jobName, "removed", result.Removed, "skipped", result.Skipped)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Service) RunDue(ctx context.Context) (Result, error) {
	now := s.now().UTC()
	stamp := now.Format(time.RFC3339Nano)
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO maintenance_jobs(name,next_run_at) VALUES (?,?)`, jobName, now.Add(Interval).Format(time.RFC3339Nano)); err != nil {
		return Result{}, err
	}
	token := rand.Text()
	claim, err := s.db.ExecContext(ctx, `UPDATE maintenance_jobs SET lease_token=?,lease_until=?
		WHERE name=? AND julianday(next_run_at)<=julianday(?)
		AND (lease_until IS NULL OR julianday(lease_until)<=julianday(?))`, token, now.Add(30*time.Minute).Format(time.RFC3339Nano), jobName, stamp, stamp)
	if err != nil {
		return Result{}, err
	}
	affected, err := claim.RowsAffected()
	if err != nil || affected != 1 {
		return Result{}, err
	}
	result := Result{Ran: true}
	runCtx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	defer func() {
		// On failure/cancellation release the lease without postponing next_run_at.
		// On success the token is already cleared, so this becomes a no-op.
		releaseCtx, releaseCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer releaseCancel()
		_, _ = s.db.ExecContext(releaseCtx, `UPDATE maintenance_jobs SET lease_token=NULL,lease_until=NULL,last_error_code='account_cleanup_failed' WHERE name=? AND lease_token=?`, jobName, token)
	}()
	rows, err := s.db.QueryContext(runCtx, `SELECT id FROM accounts WHERE deleted_at IS NULL AND `+expiredClause+` ORDER BY id`, stamp)
	if err != nil {
		return result, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return result, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	for _, id := range ids {
		removed, err := s.remove(runCtx, id, now)
		if err != nil {
			return result, err
		}
		if removed {
			result.Removed++
		} else {
			result.Skipped++
		}
	}
	finished, err := s.db.ExecContext(runCtx, `UPDATE maintenance_jobs SET last_run_at=?,next_run_at=?,
		lease_token=NULL,lease_until=NULL,last_error_code=NULL,last_removed_count=?,last_skipped_count=?
		WHERE name=? AND lease_token=?`, stamp, now.Add(Interval).Format(time.RFC3339Nano), result.Removed, result.Skipped, jobName, token)
	if err != nil {
		return result, err
	}
	if n, err := finished.RowsAffected(); err != nil || n != 1 {
		return result, errors.New("account_cleanup_lease_lost")
	}
	return result, nil
}

func (s *Service) remove(ctx context.Context, id int64, now time.Time) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	stamp := now.Format(time.RFC3339Nano)
	// Acquire the monitor write lock before rechecking eligibility. Reauthorization
	// and event producers cannot race between this check and soft deletion.
	locked, err := tx.ExecContext(ctx, `UPDATE accounts SET id=id WHERE id=? AND deleted_at IS NULL AND `+expiredClause, id, stamp)
	if err != nil {
		return false, err
	}
	if n, err := locked.RowsAffected(); err != nil || n != 1 {
		return false, err
	}
	var providerID string
	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT provider_account_id,
		(SELECT count(*) FROM allocation_account_outbox WHERE account_id=accounts.id AND delivery_status!='delivered')
		FROM accounts WHERE id=?`, id).Scan(&providerID, &pending); err != nil {
		return false, err
	}
	if pending != 0 {
		return false, nil
	}
	safe, err := s.guard.PrepareExpiredAccountCleanup(ctx, providerID, now)
	if err != nil || !safe {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE accounts SET deleted_at=?,enc_credentials=x'',credential_key_id='',
		credential_generation=credential_generation+1,updated_at=? WHERE id=?`, stamp, stamp, id); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE authorization_epochs SET ended_at=?,
		terminal_status=COALESCE(terminal_status,CASE WHEN (SELECT status FROM accounts WHERE id=?)='dead_banned' THEN 'dead_banned' ELSE 'dead_normal' END)
		WHERE account_id=? AND ended_at IS NULL`, stamp, id, id); err != nil {
		return false, err
	}
	for _, table := range []string{"device_auth_sessions", "oauth_auth_sessions"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE account_id=?", id); err != nil {
			return false, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO account_cleanup_log(account_id,removed_at,reason) VALUES (?,?,'expired_weekly_cleanup')`, id, stamp); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}
