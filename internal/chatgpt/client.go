package chatgpt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	defaultClientID       = "app_EMoamEEZ73f0CkXaXp7hrann"
	defaultTokenURL       = "https://auth.openai.com/oauth/token"
	defaultSessionURL     = "https://chatgpt.com/api/auth/session"
	defaultStatusURL      = "https://chatgpt.com/backend-api/accounts/check/v4-2023-04-27"
	defaultDeviceStartURL = "https://auth.openai.com/api/accounts/deviceauth/usercode"
	defaultDevicePollURL  = "https://auth.openai.com/api/accounts/deviceauth/token"
	defaultDeviceVerify   = "https://auth.openai.com/codex/device"
	deviceRedirectURI     = "https://auth.openai.com/deviceauth/callback"
)

type Config struct {
	HTTPClient      *http.Client
	ClientID        string
	TokenURL        string
	SessionURL      string
	StatusURL       string
	DeviceStartURL  string
	DevicePollURL   string
	DeviceVerifyURL string
	EvidenceLevel   EvidenceLevel
	Now             func() time.Time
}

type Client struct {
	httpClient      *http.Client
	clientID        string
	tokenURL        string
	sessionURL      string
	statusURL       string
	deviceStartURL  string
	devicePollURL   string
	deviceVerifyURL string
	evidenceLevel   EvidenceLevel
	now             func() time.Time
}

func NewClient(cfg Config) *Client {
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 15 * time.Second}
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	evidenceLevel := cfg.EvidenceLevel
	if evidenceLevel == "" {
		evidenceLevel = EvidenceContractVerifiedLivePending
	} else if evidenceLevel != EvidenceLiveVerified && evidenceLevel != EvidenceContractVerifiedLivePending && evidenceLevel != EvidenceUnverified {
		evidenceLevel = EvidenceUnverified
	}
	return &Client{
		httpClient:      httpClient,
		clientID:        valueOr(cfg.ClientID, defaultClientID),
		tokenURL:        valueOr(cfg.TokenURL, defaultTokenURL),
		sessionURL:      valueOr(cfg.SessionURL, defaultSessionURL),
		statusURL:       valueOr(cfg.StatusURL, defaultStatusURL),
		deviceStartURL:  valueOr(cfg.DeviceStartURL, defaultDeviceStartURL),
		devicePollURL:   valueOr(cfg.DevicePollURL, defaultDevicePollURL),
		deviceVerifyURL: valueOr(cfg.DeviceVerifyURL, defaultDeviceVerify),
		evidenceLevel:   evidenceLevel,
		now:             now,
	}
}

func valueOr(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}

func (c *Client) ExchangeCredential(ctx context.Context, kind CredentialKind, secret string) (TokenSet, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return TokenSet{}, newTypedError(ErrorInput, 0, "empty_credential", EvidenceUnverified, false, false, false, nil)
	}
	switch kind {
	case CredentialAccess:
		return TokenSet{AccessToken: secret}, nil
	case CredentialRefresh:
		return c.RefreshToken(ctx, secret)
	case CredentialSession:
		return c.exchangeSession(ctx, secret)
	default:
		return TokenSet{}, newTypedError(ErrorInput, 0, "unsupported_credential_kind", EvidenceUnverified, false, false, false, nil)
	}
}

func (c *Client) RefreshToken(ctx context.Context, refreshToken string) (TokenSet, error) {
	form := url.Values{
		"client_id":     {c.clientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {strings.TrimSpace(refreshToken)},
		"scope":         {"openid profile email"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return TokenSet{}, transient("refresh_request_create", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	status, body, _, err := c.do(req)
	if err != nil {
		return TokenSet{}, err
	}
	if status < 200 || status >= 300 {
		return TokenSet{}, classifyHTTP(status, body)
	}
	return decodeTokenSet(body, "refresh")
}

// RefreshExpiredAccess verifies the access token is expired, refreshes it, and
// confirms the refreshed credential is active without producing banned semantics.
func (c *Client) RefreshExpiredAccess(ctx context.Context, expiredAccessToken, refreshToken string) (StatusResult, TokenSet, error) {
	claims, err := parseAccessClaims(expiredAccessToken)
	if err != nil {
		return StatusResult{}, TokenSet{}, newTypedError(ErrorContractChanged, 0, "expired_access_claims_invalid", EvidenceContractVerifiedLivePending, false, false, true, err)
	}
	if claims.Exp == 0 || c.now().Before(time.Unix(claims.Exp, 0)) {
		return StatusResult{}, TokenSet{}, newTypedError(ErrorInput, 0, "access_token_not_expired", EvidenceUnverified, false, false, false, nil)
	}
	tokens, err := c.RefreshToken(ctx, refreshToken)
	if err != nil {
		return StatusResult{}, TokenSet{}, err
	}
	result, err := c.FetchStatus(ctx, tokens.AccessToken)
	if err != nil {
		return result, tokens, err
	}
	result.AccountState = StateAccessExpiredRefreshable
	result.EvidenceCode = "access_expired+refresh+accounts_check_2xx"
	return result, tokens, nil
}

func (c *Client) exchangeSession(ctx context.Context, sessionToken string) (TokenSet, error) {
	cookieNames := []string{"__Secure-next-auth.session-token", "__Secure-authjs.session-token"}
	var lastStatus int
	var lastBody []byte
	for _, cookieName := range cookieNames {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.sessionURL, nil)
		if err != nil {
			return TokenSet{}, transient("session_request_create", err)
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "chatgpt-monitor-probe/step-01")
		req.AddCookie(&http.Cookie{Name: cookieName, Value: sessionToken, Secure: true, HttpOnly: true})
		status, body, _, doErr := c.do(req)
		if doErr != nil {
			return TokenSet{}, doErr
		}
		lastStatus, lastBody = status, body
		if status < 200 || status >= 300 {
			continue
		}
		var payload struct {
			AccessToken      string `json:"accessToken"`
			AccessTokenSnake string `json:"access_token"`
		}
		if json.Unmarshal(body, &payload) == nil {
			access := strings.TrimSpace(payload.AccessToken)
			if access == "" {
				access = strings.TrimSpace(payload.AccessTokenSnake)
			}
			if access != "" {
				return TokenSet{AccessToken: access}, nil
			}
		}
	}
	if lastStatus >= 200 && lastStatus < 300 {
		return TokenSet{}, newTypedError(ErrorContractChanged, lastStatus, "session_missing_access_token", EvidenceContractVerifiedLivePending, false, false, true, nil)
	}
	return TokenSet{}, classifyHTTP(lastStatus, lastBody)
}

func (c *Client) FetchStatus(ctx context.Context, accessToken string) (StatusResult, error) {
	claims, err := parseAccessClaims(accessToken)
	if err != nil {
		return StatusResult{}, newTypedError(ErrorContractChanged, 0, "access_claims_invalid", EvidenceContractVerifiedLivePending, false, false, true, err)
	}
	result := StatusResult{
		ProviderAccountID:  claims.Auth.AccountID,
		RawPlan:            claims.Auth.Plan,
		Plan:               normalizePlan(claims.Auth.Plan),
		SubscriptionExpiry: claims.Auth.SubscriptionExpiry,
	}
	if result.ProviderAccountID == "" {
		return result, newTypedError(ErrorContractChanged, 0, "provider_account_claim_missing", EvidenceContractVerifiedLivePending, false, false, true, nil)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.statusURL, nil)
	if err != nil {
		return result, transient("status_request_create", err)
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
	req.Header.Set("X-Authorization", "Bearer "+strings.TrimSpace(accessToken))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Chatgpt-Account-Id", result.ProviderAccountID)
	req.Header.Set("User-Agent", "chatgpt-monitor-probe/step-01")
	status, body, hash, doErr := c.do(req)
	result.ResponseHash = hash
	if doErr != nil {
		var typed *Error
		if errors.As(doErr, &typed) {
			result.AccountState = stateForError(typed.Kind)
			result.EvidenceCode = typed.EvidenceCode
			result.EvidenceLevel = typed.EvidenceLevel
		}
		return result, doErr
	}
	if status >= 200 && status < 300 {
		if !json.Valid(body) {
			return result, newTypedError(ErrorContractChanged, status, "status_non_json", EvidenceContractVerifiedLivePending, false, false, true, nil)
		}
		account, parseErr := parseAccountCheck(body, result.ProviderAccountID)
		if parseErr != nil {
			return result, newTypedError(ErrorContractChanged, status, "account_check_contract_changed", EvidenceContractVerifiedLivePending, false, false, true, parseErr)
		}
		result.RawPlan = account.RawPlan
		result.Plan = normalizePlan(account.RawPlan)
		result.SubscriptionExpiry = account.SubscriptionExpiry
		if result.RawPlan == "" || (result.Plan != PlanFree && result.SubscriptionExpiry == nil) {
			return result, newTypedError(ErrorContractChanged, status, "account_check_core_field_missing", EvidenceContractVerifiedLivePending, false, false, true, nil)
		}
		result.AccountState = StateActive
		result.EvidenceCode = "access_claim+accounts_check_2xx"
		result.EvidenceLevel = c.evidenceLevel
		result.Email = ExtractEmail(accessToken, "", result.ProviderAccountID)
		return result, nil
	}
	classified := classifyHTTP(status, body)
	if typed, ok := classified.(*Error); ok {
		result.AccountState = stateForError(typed.Kind)
		result.EvidenceCode = typed.EvidenceCode
		result.EvidenceLevel = typed.EvidenceLevel
	}
	return result, classified
}

type accountCheckResult struct {
	RawPlan            string
	SubscriptionExpiry *time.Time
}

type accountCheckEntry struct {
	Account struct {
		AccountID string `json:"account_id"`
	} `json:"account"`
	Entitlement struct {
		SubscriptionPlan string          `json:"subscription_plan"`
		ExpiresAt        json.RawMessage `json:"expires_at"`
	} `json:"entitlement"`
}

func parseAccountCheck(body []byte, providerAccountID string) (accountCheckResult, error) {
	var payload struct {
		Accounts map[string]accountCheckEntry `json:"accounts"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return accountCheckResult{}, err
	}
	if len(payload.Accounts) == 0 {
		return accountCheckResult{}, fmt.Errorf("accounts missing")
	}
	var selected *accountCheckEntry
	for key, account := range payload.Accounts {
		if key == providerAccountID || account.Account.AccountID == providerAccountID {
			copy := account
			selected = &copy
			break
		}
	}
	if selected == nil && len(payload.Accounts) == 1 {
		for _, account := range payload.Accounts {
			copy := account
			selected = &copy
		}
	}
	if selected == nil {
		return accountCheckResult{}, fmt.Errorf("provider account not found")
	}
	result := accountCheckResult{RawPlan: selected.Entitlement.SubscriptionPlan}
	if len(selected.Entitlement.ExpiresAt) > 0 && string(selected.Entitlement.ExpiresAt) != "null" {
		expiry, err := parseTimeValue(selected.Entitlement.ExpiresAt)
		if err != nil {
			return accountCheckResult{}, err
		}
		result.SubscriptionExpiry = &expiry
	}
	return result, nil
}

type accessClaims struct {
	Exp  int64 `json:"exp"`
	Auth struct {
		AccountID          string          `json:"chatgpt_account_id"`
		Plan               string          `json:"chatgpt_plan_type"`
		SubscriptionUntil  json.RawMessage `json:"chatgpt_subscription_active_until"`
		SubscriptionExpiry *time.Time      `json:"-"`
	} `json:"https://api.openai.com/auth"`
}

func parseAccessClaims(token string) (accessClaims, error) {
	var claims accessClaims
	payload, err := jwtPayload(token)
	if err != nil {
		return claims, fmt.Errorf("decode claims: %w", err)
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return claims, fmt.Errorf("parse claims: %w", err)
	}
	if len(claims.Auth.SubscriptionUntil) != 0 && string(claims.Auth.SubscriptionUntil) != "null" {
		expiry, parseErr := parseTimeValue(claims.Auth.SubscriptionUntil)
		if parseErr != nil {
			return claims, parseErr
		}
		claims.Auth.SubscriptionExpiry = &expiry
	}
	return claims, nil
}

func parseTimeValue(raw json.RawMessage) (time.Time, error) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t, nil
		}
		if seconds, err := strconv.ParseInt(s, 10, 64); err == nil {
			return time.Unix(seconds, 0).UTC(), nil
		}
	}
	var number json.Number
	if json.Unmarshal(raw, &number) == nil {
		if seconds, err := number.Int64(); err == nil {
			return time.Unix(seconds, 0).UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("unsupported subscription expiry")
}

func normalizePlan(raw string) Plan {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "free", "chatgptfreeplan":
		return PlanFree
	case "plus", "chatgptplusplan":
		return PlanPlus
	case "team", "business", "chatgptteamplan", "chatgptbusinessplan":
		return PlanTeam
	default:
		return PlanUnknown
	}
}

func (c *Client) do(req *http.Request) (int, []byte, string, error) {
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return 0, nil, "", newTypedError(ErrorUpstreamTransient, 0, "upstream_timeout", EvidenceContractVerifiedLivePending, true, false, true, err)
		}
		return 0, nil, "", transient("upstream_transport", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return resp.StatusCode, nil, "", transient("upstream_read", err)
	}
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	if resp.StatusCode == http.StatusTooManyRequests {
		typed := classifyHTTP(resp.StatusCode, body).(*TypedError)
		typed.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"), c.now())
		return resp.StatusCode, body, hash, typed
	}
	return resp.StatusCode, body, hash, nil
}

func parseRetryAfter(value string, now time.Time) time.Duration {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if target, err := http.ParseTime(value); err == nil && target.After(now) {
		return target.Sub(now)
	}
	return 0
}

func transient(code string, cause error) *Error {
	return newTypedError(ErrorUpstreamTransient, 0, code, EvidenceContractVerifiedLivePending, true, false, true, cause)
}

func newTypedError(kind ErrorKind, status int, evidenceCode string, level EvidenceLevel, retryable, bannedCandidate, preserveBusinessState bool, cause error) *TypedError {
	return &TypedError{
		Kind:                  kind,
		StatusCode:            status,
		EvidenceCode:          evidenceCode,
		EvidenceLevel:         level,
		Retryable:             retryable,
		BannedCandidate:       bannedCandidate,
		PreserveBusinessState: preserveBusinessState,
		Cause:                 cause,
	}
}

func decodeTokenSet(body []byte, source string) (TokenSet, error) {
	var payload struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return TokenSet{}, newTypedError(ErrorContractChanged, 0, source+"_non_json", EvidenceContractVerifiedLivePending, false, false, true, err)
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return TokenSet{}, newTypedError(ErrorContractChanged, 0, source+"_missing_access_token", EvidenceContractVerifiedLivePending, false, false, true, nil)
	}
	return TokenSet{AccessToken: payload.AccessToken, RefreshToken: payload.RefreshToken, IDToken: payload.IDToken}, nil
}

func classifyHTTP(status int, body []byte) error {
	if status == http.StatusTooManyRequests {
		return newTypedError(ErrorRateLimited, status, "http_429", EvidenceContractVerifiedLivePending, true, false, true, nil)
	}
	if status >= 500 || status == 0 {
		return newTypedError(ErrorUpstreamTransient, status, "upstream_5xx", EvidenceContractVerifiedLivePending, true, false, true, nil)
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && (trimmed[0] == '<' || !json.Valid(trimmed)) {
		return newTypedError(ErrorContractChanged, status, "unexpected_non_json", EvidenceContractVerifiedLivePending, false, false, true, nil)
	}
	code := upstreamErrorCode(trimmed)
	switch strings.ToLower(code) {
	case "account_disabled", "account_deactivated":
		return newTypedError(ErrorAccountDisabled, status, code, EvidenceContractVerifiedLivePending, false, true, true, nil)
	case "token_revoked", "credential_revoked", "refresh_token_reused":
		return newTypedError(ErrorCredentialRevoked, status, code, EvidenceContractVerifiedLivePending, false, true, true, nil)
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return newTypedError(ErrorPermissionDenied, status, "http_"+strconv.Itoa(status), EvidenceContractVerifiedLivePending, false, false, true, nil)
	}
	return newTypedError(ErrorContractChanged, status, "unexpected_http_"+strconv.Itoa(status), EvidenceUnverified, false, false, true, nil)
}

func upstreamErrorCode(body []byte) string {
	var payload struct {
		Code  string `json:"code"`
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &payload) != nil {
		return ""
	}
	if payload.Error.Code != "" {
		return payload.Error.Code
	}
	if payload.Error.Type != "" {
		return payload.Error.Type
	}
	return payload.Code
}

func stateForError(kind ErrorKind) AccountState {
	switch kind {
	case ErrorCredentialRevoked:
		return StateCredentialRevoked
	case ErrorAccountDisabled:
		return StateAccountDisabled
	case ErrorPermissionDenied:
		return StatePermissionDenied
	case ErrorRateLimited:
		return StateRateLimited
	case ErrorUpstreamTransient:
		return StateUpstreamTransient
	default:
		return StateContractChanged
	}
}

func (c *Client) StartDeviceAuthorization(ctx context.Context) (DeviceAuthorization, error) {
	body, _ := json.Marshal(map[string]string{"client_id": c.clientID})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.deviceStartURL, bytes.NewReader(body))
	if err != nil {
		return DeviceAuthorization{}, transient("device_start_create", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	status, response, _, doErr := c.do(req)
	if doErr != nil {
		return DeviceAuthorization{}, doErr
	}
	if status < 200 || status >= 300 {
		return DeviceAuthorization{}, classifyHTTP(status, response)
	}
	var payload struct {
		DeviceAuthID string          `json:"device_auth_id"`
		UserCode     string          `json:"user_code"`
		UserCodeAlt  string          `json:"usercode"`
		Interval     json.RawMessage `json:"interval"`
		ExpiresIn    json.RawMessage `json:"expires_in"`
	}
	if json.Unmarshal(response, &payload) != nil {
		return DeviceAuthorization{}, newTypedError(ErrorContractChanged, status, "device_start_non_json", EvidenceUnverified, false, false, true, nil)
	}
	if payload.UserCode == "" {
		payload.UserCode = payload.UserCodeAlt
	}
	if payload.DeviceAuthID == "" || payload.UserCode == "" {
		return DeviceAuthorization{}, newTypedError(ErrorContractChanged, status, "device_start_fields_missing", EvidenceUnverified, false, false, true, nil)
	}
	interval := 5 * time.Second
	if parsed, err := parseTimeSeconds(payload.Interval); err == nil && parsed > 0 {
		interval = parsed
	}
	expiresIn := 15 * time.Minute
	if parsed, err := parseTimeSeconds(payload.ExpiresIn); err == nil && parsed > 0 && parsed <= time.Hour {
		expiresIn = parsed
	}
	return DeviceAuthorization{DeviceAuthID: payload.DeviceAuthID, UserCode: payload.UserCode, VerifyURL: c.deviceVerifyURL, Interval: interval, ExpiresAt: c.now().Add(expiresIn)}, nil
}

func parseTimeSeconds(raw json.RawMessage) (time.Duration, error) {
	var seconds int
	if json.Unmarshal(raw, &seconds) == nil {
		return time.Duration(seconds) * time.Second, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		value, err := strconv.Atoi(text)
		return time.Duration(value) * time.Second, err
	}
	return 0, fmt.Errorf("invalid interval")
}

func (c *Client) PollDeviceAuthorization(ctx context.Context, auth DeviceAuthorization) (TokenSet, bool, error) {
	result, err := c.PollDeviceAuthorizationResult(ctx, auth)
	if err != nil {
		return TokenSet{}, false, err
	}
	return result.Tokens, result.State != DevicePollAuthorized, nil
}
