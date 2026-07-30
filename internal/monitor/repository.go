package monitor

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"chatgpt-monitor/internal/chatgpt"
)

func (s *Service) loadDueAccounts(ctx context.Context, onlyID int64) ([]accountRecord, error) {
	query := `SELECT a.id,a.provider_account_id,a.token_type,a.enc_credentials,a.auth_expiry,a.status,a.import_time,
		a.last_check_state,a.polling_paused,e.id
		FROM accounts a JOIN authorization_epochs e ON e.account_id=a.id AND e.ended_at IS NULL
		WHERE a.deleted_at IS NULL`
	args := []any{}
	if onlyID != 0 {
		query += " AND a.id=?"
		args = append(args, onlyID)
	} else {
		query += " AND a.polling_paused=0 AND (a.next_retry_at IS NULL OR a.next_retry_at<=?)"
		args = append(args, formatTime(s.now().UTC()))
	}
	query += " ORDER BY a.id"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []accountRecord
	for rows.Next() {
		var record accountRecord
		var authExpiry, imported string
		if err := rows.Scan(&record.ID, &record.ProviderID, &record.TokenType, &record.Envelope, &authExpiry, &record.Status, &imported, &record.LastCheck, &record.Paused, &record.EpochID); err != nil {
			return nil, err
		}
		record.AuthExpiry, err = parseTime(authExpiry)
		if err != nil {
			return nil, err
		}
		record.ImportTime, err = parseTime(imported)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (s *Service) createRun(ctx context.Context, trigger string, accountID *int64, total int) (Run, error) {
	id, err := randomID()
	if err != nil {
		return Run{}, err
	}
	now := s.now().UTC()
	_, err = s.db.ExecContext(ctx, `INSERT INTO poll_runs(id,started_at,state,accounts_total,trigger_type,account_id,error_counts_json)
		VALUES (?,?,'running',?,?,?,'{}')`, id, formatTime(now), total, trigger, accountID)
	if err != nil {
		return Run{}, err
	}
	return Run{ID: id, State: "running", Trigger: trigger, AccountID: accountID, StartedAt: now, AccountsTotal: total, ErrorCounts: map[string]int{}}, nil
}

func (s *Service) recordSkipped(ctx context.Context) (Run, error) {
	run, err := s.createRun(ctx, "scheduled", nil, 0)
	if err != nil {
		return Run{}, err
	}
	now := s.now().UTC()
	_, err = s.db.ExecContext(ctx, `UPDATE poll_runs SET state='completed',finished_at=?,accounts_skipped=0,error_code='round_overlap_skipped' WHERE id=?`, formatTime(now), run.ID)
	if err != nil {
		return Run{}, err
	}
	return s.GetRun(ctx, run.ID)
}

func (s *Service) GetRun(ctx context.Context, runID string) (Run, error) {
	var run Run
	var started, finished string
	var finishedNullable sql.NullString
	var accountID sql.NullInt64
	var errorsJSON string
	err := s.db.QueryRowContext(ctx, `SELECT id,state,trigger_type,account_id,started_at,finished_at,accounts_total,accounts_ok,accounts_failed,accounts_skipped,error_counts_json,COALESCE(error_code,'')
		FROM poll_runs WHERE id=?`, runID).Scan(&run.ID, &run.State, &run.Trigger, &accountID, &started, &finishedNullable, &run.AccountsTotal, &run.AccountsOK, &run.AccountsFailed, &run.AccountsSkipped, &errorsJSON, &run.ErrorCode)
	if errors.Is(err, sql.ErrNoRows) {
		return Run{}, &NotFoundError{}
	}
	if err != nil {
		return Run{}, err
	}
	run.StartedAt, err = parseTime(started)
	if err != nil {
		return Run{}, err
	}
	if finishedNullable.Valid {
		finished = finishedNullable.String
		parsed, parseErr := parseTime(finished)
		if parseErr != nil {
			return Run{}, parseErr
		}
		run.FinishedAt = &parsed
	}
	if accountID.Valid {
		run.AccountID = pointer(accountID.Int64)
	}
	if json.Unmarshal([]byte(errorsJSON), &run.ErrorCounts) != nil {
		run.ErrorCounts = map[string]int{"invalid_error_counts": 1}
	}
	if run.ErrorCode == "round_overlap_skipped" {
		run.State = "skipped"
	}
	if run.ErrorCode == "startup_interrupted" {
		run.State = "interrupted"
	}
	return run, nil
}

func (s *Service) applyResult(ctx context.Context, runID string, record accountRecord, outcome pollResult, interval time.Duration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now().UTC()
	var currentStatus, currentPlan, currentRawPlan, currentExpiry, lastCheck, currentLabel string
	var currentExpiryNull, currentEmail sql.NullString
	var paused bool
	if err := tx.QueryRowContext(ctx, `SELECT status,plan,raw_plan,current_expiry,last_check_state,polling_paused,email,label FROM accounts WHERE id=? AND deleted_at IS NULL`, record.ID).
		Scan(&currentStatus, &currentPlan, &currentRawPlan, &currentExpiryNull, &lastCheck, &paused, &currentEmail, &currentLabel); err != nil {
		return err
	}
	if paused && outcome.code != "normal_expiry" {
		return nil
	}
	if currentExpiryNull.Valid {
		currentExpiry = currentExpiryNull.String
	}

	level := chatgpt.EvidenceLiveVerified
	code := outcome.code
	signature := EvidenceSignatureFor(outcome.endpoint, code, s.cfg.ParserVersion)
	newStatus, newPlan, newRawPlan, newExpiry, newCheck := currentStatus, currentPlan, currentRawPlan, currentExpiry, lastCheck
	var deadAt any
	var deathType any
	var survival any
	var nextRetry any
	var pauseReason any
	var pendingSignature any
	var pendingDetected any
	pollingPaused := 0
	reviewDecision := any(nil)

	if outcome.status != nil && code == "normal_expiry" {
		newStatus, newCheck = StateDeadNormal, CheckOK
		deadAt, deathType = formatTime(record.AuthExpiry), "normal_expiry"
		if err := closeEpoch(tx, record.EpochID, StateDeadNormal, record.AuthExpiry, nil); err != nil {
			return err
		}
	} else if outcome.typed == nil && outcome.status != nil {
		level = outcome.status.EvidenceLevel
		if level == chatgpt.EvidenceLiveVerified {
			newCheck = CheckOK
			if currentStatus != StateDeadBanned {
				newStatus = StateAlive
			}
			newPlan = string(outcome.status.Plan)
			newRawPlan = outcome.status.RawPlan
			newExpiry = ""
			if outcome.status.SubscriptionExpiry != nil {
				newExpiry = formatTime(*outcome.status.SubscriptionExpiry)
			}
		} else {
			if level != chatgpt.EvidenceContractVerifiedLivePending {
				level = chatgpt.EvidenceUnverified
			}
			newCheck = CheckContractChanged
			pollingPaused = 1
			pauseReason = "unverified_active_result"
			pendingSignature = signature
			pendingDetected = formatTime(now)
		}
	} else {
		level = outcome.typed.EvidenceLevel
		candidate := isStableBanCandidate(outcome.typed)
		registryLevel := s.registryLevel(ctx, tx, signature)
		if candidate && registryLevel != chatgpt.EvidenceUnverified {
			newStatus, newCheck = StateDeadBanned, CheckOK
			deadAt, deathType = formatTime(now), "abnormal_ban"
			days := now.Sub(record.ImportTime).Hours() / 24
			if days < 0 {
				days = 0
			}
			survival = days
			if err := closeEpoch(tx, record.EpochID, StateDeadBanned, now, &days); err != nil {
				return err
			}
		} else if candidate {
			newCheck, pollingPaused = CheckContractChanged, 1
			pauseReason, pendingSignature, pendingDetected = "evidence_signature_rejected", signature, formatTime(now)
		} else if level == chatgpt.EvidenceUnverified || outcome.typed.Kind == chatgpt.ErrorContractChanged {
			newCheck, pollingPaused = CheckContractChanged, 1
			pauseReason, pendingSignature, pendingDetected = "contract_changed", signature, formatTime(now)
		} else {
			newCheck = CheckError
			nextRetry = formatTime(now.Add(s.retryDelay(outcome.typed)))
		}
	}

	if len(outcome.envelope) > 0 {
		if _, err := tx.ExecContext(ctx, "UPDATE accounts SET enc_credentials=?,credential_key_id=? WHERE id=?", outcome.envelope, s.cipher.ActiveKeyID(), record.ID); err != nil {
			return err
		}
	}
	nextDue := nextRetry
	if nextDue == nil && newCheck == CheckOK && newStatus == StateAlive {
		nextDue = formatTime(now.Add(interval + s.jitter(record.ID, interval)))
	}
	fillEmail, fillLabel := fillOnlyEmailProjection(currentEmail, currentLabel, record.ProviderID, outcome)
	if _, err := tx.ExecContext(ctx, `UPDATE accounts SET plan=?,raw_plan=?,current_expiry=?,status=?,last_alive_at=CASE WHEN ?='alive' AND ?='ok' THEN ? ELSE last_alive_at END,
		dead_at=?,death_type=?,banned_survival_days=?,last_check_state=?,last_check_error_code=?,next_retry_at=?,polling_paused=?,pause_reason=?,pending_evidence_signature=?,pending_detected_at=?,updated_at=? WHERE id=?`,
		newPlan, newRawPlan, nullable(newExpiry), newStatus, newStatus, newCheck, formatTime(now), deadAt, deathType, survival, newCheck, nullableError(newCheck, code), nextDue, pollingPaused, pauseReason, pendingSignature, pendingDetected, formatTime(now), record.ID); err != nil {
		return err
	}
	if fillEmail != nil || fillLabel != nil {
		if _, err := tx.ExecContext(ctx, `UPDATE accounts SET
			email=CASE WHEN ? IS NOT NULL AND (email IS NULL OR email='') THEN ? ELSE email END,
			label=CASE WHEN ? IS NOT NULL AND (label='' OR label=provider_account_id) THEN ? ELSE label END,
			updated_at=?
			WHERE id=? AND deleted_at IS NULL`,
			fillEmail, fillEmail, fillLabel, fillLabel, formatTime(now), record.ID); err != nil {
			return err
		}
	}
	changes := []struct{ field, from, to string }{{"status", currentStatus, newStatus}, {"plan", currentPlan, newPlan}, {"current_expiry", currentExpiry, newExpiry}, {"last_check_state", lastCheck, newCheck}}
	for _, change := range changes {
		if change.from == change.to {
			continue
		}
		if err := insertChange(ctx, tx, record.ID, record.EpochID, now, change.field, change.from, change.to, code, level, signature, reviewDecision, runID); err != nil {
			return err
		}
	}
	if currentStatus != StateDeadBanned && newStatus == StateDeadBanned {
		if _, err := insertAlert(ctx, tx, record.ID, record.EpochID, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func isStableBanCandidate(typed *chatgpt.TypedError) bool {
	if typed == nil || !typed.BannedCandidate {
		return false
	}
	switch typed.EvidenceCode {
	case "account_disabled", "account_deactivated":
		return typed.Kind == chatgpt.ErrorAccountDisabled
	case "token_revoked", "credential_revoked", "refresh_token_reused":
		return typed.Kind == chatgpt.ErrorCredentialRevoked
	default:
		return false
	}
}

func closeEpoch(tx *sql.Tx, epochID int64, terminal string, deadAt time.Time, survival *float64) error {
	_, err := tx.Exec(`UPDATE authorization_epochs SET ended_at=?,terminal_status=?,dead_at=?,banned_survival_days=? WHERE id=? AND ended_at IS NULL`, formatTime(deadAt), terminal, formatTime(deadAt), survival, epochID)
	return err
}

func insertChange(ctx context.Context, tx *sql.Tx, accountID, epochID int64, at time.Time, field, from, to, code string, level chatgpt.EvidenceLevel, signature string, review any, runID string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO status_change_log(account_id,epoch_id,at,field,from_value,to_value,evidence_code,evidence_level,evidence_signature,review_decision,run_id)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`, accountID, epochID, formatTime(at), field, nullable(from), nullable(to), code, string(level), signature, review, nullable(runID))
	return err
}

func insertAlert(ctx context.Context, tx *sql.Tx, accountID, epochID int64, at time.Time) (bool, error) {
	eventKey := "epoch:" + strconv.FormatInt(epochID, 10) + ":alive_to_dead_banned"
	result, err := tx.ExecContext(ctx, `INSERT INTO alert_events(account_id,epoch_id,event_key,event_type,delivery_status,created_at,updated_at)
		VALUES (?,?,?,'abnormal_ban','pending',?,?) ON CONFLICT(event_key) DO NOTHING`, accountID, epochID, eventKey, formatTime(at), formatTime(at))
	if err != nil {
		return false, err
	}
	count, _ := result.RowsAffected()
	return count == 1, nil
}

func (s *Service) registryLevel(ctx context.Context, tx *sql.Tx, signature string) chatgpt.EvidenceLevel {
	var value []byte
	if tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=?", "internal.evidence."+signature).Scan(&value) != nil {
		return chatgpt.EvidenceContractVerifiedLivePending
	}
	var entry struct {
		Level chatgpt.EvidenceLevel `json:"level"`
	}
	if json.Unmarshal(value, &entry) != nil {
		return chatgpt.EvidenceUnverified
	}
	return entry.Level
}

func (s *Service) currentPollInterval(ctx context.Context) time.Duration {
	var value []byte
	if s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key='poll_interval'").Scan(&value) == nil {
		seconds, err := strconv.Atoi(string(value))
		if err == nil && time.Duration(seconds)*time.Second >= s.cfg.MinInterval {
			return time.Duration(seconds) * time.Second
		}
	}
	return s.cfg.DefaultInterval
}

func (s *Service) jitter(accountID int64, interval time.Duration) time.Duration {
	sum := sha256Bytes(fmt.Sprintf("%d", accountID))
	portion := float64(sum[0]) / 255 * .10
	return time.Duration(float64(interval) * portion)
}

func sha256Bytes(value string) [32]byte { return sha256.Sum256([]byte(value)) }

func (s *Service) retryDelay(typed *chatgpt.TypedError) time.Duration {
	delay := s.cfg.MinInterval
	if typed != nil && typed.RetryAfter > delay {
		delay = typed.RetryAfter
	}
	return delay
}

func nullableError(check, code string) any {
	if check == CheckOK {
		return nil
	}
	return code
}

func fillOnlyEmailProjection(currentEmail sql.NullString, currentLabel, providerID string, outcome pollResult) (any, any) {
	if outcome.typed != nil || outcome.status == nil || outcome.status.EvidenceLevel != chatgpt.EvidenceLiveVerified || outcome.status.Email == "" {
		return nil, nil
	}
	if currentEmail.Valid && currentEmail.String != "" && currentEmail.String != outcome.status.Email {
		return nil, nil
	}
	var email any
	if !currentEmail.Valid || currentEmail.String == "" {
		email = outcome.status.Email
	}
	var label any
	if currentLabel == "" || currentLabel == providerID {
		label = outcome.status.Email
	}
	return email, label
}
