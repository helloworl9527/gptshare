package account

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"chatgpt-monitor/internal/chatgpt"
	credentialcrypto "chatgpt-monitor/internal/crypto"
)

const (
	maxCredentialLength = 16 * 1024
	maxCombinedLength   = 32 * 1024
	maxLabelLength      = 256
)

type ErrorKind string

const (
	ErrorInvalid     ErrorKind = "invalid"
	ErrorDuplicate   ErrorKind = "duplicate"
	ErrorNotFound    ErrorKind = "not_found"
	ErrorUnavailable ErrorKind = "upstream_unavailable"
	ErrorInternal    ErrorKind = "internal"
)

type ServiceError struct {
	Kind      ErrorKind
	Code      string
	Retryable bool
}

func (e *ServiceError) Error() string { return "account: " + string(e.Kind) + " (" + e.Code + ")" }

type ChatGPTClient interface {
	ExchangeCredential(context.Context, chatgpt.CredentialKind, string) (chatgpt.TokenSet, error)
	FetchStatus(context.Context, string) (chatgpt.StatusResult, error)
}

type Cipher interface {
	ActiveKeyID() string
	Seal([]byte, []byte) ([]byte, error)
	Open([]byte, []byte) ([]byte, error)
}

type Service struct {
	db     *sql.DB
	client ChatGPTClient
	device DeviceClient
	cipher Cipher
	now    func() time.Time
}

type TokenInput struct {
	Label        string `json:"label"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	SessionToken string `json:"session_token"`
}

type CredentialSummary struct {
	Type       string `json:"type"`
	Configured bool   `json:"configured"`
}

type Account struct {
	ID                 int64             `json:"id"`
	ProviderAccountID  string            `json:"provider_account_id"`
	Email              *string           `json:"email"`
	Label              string            `json:"label"`
	Plan               string            `json:"plan"`
	CurrentExpiry      *time.Time        `json:"current_expiry"`
	AuthExpiry         time.Time         `json:"auth_expiry"`
	Status             string            `json:"status"`
	LastAliveAt        *time.Time        `json:"last_alive_at,omitempty"`
	DeadAt             *time.Time        `json:"dead_at,omitempty"`
	DeathType          string            `json:"death_type,omitempty"`
	BannedSurvivalDays *float64          `json:"banned_survival_days,omitempty"`
	LastCheckState     string            `json:"last_check_state"`
	LastCheckErrorCode string            `json:"last_check_error_code,omitempty"`
	NextRetryAt        *time.Time        `json:"next_retry_at,omitempty"`
	NearExpiry         bool              `json:"near_expiry"`
	PollingPaused      bool              `json:"polling_paused"`
	LastAuthorizedAt   time.Time         `json:"last_authorized_at"`
	Credential         CredentialSummary `json:"credential"`
}

type credentialPayload struct {
	Access          string     `json:"access,omitempty"`
	Refresh         string     `json:"refresh,omitempty"`
	Session         string     `json:"session,omitempty"`
	IDToken         string     `json:"id_token,omitempty"`
	OAuthSource     string     `json:"oauth_source,omitempty"`
	AccessExpiresAt *time.Time `json:"access_expires_at,omitempty"`
}

type preparedImport struct {
	kind      chatgpt.CredentialKind
	status    chatgpt.StatusResult
	plaintext []byte
}

func NewService(db *sql.DB, client ChatGPTClient, cipher Cipher) (*Service, error) {
	if db == nil || client == nil || cipher == nil {
		return nil, errors.New("account service dependencies are required")
	}
	device, _ := client.(DeviceClient)
	return &Service{db: db, client: client, device: device, cipher: cipher, now: time.Now}, nil
}

func (s *Service) ImportByToken(ctx context.Context, input *TokenInput) (Account, error) {
	if input == nil {
		return Account{}, &ServiceError{Kind: ErrorInvalid, Code: "credential_required"}
	}
	defer clearInput(input)
	prepared, err := s.prepare(ctx, input)
	if err != nil {
		return Account{}, err
	}
	defer zero(prepared.plaintext)
	return s.importPrepared(ctx, input.Label, prepared)
}

func (s *Service) importPrepared(ctx context.Context, inputLabel string, prepared preparedImport) (Account, error) {
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Account{}, internalError("transaction_begin")
	}
	defer tx.Rollback()
	var existing int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM accounts WHERE provider_account_id=? AND deleted_at IS NULL", prepared.status.ProviderAccountID).Scan(&existing); err != nil {
		return Account{}, internalError("duplicate_check")
	}
	if existing != 0 {
		return Account{}, &ServiceError{Kind: ErrorDuplicate, Code: "provider_account_exists"}
	}
	expiry := prepared.status.SubscriptionExpiry.UTC()
	email := nullable(prepared.status.Email)
	label := defaultLabel(inputLabel, prepared.status)
	result, err := tx.ExecContext(ctx, `INSERT INTO accounts
		(provider_account_id,email,label,token_type,enc_credentials,credential_key_id,plan,raw_plan,current_expiry,auth_expiry,status,last_alive_at,import_time,last_check_state,last_check_error_code,updated_at)
		VALUES (?,?,?,?,x'',?,?,?,?,?,'alive',?,?,'ok',NULL,?)`,
		prepared.status.ProviderAccountID, email, label, string(prepared.kind), s.cipher.ActiveKeyID(), string(prepared.status.Plan), prepared.status.RawPlan,
		expiry.Format(time.RFC3339Nano), expiry.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") {
			return Account{}, &ServiceError{Kind: ErrorDuplicate, Code: "provider_account_exists"}
		}
		return Account{}, internalError("account_insert")
	}
	accountID, err := result.LastInsertId()
	if err != nil {
		return Account{}, internalError("account_id")
	}
	envelope, err := s.cipher.Seal(prepared.plaintext, credentialcrypto.CredentialAAD(accountID, string(prepared.kind)))
	if err != nil {
		return Account{}, internalError("credential_encrypt")
	}
	if _, err := tx.ExecContext(ctx, "UPDATE accounts SET enc_credentials=? WHERE id=?", envelope, accountID); err != nil {
		return Account{}, internalError("credential_store")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO authorization_epochs(account_id,started_at,auth_expiry)
		VALUES (?,?,?)`, accountID, now.Format(time.RFC3339Nano), expiry.Format(time.RFC3339Nano)); err != nil {
		return Account{}, internalError("epoch_insert")
	}
	if err := tx.Commit(); err != nil {
		return Account{}, internalError("transaction_commit")
	}
	return s.Get(ctx, accountID)
}

func (s *Service) ReauthorizeByToken(ctx context.Context, accountID int64, input *TokenInput) (Account, error) {
	if input == nil {
		return Account{}, &ServiceError{Kind: ErrorInvalid, Code: "credential_required"}
	}
	defer clearInput(input)
	prepared, err := s.prepare(ctx, input)
	if err != nil {
		return Account{}, err
	}
	defer zero(prepared.plaintext)
	now := s.now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Account{}, internalError("transaction_begin")
	}
	defer tx.Rollback()
	var providerID, currentLabel string
	var currentEmail sql.NullString
	if err := tx.QueryRowContext(ctx, "SELECT provider_account_id,email,label FROM accounts WHERE id=? AND deleted_at IS NULL", accountID).Scan(&providerID, &currentEmail, &currentLabel); err != nil {
		if err == sql.ErrNoRows {
			return Account{}, &ServiceError{Kind: ErrorNotFound, Code: "account_not_found"}
		}
		return Account{}, internalError("account_lookup")
	}
	if providerID != prepared.status.ProviderAccountID {
		return Account{}, &ServiceError{Kind: ErrorInvalid, Code: "provider_account_mismatch"}
	}
	envelope, err := s.cipher.Seal(prepared.plaintext, credentialcrypto.CredentialAAD(accountID, string(prepared.kind)))
	if err != nil {
		return Account{}, internalError("credential_encrypt")
	}
	expiry := prepared.status.SubscriptionExpiry.UTC()
	stamp := now.Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, "UPDATE authorization_epochs SET ended_at=? WHERE account_id=? AND ended_at IS NULL", stamp, accountID); err != nil {
		return Account{}, internalError("epoch_close")
	}
	label := reauthorizeLabel(input.Label, currentLabel, providerID, currentEmail, prepared.status.Email)
	if _, err := tx.ExecContext(ctx, `UPDATE accounts SET
		email=CASE WHEN (email IS NULL OR email='') AND ? IS NOT NULL THEN ? ELSE email END,
		label=?,token_type=?,enc_credentials=?,credential_key_id=?,plan=?,raw_plan=?,current_expiry=?,auth_expiry=?,
		status='alive',last_alive_at=?,dead_at=NULL,death_type=NULL,banned_survival_days=NULL,import_time=?,last_check_state='ok',last_check_error_code=NULL,next_retry_at=NULL,
		polling_paused=0,pause_reason=NULL,pending_evidence_signature=NULL,pending_detected_at=NULL,credential_generation=credential_generation+1,updated_at=?
		WHERE id=?`, nullable(prepared.status.Email), nullable(prepared.status.Email), label, string(prepared.kind), envelope, s.cipher.ActiveKeyID(), string(prepared.status.Plan), prepared.status.RawPlan,
		expiry.Format(time.RFC3339Nano), expiry.Format(time.RFC3339Nano), stamp, stamp, stamp, accountID); err != nil {
		return Account{}, internalError("account_reauthorize")
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO authorization_epochs(account_id,started_at,auth_expiry) VALUES (?,?,?)", accountID, stamp, expiry.Format(time.RFC3339Nano)); err != nil {
		return Account{}, internalError("epoch_insert")
	}
	if err := tx.Commit(); err != nil {
		return Account{}, internalError("transaction_commit")
	}
	return s.Get(ctx, accountID)
}

func (s *Service) Delete(ctx context.Context, accountID int64) error {
	now := s.now().UTC().Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return internalError("transaction_begin")
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE accounts SET deleted_at=COALESCE(deleted_at,?),enc_credentials=x'',credential_key_id='',credential_generation=credential_generation+1,updated_at=? WHERE id=?`, now, now, accountID); err != nil {
		return internalError("account_delete")
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM device_auth_sessions WHERE account_id=?", accountID); err != nil {
		return internalError("device_session_delete")
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM oauth_auth_sessions WHERE account_id=?", accountID); err != nil {
		return internalError("oauth_session_delete")
	}
	if err := tx.Commit(); err != nil {
		return internalError("transaction_commit")
	}
	return nil
}

func (s *Service) List(ctx context.Context) ([]Account, error) {
	nearExpiryDays := s.nearExpiryDays(ctx)
	rows, err := s.db.QueryContext(ctx, accountSelect+` WHERE a.deleted_at IS NULL ORDER BY a.id`)
	if err != nil {
		return nil, internalError("account_list")
	}
	defer rows.Close()
	accounts := make([]Account, 0)
	for rows.Next() {
		account, err := scanAccount(rows, s.now().UTC(), nearExpiryDays)
		if err != nil {
			return nil, internalError("account_scan")
		}
		accounts = append(accounts, account)
	}
	if err := rows.Err(); err != nil {
		return nil, internalError("account_list")
	}
	return accounts, nil
}

func (s *Service) Get(ctx context.Context, accountID int64) (Account, error) {
	account, err := scanAccount(s.db.QueryRowContext(ctx, accountSelect+` WHERE a.id=? AND a.deleted_at IS NULL`, accountID), s.now().UTC(), s.nearExpiryDays(ctx))
	if err == sql.ErrNoRows {
		return Account{}, &ServiceError{Kind: ErrorNotFound, Code: "account_not_found"}
	}
	if err != nil {
		return Account{}, internalError("account_get")
	}
	return account, nil
}

const accountSelect = `SELECT a.id,a.provider_account_id,a.label,a.plan,a.current_expiry,a.auth_expiry,a.status,
	a.last_alive_at,a.dead_at,a.death_type,a.banned_survival_days,a.last_check_state,a.last_check_error_code,a.next_retry_at,a.polling_paused,
	e.started_at,a.token_type,length(a.enc_credentials)>0,a.email
	FROM accounts a JOIN authorization_epochs e ON e.id=(SELECT e2.id FROM authorization_epochs e2 WHERE e2.account_id=a.id ORDER BY e2.started_at DESC,e2.id DESC LIMIT 1)`

type scanner interface{ Scan(...any) error }

func scanAccount(row scanner, now time.Time, nearExpiryDays int) (Account, error) {
	var account Account
	var currentExpiry, lastAlive, deadAt, deathType, lastError, nextRetry, email sql.NullString
	var survival sql.NullFloat64
	var authExpiry, authorizedAt, credentialType string
	var configured bool
	if err := row.Scan(&account.ID, &account.ProviderAccountID, &account.Label, &account.Plan, &currentExpiry, &authExpiry, &account.Status,
		&lastAlive, &deadAt, &deathType, &survival, &account.LastCheckState, &lastError, &nextRetry, &account.PollingPaused,
		&authorizedAt, &credentialType, &configured, &email); err != nil {
		return Account{}, err
	}
	parsedAuth, err := time.Parse(time.RFC3339Nano, authExpiry)
	if err != nil {
		return Account{}, err
	}
	parsedAuthorized, err := time.Parse(time.RFC3339Nano, authorizedAt)
	if err != nil {
		return Account{}, err
	}
	if currentExpiry.Valid {
		parsed, err := time.Parse(time.RFC3339Nano, currentExpiry.String)
		if err != nil {
			return Account{}, err
		}
		account.CurrentExpiry = &parsed
	}
	if lastAlive.Valid {
		parsed, err := time.Parse(time.RFC3339Nano, lastAlive.String)
		if err != nil {
			return Account{}, err
		}
		account.LastAliveAt = &parsed
	}
	if deadAt.Valid {
		parsed, err := time.Parse(time.RFC3339Nano, deadAt.String)
		if err != nil {
			return Account{}, err
		}
		account.DeadAt = &parsed
	}
	if nextRetry.Valid {
		parsed, err := time.Parse(time.RFC3339Nano, nextRetry.String)
		if err != nil {
			return Account{}, err
		}
		account.NextRetryAt = &parsed
	}
	if deathType.Valid {
		account.DeathType = deathType.String
	}
	if survival.Valid {
		account.BannedSurvivalDays = &survival.Float64
	}
	if lastError.Valid {
		account.LastCheckErrorCode = lastError.String
	}
	if email.Valid && strings.TrimSpace(email.String) != "" {
		value := email.String
		account.Email = &value
	}
	account.AuthExpiry = parsedAuth
	account.LastAuthorizedAt = parsedAuthorized
	account.Credential = CredentialSummary{Type: credentialType, Configured: configured}
	account.NearExpiry = account.Status == "alive" && !account.AuthExpiry.Before(now) && !account.AuthExpiry.After(now.Add(time.Duration(nearExpiryDays)*24*time.Hour))
	return account, nil
}

func (s *Service) nearExpiryDays(ctx context.Context) int {
	var value []byte
	if err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key='near_expiry_days' AND is_secret=0").Scan(&value); err == nil {
		if days, parseErr := strconv.Atoi(string(value)); parseErr == nil && days >= 1 && days <= 30 {
			return days
		}
	}
	return 3
}

func (s *Service) prepare(ctx context.Context, input *TokenInput) (preparedImport, error) {
	input.Label = strings.TrimSpace(input.Label)
	input.AccessToken = strings.TrimSpace(input.AccessToken)
	input.RefreshToken = strings.TrimSpace(input.RefreshToken)
	input.SessionToken = strings.TrimSpace(input.SessionToken)
	if len(input.Label) > maxLabelLength || len(input.AccessToken) > maxCredentialLength || len(input.RefreshToken) > maxCredentialLength || len(input.SessionToken) > maxCredentialLength || len(input.AccessToken)+len(input.RefreshToken)+len(input.SessionToken) > maxCombinedLength {
		return preparedImport{}, &ServiceError{Kind: ErrorInvalid, Code: "credential_input_too_large"}
	}
	var kind chatgpt.CredentialKind
	var secret string
	switch {
	case input.AccessToken != "":
		kind, secret = chatgpt.CredentialAccess, input.AccessToken
	case input.RefreshToken != "":
		kind, secret = chatgpt.CredentialRefresh, input.RefreshToken
	case input.SessionToken != "":
		kind, secret = chatgpt.CredentialSession, input.SessionToken
	default:
		return preparedImport{}, &ServiceError{Kind: ErrorInvalid, Code: "credential_required"}
	}
	tokens, err := s.client.ExchangeCredential(ctx, kind, secret)
	secret = ""
	if err != nil {
		return preparedImport{}, classifyUpstream(err)
	}
	if tokens.RefreshToken == "" {
		tokens.RefreshToken = input.RefreshToken
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
	trustedSource := ""
	if kind == chatgpt.CredentialRefresh {
		trustedSource = "refresh"
	}
	payload, err := json.Marshal(newCredentialPayload(tokens, input.SessionToken, trustedSource))
	tokens = chatgpt.TokenSet{}
	input.AccessToken, input.RefreshToken, input.SessionToken = "", "", ""
	if err != nil {
		return preparedImport{}, internalError("credential_encode")
	}
	return preparedImport{kind: kind, status: status, plaintext: payload}, nil
}

func newCredentialPayload(tokens chatgpt.TokenSet, session, source string) credentialPayload {
	expiresAt := tokens.AccessExpiresAt
	if expiresAt == nil {
		if parsed, ok := chatgpt.AccessTokenExpiry(tokens.AccessToken); ok {
			expiresAt = &parsed
		}
	}
	return credentialPayload{
		Access: tokens.AccessToken, Refresh: tokens.RefreshToken, Session: session,
		IDToken: tokens.IDToken, OAuthSource: source, AccessExpiresAt: expiresAt,
	}
}

func validateCredentialStatus(status chatgpt.StatusResult, now time.Time) error {
	code := ""
	switch {
	case strings.TrimSpace(status.ProviderAccountID) == "":
		code = "credential_account_id_missing"
	case status.Plan == chatgpt.PlanUnknown:
		code = "credential_plan_unknown"
	case status.SubscriptionExpiry == nil:
		code = "credential_subscription_expiry_missing"
	case !now.Before(status.SubscriptionExpiry.UTC()):
		code = "credential_subscription_expired"
	case status.AccountState != chatgpt.StateActive:
		code = "credential_account_inactive"
	case status.EvidenceLevel != chatgpt.EvidenceLiveVerified:
		code = "credential_evidence_unverified"
	}
	if code != "" {
		return &ServiceError{Kind: ErrorInvalid, Code: code}
	}
	return nil
}

func classifyUpstream(err error) error {
	var typed *chatgpt.TypedError
	if errors.As(err, &typed) {
		if typed.Retryable || typed.Kind == chatgpt.ErrorRateLimited || typed.Kind == chatgpt.ErrorUpstreamTransient {
			return &ServiceError{Kind: ErrorUnavailable, Code: typed.EvidenceCode, Retryable: true}
		}
		return &ServiceError{Kind: ErrorInvalid, Code: typed.EvidenceCode}
	}
	return internalError("upstream_failure")
}

func internalError(code string) *ServiceError { return &ServiceError{Kind: ErrorInternal, Code: code} }

func zero(value []byte) {
	for index := range value {
		value[index] = 0
	}
}

func clearInput(input *TokenInput) {
	input.AccessToken = ""
	input.RefreshToken = ""
	input.SessionToken = ""
}

func defaultLabel(input string, status chatgpt.StatusResult) string {
	label := strings.TrimSpace(input)
	if label != "" {
		return label
	}
	if status.Email != "" {
		return status.Email
	}
	return status.ProviderAccountID
}

func reauthorizeLabel(input, current, providerID string, currentEmail sql.NullString, candidateEmail string) string {
	label := strings.TrimSpace(input)
	if label != "" {
		return label
	}
	if candidateEmail == "" {
		return current
	}
	if currentEmail.Valid && strings.TrimSpace(currentEmail.String) != "" && currentEmail.String != candidateEmail {
		return current
	}
	if strings.TrimSpace(current) == "" || current == providerID {
		return candidateEmail
	}
	return current
}

func nullable(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}
