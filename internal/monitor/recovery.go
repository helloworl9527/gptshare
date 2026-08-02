package monitor

import (
	"context"

	"chatgpt-monitor/internal/chatgpt"
)

const (
	supplementalRecoveryCode     = "supplemental_denial_reclassified"
	supplementalRecoveryOperator = "startup_recovery"
	supplementalRecoveryReason   = "accounts_check generic denial reclassified as supplemental"
)

// RecoverMisclassifiedSupplementalDenials repairs accounts paused before
// accounts/check non-JSON 401/403 responses were classified by status code.
func (s *Service) RecoverMisclassifiedSupplementalDenials(ctx context.Context) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	signature := EvidenceSignatureFor("accounts_check", "unexpected_non_json", "status-v1")
	recoverySignature := EvidenceSignatureFor("startup_recovery", supplementalRecoveryCode, "status-v1")
	rows, err := tx.QueryContext(ctx, `SELECT a.id,e.id
		FROM accounts a
		JOIN authorization_epochs e ON e.account_id=a.id AND e.ended_at IS NULL
		WHERE a.deleted_at IS NULL
		  AND a.status='alive'
		  AND a.token_type IN ('refresh','device')
		  AND a.last_check_state='contract_changed'
		  AND a.last_check_error_code='unexpected_non_json'
		  AND a.polling_paused=1
		  AND a.pending_evidence_signature=?
		ORDER BY a.id`, signature)
	if err != nil {
		return 0, err
	}
	type candidate struct {
		accountID int64
		epochID   int64
	}
	var candidates []candidate
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.accountID, &item.epochID); err != nil {
			rows.Close()
			return 0, err
		}
		candidates = append(candidates, item)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	now := s.now().UTC()
	recovered := 0
	for _, item := range candidates {
		result, err := tx.ExecContext(ctx, `UPDATE accounts SET
			last_check_state='ok',last_check_error_code=NULL,next_retry_at=NULL,
			polling_paused=0,pause_reason=NULL,pending_evidence_signature=NULL,pending_detected_at=NULL,updated_at=?
			WHERE id=? AND deleted_at IS NULL
			  AND status='alive' AND token_type IN ('refresh','device')
			  AND last_check_state='contract_changed' AND last_check_error_code='unexpected_non_json'
			  AND polling_paused=1 AND pending_evidence_signature=?`, formatTime(now), item.accountID, signature)
		if err != nil {
			return 0, err
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return 0, err
		}
		if updated != 1 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE status_change_log SET
			review_decision='rejected',reviewed_at=?,review_operator=?,review_reason=?
			WHERE account_id=? AND evidence_signature=? AND reviewed_at IS NULL
			  AND (review_decision IS NULL OR review_decision='pending')`,
			formatTime(now), supplementalRecoveryOperator, supplementalRecoveryReason, item.accountID, signature); err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO status_change_log(
			account_id,epoch_id,at,field,from_value,to_value,evidence_code,evidence_level,evidence_signature)
			VALUES (?,?,?,?,?,?,?,?,?)`,
			item.accountID, item.epochID, formatTime(now), "last_check_state", CheckContractChanged, CheckOK,
			supplementalRecoveryCode, string(chatgpt.EvidenceContractVerifiedLivePending), recoverySignature); err != nil {
			return 0, err
		}
		recovered++
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return recovered, nil
}
