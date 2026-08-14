package account

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"time"

	"chatgpt-monitor/internal/chatgpt"
	credentialcrypto "chatgpt-monitor/internal/crypto"
)

const (
	oauthSessionTTL      = 15 * time.Minute
	oauthWorkflowTimeout = 20 * time.Second
	oauthCallbackMaxLen  = 8 << 10
)

type OAuthClient interface {
	BuildOAuthAuthorizationURL(state, codeChallenge string) string
	ExchangeOAuthCode(context.Context, string, string) (chatgpt.TokenSet, error)
}

type OAuthStart struct {
	SessionID        string    `json:"session_id"`
	AuthorizationURL string    `json:"authorization_url"`
	ExpiresAt        time.Time `json:"expires_at"`
}

type oauthPayload struct {
	State        string `json:"state"`
	CodeVerifier string `json:"code_verifier"`
	Label        string `json:"label,omitempty"`
	Reauthorize  bool   `json:"reauthorize,omitempty"`
}

type oauthRecord struct {
	id        string
	accountID sql.NullInt64
	envelope  []byte
	state     string
	expiresAt time.Time
}

func (s *Service) StartOAuthImport(ctx context.Context, label string) (OAuthStart, error) {
	return s.startOAuth(ctx, label, nil)
}

func (s *Service) StartOAuthReauthorization(ctx context.Context, accountID int64) (OAuthStart, error) {
	if accountID <= 0 {
		return OAuthStart{}, &ServiceError{Kind: ErrorInvalid, Code: "account_id_invalid"}
	}
	if _, err := s.Get(ctx, accountID); err != nil {
		return OAuthStart{}, err
	}
	return s.startOAuth(ctx, "", &accountID)
}

func (s *Service) startOAuth(ctx context.Context, label string, accountID *int64) (OAuthStart, error) {
	label = strings.TrimSpace(label)
	if len(label) > maxLabelLength {
		return OAuthStart{}, &ServiceError{Kind: ErrorInvalid, Code: "label_too_large"}
	}
	client, ok := s.client.(OAuthClient)
	if !ok {
		return OAuthStart{}, internalError("oauth_client_unavailable")
	}
	if err := s.cleanupExpiredOAuthSessions(ctx); err != nil {
		return OAuthStart{}, err
	}
	sessionID, err := randomOAuthValue(32)
	if err != nil {
		return OAuthStart{}, internalError("oauth_session_id")
	}
	state, err := randomOAuthValue(32)
	if err != nil {
		return OAuthStart{}, internalError("oauth_state")
	}
	verifierBytes := make([]byte, 64)
	if _, err := rand.Read(verifierBytes); err != nil {
		return OAuthStart{}, internalError("oauth_verifier")
	}
	verifier := hex.EncodeToString(verifierBytes)
	for index := range verifierBytes {
		verifierBytes[index] = 0
	}
	challengeHash := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeHash[:])
	payload := oauthPayload{State: state, CodeVerifier: verifier, Label: label, Reauthorize: accountID != nil}
	plaintext, err := json.Marshal(payload)
	payload.CodeVerifier, payload.State = "", ""
	if err != nil {
		return OAuthStart{}, internalError("oauth_session_encode")
	}
	defer zero(plaintext)
	envelope, err := s.cipher.Seal(plaintext, credentialcrypto.OAuthSessionAAD(sessionID))
	if err != nil {
		return OAuthStart{}, internalError("oauth_session_encrypt")
	}
	now := s.now().UTC()
	expiresAt := now.Add(oauthSessionTTL)
	var target any
	if accountID != nil {
		target = *accountID
	}
	stamp := now.Format(time.RFC3339Nano)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO oauth_auth_sessions
		(id,account_id,enc_session,credential_key_id,state,expires_at,created_at,updated_at)
		VALUES (?,?,?,?,'pending',?,?,?)`, sessionID, target, envelope, s.cipher.ActiveKeyID(), expiresAt.Format(time.RFC3339Nano), stamp, stamp); err != nil {
		return OAuthStart{}, internalError("oauth_session_store")
	}
	return OAuthStart{
		SessionID:        sessionID,
		AuthorizationURL: client.BuildOAuthAuthorizationURL(state, challenge),
		ExpiresAt:        expiresAt,
	}, nil
}

func (s *Service) CompleteOAuth(ctx context.Context, sessionID, callbackURL string) (Account, error) {
	if len(sessionID) < 32 || len(sessionID) > 128 {
		return Account{}, &ServiceError{Kind: ErrorInvalid, Code: "oauth_session_invalid"}
	}
	if len(callbackURL) == 0 || len(callbackURL) > oauthCallbackMaxLen {
		return Account{}, &ServiceError{Kind: ErrorInvalid, Code: "oauth_callback_invalid"}
	}
	if err := s.cleanupExpiredOAuthSessions(ctx); err != nil {
		return Account{}, err
	}
	record, err := s.loadOAuth(ctx, sessionID)
	if err != nil {
		return Account{}, err
	}
	if record.state == "expired" {
		return Account{}, &ServiceError{Kind: ErrorInvalid, Code: "oauth_session_expired"}
	}
	if record.state != "pending" {
		return Account{}, &ServiceError{Kind: ErrorInvalid, Code: "oauth_session_used"}
	}
	now := s.now().UTC()
	if !now.Before(record.expiresAt) {
		_ = s.finishOAuth(ctx, sessionID, "expired", nil)
		return Account{}, &ServiceError{Kind: ErrorInvalid, Code: "oauth_session_expired"}
	}
	plaintext, err := s.cipher.Open(record.envelope, credentialcrypto.OAuthSessionAAD(sessionID))
	if err != nil {
		_ = s.finishOAuth(ctx, sessionID, "failed", nil)
		return Account{}, internalError("oauth_session_decrypt")
	}
	defer zero(plaintext)
	var payload oauthPayload
	if json.Unmarshal(plaintext, &payload) != nil || payload.State == "" || payload.CodeVerifier == "" {
		_ = s.finishOAuth(ctx, sessionID, "failed", nil)
		return Account{}, internalError("oauth_session_decode")
	}
	code, callbackState, err := parseOAuthCallback(callbackURL)
	callbackURL = ""
	if err != nil {
		return Account{}, err
	}
	if len(callbackState) != len(payload.State) || subtle.ConstantTimeCompare([]byte(callbackState), []byte(payload.State)) != 1 {
		return Account{}, &ServiceError{Kind: ErrorInvalid, Code: "oauth_state_mismatch"}
	}
	reserved, err := s.db.ExecContext(ctx, `UPDATE oauth_auth_sessions SET state='exchanging',updated_at=?
		WHERE id=? AND state='pending'`, now.Format(time.RFC3339Nano), sessionID)
	if err != nil {
		return Account{}, internalError("oauth_session_reserve")
	}
	affected, _ := reserved.RowsAffected()
	if affected != 1 {
		return Account{}, &ServiceError{Kind: ErrorInvalid, Code: "oauth_session_used"}
	}
	client, ok := s.client.(OAuthClient)
	if !ok {
		_ = s.finishOAuth(ctx, sessionID, "failed", nil)
		return Account{}, internalError("oauth_client_unavailable")
	}
	requestCtx, cancel := context.WithTimeout(ctx, oauthWorkflowTimeout)
	defer cancel()
	tokens, err := client.ExchangeOAuthCode(requestCtx, code, payload.CodeVerifier)
	code, payload.CodeVerifier, payload.State = "", "", ""
	if err != nil {
		_ = s.finishOAuth(ctx, sessionID, "failed", nil)
		return Account{}, classifyUpstream(err)
	}
	prepared, err := s.prepareOAuthTokens(requestCtx, tokens)
	tokens = chatgpt.TokenSet{}
	if err != nil {
		_ = s.finishOAuth(ctx, sessionID, "failed", nil)
		return Account{}, err
	}
	defer zero(prepared.plaintext)
	accountID, err := s.completeOAuth(ctx, record, payload, prepared, now)
	if err != nil {
		_ = s.finishOAuth(ctx, sessionID, "failed", nil)
		return Account{}, err
	}
	return s.Get(ctx, accountID)
}

func parseOAuthCallback(raw string) (string, string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme != "http" || parsed.Hostname() != "localhost" || parsed.Port() != "1455" ||
		parsed.Path != "/auth/callback" || parsed.User != nil || parsed.Fragment != "" {
		return "", "", &ServiceError{Kind: ErrorInvalid, Code: "oauth_callback_invalid"}
	}
	query := parsed.Query()
	if query.Get("error") != "" {
		return "", "", &ServiceError{Kind: ErrorInvalid, Code: "oauth_authorization_denied"}
	}
	code, state := query.Get("code"), query.Get("state")
	if code == "" || state == "" || len(query["code"]) != 1 || len(query["state"]) != 1 || len(code) > 4096 || len(state) > 256 {
		return "", "", &ServiceError{Kind: ErrorInvalid, Code: "oauth_callback_invalid"}
	}
	return code, state, nil
}

func (s *Service) prepareOAuthTokens(ctx context.Context, tokens chatgpt.TokenSet) (preparedImport, error) {
	if tokens.AccessToken == "" || tokens.RefreshToken == "" {
		return preparedImport{}, &ServiceError{Kind: ErrorInvalid, Code: "oauth_token_incomplete"}
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
	plaintext, err := json.Marshal(newCredentialPayload(tokens, "", "oauth"))
	tokens = chatgpt.TokenSet{}
	if err != nil {
		return preparedImport{}, internalError("credential_encode")
	}
	// OAuth credentials are refreshable and use the existing refresh credential
	// storage type; the authorization session itself records the OAuth source.
	return preparedImport{kind: chatgpt.CredentialRefresh, status: status, plaintext: plaintext}, nil
}

func (s *Service) completeOAuth(ctx context.Context, record oauthRecord, payload oauthPayload, prepared preparedImport, now time.Time) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, internalError("transaction_begin")
	}
	defer tx.Rollback()
	var state string
	if err := tx.QueryRowContext(ctx, "SELECT state FROM oauth_auth_sessions WHERE id=?", record.id).Scan(&state); err != nil || state != "exchanging" {
		return 0, internalError("oauth_session_state")
	}
	var accountID int64
	if payload.Reauthorize {
		if !record.accountID.Valid {
			return 0, internalError("oauth_reauthorization_target")
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
	if err := enqueueAuthorizationEvent(ctx, tx, accountID, payload.Reauthorize, now); err != nil {
		return 0, internalError("allocation_sync_enqueue")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE oauth_auth_sessions SET account_id=?,enc_session=x'',credential_key_id='',state='authorized',updated_at=? WHERE id=?`,
		accountID, now.Format(time.RFC3339Nano), record.id); err != nil {
		return 0, internalError("oauth_session_complete")
	}
	if err := tx.Commit(); err != nil {
		return 0, internalError("transaction_commit")
	}
	return accountID, nil
}

func (s *Service) loadOAuth(ctx context.Context, sessionID string) (oauthRecord, error) {
	var record oauthRecord
	var expires string
	err := s.db.QueryRowContext(ctx, `SELECT id,account_id,enc_session,state,expires_at FROM oauth_auth_sessions WHERE id=?`, sessionID).
		Scan(&record.id, &record.accountID, &record.envelope, &record.state, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return oauthRecord{}, &ServiceError{Kind: ErrorNotFound, Code: "oauth_session_not_found"}
	}
	if err != nil {
		return oauthRecord{}, internalError("oauth_session_lookup")
	}
	record.expiresAt, err = time.Parse(time.RFC3339Nano, expires)
	if err != nil {
		return oauthRecord{}, internalError("oauth_session_expiry")
	}
	return record, nil
}

func (s *Service) finishOAuth(ctx context.Context, sessionID, state string, accountID *int64) error {
	var target any
	if accountID != nil {
		target = *accountID
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE oauth_auth_sessions SET account_id=COALESCE(?,account_id),
		enc_session=x'',credential_key_id='',state=?,updated_at=? WHERE id=?`,
		target, state, s.now().UTC().Format(time.RFC3339Nano), sessionID); err != nil {
		return internalError("oauth_session_finish")
	}
	return nil
}

func (s *Service) cleanupExpiredOAuthSessions(ctx context.Context) error {
	now := s.now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.ExecContext(ctx, `UPDATE oauth_auth_sessions SET enc_session=x'',credential_key_id='',state='expired',updated_at=?
		WHERE state IN ('pending','exchanging') AND expires_at<=?`, now, now); err != nil {
		return internalError("oauth_session_cleanup")
	}
	return nil
}

func randomOAuthValue(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	result := base64.RawURLEncoding.EncodeToString(value)
	for index := range value {
		value[index] = 0
	}
	return result, nil
}
