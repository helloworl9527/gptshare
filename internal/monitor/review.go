package monitor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"chatgpt-monitor/internal/chatgpt"
)

type reviewCandidate struct {
	accountID                  int64
	status, imported, detected string
	epochID                    int64
}

func (s *Service) ReviewEvidence(ctx context.Context, request ReviewRequest) (ReviewResult, error) {
	request.Signature = strings.TrimSpace(request.Signature)
	request.Reason = strings.TrimSpace(request.Reason)
	request.Operator = strings.TrimSpace(request.Operator)
	if !strings.HasPrefix(request.Signature, "ev1:") || len(request.Signature) != 68 || (request.Decision != ReviewConfirm && request.Decision != ReviewReject) || request.Reason == "" || len(request.Reason) > 512 || strings.ContainsAny(request.Reason, "\r\n") || request.Operator == "" || len(request.Operator) > 128 {
		return ReviewResult{}, errors.New("invalid evidence review request")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReviewResult{}, err
	}
	defer tx.Rollback()
	now := s.now().UTC()
	rows, err := tx.QueryContext(ctx, `SELECT a.id,a.status,a.import_time,a.pending_detected_at,e.id
		FROM accounts a JOIN authorization_epochs e ON e.account_id=a.id AND e.ended_at IS NULL
		WHERE a.polling_paused=1 AND a.pending_evidence_signature=? AND a.last_check_state='verification_required'`, request.Signature)
	if err != nil {
		return ReviewResult{}, err
	}
	var candidates []reviewCandidate
	for rows.Next() {
		var item reviewCandidate
		if err := rows.Scan(&item.accountID, &item.status, &item.imported, &item.detected, &item.epochID); err != nil {
			rows.Close()
			return ReviewResult{}, err
		}
		candidates = append(candidates, item)
	}
	if err := rows.Close(); err != nil {
		return ReviewResult{}, err
	}
	if len(candidates) == 0 {
		return ReviewResult{}, &NotFoundError{}
	}
	result := ReviewResult{Signature: request.Signature, Decision: request.Decision, ReviewedAt: now}
	if request.Decision == ReviewConfirm {
		entry, _ := json.Marshal(map[string]any{"level": chatgpt.EvidenceLiveVerified, "decision": "confirmed", "reviewed_at": formatTime(now)})
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value,is_secret,key_id,updated_at) VALUES (?, ?,0,NULL,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, "internal.evidence."+request.Signature, entry, formatTime(now)); err != nil {
			return ReviewResult{}, err
		}
	} else {
		entry, _ := json.Marshal(map[string]any{"level": chatgpt.EvidenceUnverified, "decision": "rejected", "reviewed_at": formatTime(now)})
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key,value,is_secret,key_id,updated_at) VALUES (?, ?,0,NULL,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`, "internal.evidence."+request.Signature, entry, formatTime(now)); err != nil {
			return ReviewResult{}, err
		}
	}
	for _, item := range candidates {
		detected, err := parseTime(item.detected)
		if err != nil {
			return ReviewResult{}, err
		}
		imported, err := parseTime(item.imported)
		if err != nil {
			return ReviewResult{}, err
		}
		if request.Decision == ReviewConfirm {
			days := detected.Sub(imported).Hours() / 24
			if days < 0 {
				days = 0
			}
			if _, err := tx.ExecContext(ctx, `UPDATE accounts SET status='dead_banned',dead_at=?,death_type='abnormal_ban',banned_survival_days=?,last_check_state='ok',last_check_error_code=NULL,
				polling_paused=0,pause_reason=NULL,pending_evidence_signature=NULL,pending_detected_at=NULL,next_retry_at=NULL,updated_at=? WHERE id=?`, formatTime(detected), days, formatTime(now), item.accountID); err != nil {
				return ReviewResult{}, err
			}
			if err := closeEpoch(tx, item.epochID, StateDeadBanned, detected, &days); err != nil {
				return ReviewResult{}, err
			}
			if err := insertReviewedChange(ctx, tx, item, now, request, StateDeadBanned, chatgpt.EvidenceLiveVerified); err != nil {
				return ReviewResult{}, err
			}
			created, err := insertAlert(ctx, tx, item.accountID, item.epochID, detected)
			if err != nil {
				return ReviewResult{}, err
			}
			if created {
				result.AlertsCreated++
			}
		} else {
			if _, err := tx.ExecContext(ctx, `UPDATE accounts SET last_check_state='contract_changed',last_check_error_code='evidence_rejected',pause_reason='contract_changed',updated_at=? WHERE id=?`, formatTime(now), item.accountID); err != nil {
				return ReviewResult{}, err
			}
			if err := insertReviewedChange(ctx, tx, item, now, request, CheckContractChanged, chatgpt.EvidenceContractVerifiedLivePending); err != nil {
				return ReviewResult{}, err
			}
		}
		result.AccountsAffected++
	}
	decision := "confirmed"
	if request.Decision == ReviewReject {
		decision = "rejected"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE status_change_log SET review_decision=?,reviewed_at=?,review_operator=?,review_reason=?
		WHERE evidence_signature=? AND review_decision='pending'`, decision, formatTime(now), request.Operator, request.Reason, request.Signature); err != nil {
		return ReviewResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return ReviewResult{}, err
	}
	return result, nil
}

func insertReviewedChange(ctx context.Context, tx *sql.Tx, item reviewCandidate, now time.Time, request ReviewRequest, to string, level chatgpt.EvidenceLevel) error {
	field := "status"
	from := item.status
	if request.Decision == ReviewReject {
		field, from = "last_check_state", CheckVerificationRequired
	}
	decision := "confirmed"
	if request.Decision == ReviewReject {
		decision = "rejected"
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO status_change_log(account_id,epoch_id,at,field,from_value,to_value,evidence_code,evidence_level,evidence_signature,review_decision,reviewed_at,review_operator,review_reason)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`, item.accountID, item.epochID, formatTime(now), field, from, to, "evidence_review", string(level), request.Signature, decision, formatTime(now), request.Operator, request.Reason)
	return err
}

func PendingSignatures(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT pending_evidence_signature FROM accounts WHERE polling_paused=1 AND last_check_state='verification_required' ORDER BY pending_evidence_signature`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var signatures []string
	for rows.Next() {
		var value sql.NullString
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		if value.Valid {
			signatures = append(signatures, value.String)
		}
	}
	return signatures, rows.Err()
}
