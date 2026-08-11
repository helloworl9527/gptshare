package account

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"chatgpt-monitor/internal/chatgpt"
	credentialcrypto "chatgpt-monitor/internal/crypto"
)

const (
	maxDevicePolls          = 180
	deviceWorkflowTimeout   = 9 * time.Second
	deviceReservationWindow = 15 * time.Second
)

type DeviceClient interface {
	StartDeviceAuthorization(context.Context) (chatgpt.DeviceAuthorization, error)
	PollDeviceAuthorizationResult(context.Context, chatgpt.DeviceAuthorization) (chatgpt.DevicePollResult, error)
}

type DeviceStart struct {
	SessionID       string    `json:"session_id"`
	UserCode        string    `json:"user_code"`
	VerifyURL       string    `json:"verify_url"`
	IntervalSeconds int       `json:"interval_seconds"`
	ExpiresAt       time.Time `json:"expires_at"`
	State           string    `json:"state"`
}

type DevicePoll struct {
	SessionID       string   `json:"session_id"`
	State           string   `json:"state"`
	RetryAfter      int      `json:"retry_after_seconds,omitempty"`
	RestartRequired bool     `json:"restart_required,omitempty"`
	Account         *Account `json:"account,omitempty"`
}

type devicePayload struct {
	Authorization chatgpt.DeviceAuthorization `json:"authorization"`
	Label         string                      `json:"label,omitempty"`
	Reauthorize   bool                        `json:"reauthorize,omitempty"`
	PollCount     int                         `json:"poll_count"`
	Tokens        chatgpt.TokenSet            `json:"tokens,omitempty"`
}

type deviceRecord struct {
	id         string
	accountID  sql.NullInt64
	envelope   []byte
	interval   int
	expiresAt  time.Time
	state      string
	updatedAt  time.Time
	updatedRaw string
}

func (s *Service) StartDeviceImport(ctx context.Context, label string) (DeviceStart, error) {
	return s.startDevice(ctx, label, nil)
}

func (s *Service) StartDeviceReauthorization(ctx context.Context, accountID int64) (DeviceStart, error) {
	if accountID <= 0 {
		return DeviceStart{}, &ServiceError{Kind: ErrorInvalid, Code: "account_id_invalid"}
	}
	if _, err := s.Get(ctx, accountID); err != nil {
		return DeviceStart{}, err
	}
	return s.startDevice(ctx, "", &accountID)
}

func (s *Service) startDevice(ctx context.Context, label string, accountID *int64) (DeviceStart, error) {
	label = strings.TrimSpace(label)
	if len(label) > maxLabelLength {
		return DeviceStart{}, &ServiceError{Kind: ErrorInvalid, Code: "label_too_large"}
	}
	if s.device == nil {
		return DeviceStart{}, internalError("device_client_unavailable")
	}
	if err := s.cleanupExpiredDeviceSessions(ctx); err != nil {
		return DeviceStart{}, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, deviceWorkflowTimeout)
	defer cancel()
	authorization, err := s.device.StartDeviceAuthorization(requestCtx)
	if err != nil {
		return DeviceStart{}, classifyUpstream(err)
	}
	now := s.now().UTC()
	if authorization.DeviceAuthID == "" || authorization.UserCode == "" || authorization.VerifyURL == "" || authorization.Interval < time.Second || !authorization.ExpiresAt.After(now) {
		return DeviceStart{}, &ServiceError{Kind: ErrorInvalid, Code: "device_start_incomplete"}
	}
	sessionID, err := randomDeviceSessionID()
	if err != nil {
		return DeviceStart{}, internalError("device_session_id")
	}
	payload := devicePayload{Authorization: authorization, Label: label, Reauthorize: accountID != nil}
	plaintext, err := json.Marshal(payload)
	if err != nil {
		return DeviceStart{}, internalError("device_session_encode")
	}
	defer zero(plaintext)
	envelope, err := s.cipher.Seal(plaintext, credentialcrypto.DeviceSessionAAD(sessionID))
	if err != nil {
		return DeviceStart{}, internalError("device_session_encrypt")
	}
	var target any
	if accountID != nil {
		target = *accountID
	}
	stamp := now.Format(time.RFC3339Nano)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO device_auth_sessions
		(id,account_id,enc_device_code,credential_key_id,interval_seconds,expires_at,state,created_at,updated_at)
		VALUES (?,?,?,?,?,?,'pending',?,?)`, sessionID, target, envelope, s.cipher.ActiveKeyID(), int(authorization.Interval/time.Second), authorization.ExpiresAt.UTC().Format(time.RFC3339Nano), stamp, stamp); err != nil {
		return DeviceStart{}, internalError("device_session_store")
	}
	return DeviceStart{SessionID: sessionID, UserCode: authorization.UserCode, VerifyURL: authorization.VerifyURL, IntervalSeconds: int(authorization.Interval / time.Second), ExpiresAt: authorization.ExpiresAt.UTC(), State: "pending"}, nil
}

func (s *Service) PollDevice(ctx context.Context, sessionID string) (DevicePoll, error) {
	requestCtx, cancel := context.WithTimeout(ctx, deviceWorkflowTimeout)
	defer cancel()
	ctx = requestCtx
	if err := s.cleanupExpiredDeviceSessions(ctx); err != nil {
		return DevicePoll{}, err
	}
	if len(sessionID) < 32 || len(sessionID) > 128 {
		return DevicePoll{}, &ServiceError{Kind: ErrorInvalid, Code: "device_session_invalid"}
	}
	record, err := s.loadDevice(ctx, sessionID)
	if err != nil {
		return DevicePoll{}, err
	}
	if record.state == "authorized" {
		if !record.accountID.Valid {
			return DevicePoll{}, internalError("device_session_result_missing")
		}
		result, err := s.Get(ctx, record.accountID.Int64)
		if err != nil {
			return DevicePoll{}, err
		}
		return DevicePoll{SessionID: sessionID, State: "authorized", Account: &result}, nil
	}
	if record.state == "expired" {
		return DevicePoll{SessionID: sessionID, State: "expired", RestartRequired: true}, nil
	}
	if record.state == "failed" {
		return DevicePoll{}, &ServiceError{Kind: ErrorInvalid, Code: "device_authorization_failed"}
	}
	now := s.now().UTC()
	if !now.Before(record.expiresAt) {
		if err := s.finishDevice(ctx, sessionID, "expired", nil); err != nil {
			return DevicePoll{}, err
		}
		return DevicePoll{SessionID: sessionID, State: "expired", RestartRequired: true}, nil
	}
	nextPoll := record.updatedAt.Add(time.Duration(record.interval) * time.Second)
	if now.Before(nextPoll) {
		return DevicePoll{SessionID: sessionID, State: "pending", RetryAfter: ceilSeconds(nextPoll.Sub(now))}, nil
	}
	reserved := now.Add(deviceReservationWindow).Format(time.RFC3339Nano)
	result, err := s.db.ExecContext(ctx, `UPDATE device_auth_sessions SET updated_at=?
		WHERE id=? AND state='pending' AND updated_at=?`, reserved, sessionID, record.updatedRaw)
	if err != nil {
		return DevicePoll{}, internalError("device_poll_reserve")
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return DevicePoll{SessionID: sessionID, State: "pending", RetryAfter: record.interval}, nil
	}
	plaintext, err := s.cipher.Open(record.envelope, credentialcrypto.DeviceSessionAAD(sessionID))
	if err != nil {
		_ = s.finishDevice(ctx, sessionID, "failed", nil)
		return DevicePoll{}, internalError("device_session_decrypt")
	}
	defer zero(plaintext)
	var payload devicePayload
	if json.Unmarshal(plaintext, &payload) != nil || payload.Authorization.DeviceAuthID == "" || payload.Authorization.UserCode == "" {
		_ = s.finishDevice(ctx, sessionID, "failed", nil)
		return DevicePoll{}, internalError("device_session_decode")
	}
	if payload.Tokens.AccessToken == "" {
		payload.PollCount++
		if payload.PollCount > maxDevicePolls {
			_ = s.finishDevice(ctx, sessionID, "expired", nil)
			return DevicePoll{SessionID: sessionID, State: "expired", RestartRequired: true}, nil
		}
		pollResult, pollErr := s.device.PollDeviceAuthorizationResult(ctx, payload.Authorization)
		if pollErr != nil {
			if updateErr := s.updatePendingDevice(ctx, sessionID, payload, record.interval, now); updateErr != nil {
				return DevicePoll{}, updateErr
			}
			return DevicePoll{}, classifyUpstream(pollErr)
		}
		if pollResult.State == chatgpt.DevicePollAuthorized {
			payload.Tokens = pollResult.Tokens
		} else {
			interval := record.interval
			state := "pending"
			if pollResult.State == chatgpt.DevicePollSlowDown {
				state = "slow_down"
				interval += 5
			}
			if suggested := int(pollResult.RetryAfter / time.Second); suggested > interval {
				interval = suggested
			}
			payload.Authorization.Interval = time.Duration(interval) * time.Second
			if err := s.updatePendingDevice(ctx, sessionID, payload, interval, now); err != nil {
				return DevicePoll{}, err
			}
			return DevicePoll{SessionID: sessionID, State: state, RetryAfter: interval}, nil
		}
	}
	prepared, err := s.prepareDeviceTokens(ctx, payload.Tokens)
	if err != nil {
		var serviceErr *ServiceError
		if errors.As(err, &serviceErr) && serviceErr.Kind == ErrorUnavailable {
			if updateErr := s.updatePendingDevice(ctx, sessionID, payload, record.interval, now); updateErr != nil {
				return DevicePoll{}, updateErr
			}
			return DevicePoll{}, err
		}
		_ = s.finishDevice(ctx, sessionID, "failed", nil)
		return DevicePoll{}, err
	}
	defer zero(prepared.plaintext)
	accountID, err := s.completeDevice(ctx, record, payload, prepared, now)
	if err != nil {
		_ = s.finishDevice(ctx, sessionID, "failed", nil)
		return DevicePoll{}, err
	}
	accountResult, err := s.Get(ctx, accountID)
	if err != nil {
		return DevicePoll{}, err
	}
	return DevicePoll{SessionID: sessionID, State: "authorized", Account: &accountResult}, nil
}

func (s *Service) prepareDeviceTokens(ctx context.Context, tokens chatgpt.TokenSet) (preparedImport, error) {
	if tokens.AccessToken == "" {
		return preparedImport{}, &ServiceError{Kind: ErrorInvalid, Code: "device_access_token_missing"}
	}
	status, err := s.client.FetchStatus(ctx, tokens.AccessToken)
	if err != nil {
		return preparedImport{}, classifyUpstream(err)
	}
	if err := validateCredentialStatus(status, s.now().UTC()); err != nil {
		return preparedImport{}, err
	}
	if email := chatgpt.ExtractEmail(tokens.AccessToken, tokens.IDToken, status.ProviderAccountID); email != "" {
		status.Email = email
	}
	plaintext, err := json.Marshal(newCredentialPayload(tokens, "", "device"))
	tokens = chatgpt.TokenSet{}
	if err != nil {
		return preparedImport{}, internalError("credential_encode")
	}
	return preparedImport{kind: chatgpt.CredentialDevice, status: status, plaintext: plaintext}, nil
}

func (s *Service) completeDevice(ctx context.Context, record deviceRecord, payload devicePayload, prepared preparedImport, now time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, internalError("transaction_begin")
	}
	defer tx.Rollback()
	var state string
	if err := tx.QueryRowContext(ctx, "SELECT state FROM device_auth_sessions WHERE id=?", record.id).Scan(&state); err != nil || state != "pending" {
		return 0, internalError("device_session_state")
	}
	var accountID int64
	if payload.Reauthorize {
		if !record.accountID.Valid {
			return 0, internalError("device_reauthorization_target")
		}
		accountID = record.accountID.Int64
		if err := s.reauthorizeDeviceTx(ctx, tx, accountID, prepared, now); err != nil {
			return 0, err
		}
	} else {
		accountID, err = s.importDeviceTx(ctx, tx, payload.Label, prepared, now)
		if err != nil {
			return 0, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE device_auth_sessions SET account_id=?,enc_device_code=x'',credential_key_id='',state='authorized',updated_at=? WHERE id=?`, accountID, now.Format(time.RFC3339Nano), record.id); err != nil {
		return 0, internalError("device_session_complete")
	}
	if err := tx.Commit(); err != nil {
		return 0, internalError("transaction_commit")
	}
	return accountID, nil
}

func (s *Service) importDeviceTx(ctx context.Context, tx *sql.Tx, label string, prepared preparedImport, now time.Time) (int64, error) {
	var existing int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM accounts WHERE provider_account_id=? AND deleted_at IS NULL", prepared.status.ProviderAccountID).Scan(&existing); err != nil {
		return 0, internalError("duplicate_check")
	}
	if existing != 0 {
		return 0, &ServiceError{Kind: ErrorDuplicate, Code: "provider_account_exists"}
	}
	expiry := prepared.status.SubscriptionExpiry.UTC()
	stamp := now.Format(time.RFC3339Nano)
	label = defaultLabel(label, prepared.status)
	result, err := tx.ExecContext(ctx, `INSERT INTO accounts
		(provider_account_id,email,label,token_type,enc_credentials,credential_key_id,plan,raw_plan,current_expiry,auth_expiry,status,last_alive_at,import_time,last_check_state,last_check_error_code,updated_at)
		VALUES (?,?,?,?,x'',?,?,?,?,?,'alive',?,?,'ok',NULL,?)`, prepared.status.ProviderAccountID, nullable(prepared.status.Email), label, string(prepared.kind), s.cipher.ActiveKeyID(), string(prepared.status.Plan), prepared.status.RawPlan,
		expiry.Format(time.RFC3339Nano), expiry.Format(time.RFC3339Nano), stamp, stamp, stamp)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return 0, &ServiceError{Kind: ErrorDuplicate, Code: "provider_account_exists"}
		}
		return 0, internalError("account_insert")
	}
	accountID, err := result.LastInsertId()
	if err != nil {
		return 0, internalError("account_id")
	}
	envelope, err := s.cipher.Seal(prepared.plaintext, credentialcrypto.CredentialAAD(accountID, string(prepared.kind)))
	if err != nil {
		return 0, internalError("credential_encrypt")
	}
	if _, err := tx.ExecContext(ctx, "UPDATE accounts SET enc_credentials=? WHERE id=?", envelope, accountID); err != nil {
		return 0, internalError("credential_store")
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO authorization_epochs(account_id,started_at,auth_expiry) VALUES (?,?,?)", accountID, stamp, expiry.Format(time.RFC3339Nano)); err != nil {
		return 0, internalError("epoch_insert")
	}
	return accountID, nil
}

func (s *Service) reauthorizeDeviceTx(ctx context.Context, tx *sql.Tx, accountID int64, prepared preparedImport, now time.Time) error {
	var providerID, currentLabel string
	var currentEmail sql.NullString
	if err := tx.QueryRowContext(ctx, "SELECT provider_account_id,email,label FROM accounts WHERE id=? AND deleted_at IS NULL", accountID).Scan(&providerID, &currentEmail, &currentLabel); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return &ServiceError{Kind: ErrorNotFound, Code: "account_not_found"}
		}
		return internalError("account_lookup")
	}
	if providerID != prepared.status.ProviderAccountID {
		return &ServiceError{Kind: ErrorInvalid, Code: "provider_account_mismatch"}
	}
	envelope, err := s.cipher.Seal(prepared.plaintext, credentialcrypto.CredentialAAD(accountID, string(prepared.kind)))
	if err != nil {
		return internalError("credential_encrypt")
	}
	expiry := prepared.status.SubscriptionExpiry.UTC()
	stamp := now.Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, "UPDATE authorization_epochs SET ended_at=? WHERE account_id=? AND ended_at IS NULL", stamp, accountID); err != nil {
		return internalError("epoch_close")
	}
	label := reauthorizeLabel("", currentLabel, providerID, currentEmail, prepared.status.Email)
	if _, err := tx.ExecContext(ctx, `UPDATE accounts SET email=CASE WHEN (email IS NULL OR email='') AND ? IS NOT NULL THEN ? ELSE email END,
		label=?,token_type=?,enc_credentials=?,credential_key_id=?,plan=?,raw_plan=?,current_expiry=?,auth_expiry=?,
		status='alive',last_alive_at=?,dead_at=NULL,death_type=NULL,banned_survival_days=NULL,import_time=?,last_check_state='ok',last_check_error_code=NULL,next_retry_at=NULL,
		polling_paused=0,pause_reason=NULL,pending_evidence_signature=NULL,pending_detected_at=NULL,credential_generation=credential_generation+1,updated_at=? WHERE id=?`,
		nullable(prepared.status.Email), nullable(prepared.status.Email), label, string(prepared.kind), envelope, s.cipher.ActiveKeyID(), string(prepared.status.Plan), prepared.status.RawPlan, expiry.Format(time.RFC3339Nano), expiry.Format(time.RFC3339Nano), stamp, stamp, stamp, accountID); err != nil {
		return internalError("account_reauthorize")
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO authorization_epochs(account_id,started_at,auth_expiry) VALUES (?,?,?)", accountID, stamp, expiry.Format(time.RFC3339Nano)); err != nil {
		return internalError("epoch_insert")
	}
	return nil
}

func (s *Service) loadDevice(ctx context.Context, sessionID string) (deviceRecord, error) {
	var record deviceRecord
	var expires, updated string
	err := s.db.QueryRowContext(ctx, `SELECT id,account_id,enc_device_code,interval_seconds,expires_at,state,updated_at FROM device_auth_sessions WHERE id=?`, sessionID).
		Scan(&record.id, &record.accountID, &record.envelope, &record.interval, &expires, &record.state, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return deviceRecord{}, &ServiceError{Kind: ErrorNotFound, Code: "device_session_not_found"}
	}
	if err != nil {
		return deviceRecord{}, internalError("device_session_lookup")
	}
	record.expiresAt, err = time.Parse(time.RFC3339Nano, expires)
	if err != nil {
		return deviceRecord{}, internalError("device_session_expiry")
	}
	record.updatedAt, err = time.Parse(time.RFC3339Nano, updated)
	if err != nil {
		return deviceRecord{}, internalError("device_session_updated")
	}
	record.updatedRaw = updated
	return record, nil
}

func (s *Service) updatePendingDevice(ctx context.Context, sessionID string, payload devicePayload, interval int, now time.Time) error {
	plaintext, err := json.Marshal(payload)
	if err != nil {
		return internalError("device_session_encode")
	}
	defer zero(plaintext)
	envelope, err := s.cipher.Seal(plaintext, credentialcrypto.DeviceSessionAAD(sessionID))
	if err != nil {
		return internalError("device_session_encrypt")
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE device_auth_sessions SET enc_device_code=?,credential_key_id=?,interval_seconds=?,updated_at=? WHERE id=? AND state='pending'`, envelope, s.cipher.ActiveKeyID(), interval, now.Format(time.RFC3339Nano), sessionID); err != nil {
		return internalError("device_session_update")
	}
	return nil
}

func (s *Service) finishDevice(ctx context.Context, sessionID, state string, accountID *int64) error {
	var target any
	if accountID != nil {
		target = *accountID
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE device_auth_sessions SET account_id=COALESCE(?,account_id),enc_device_code=x'',credential_key_id='',state=?,updated_at=? WHERE id=?`, target, state, s.now().UTC().Format(time.RFC3339Nano), sessionID); err != nil {
		return internalError("device_session_finish")
	}
	return nil
}

func (s *Service) cleanupExpiredDeviceSessions(ctx context.Context) error {
	stamp := s.now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.ExecContext(ctx, `UPDATE device_auth_sessions SET enc_device_code=x'',credential_key_id='',state='expired',updated_at=?
		WHERE state='pending' AND expires_at<=?`, stamp, stamp); err != nil {
		return internalError("device_session_cleanup")
	}
	return nil
}

func randomDeviceSessionID() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func ceilSeconds(duration time.Duration) int {
	seconds := int(duration / time.Second)
	if duration%time.Second != 0 {
		seconds++
	}
	if seconds < 1 {
		return 1
	}
	return seconds
}
