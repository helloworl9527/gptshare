package monitor

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"chatgpt-monitor/internal/chatgpt"
	credentialcrypto "chatgpt-monitor/internal/crypto"
)

type Service struct {
	db     *sql.DB
	client Client
	cipher Cipher
	cfg    Config
	now    func() time.Time
	sleep  func(context.Context, time.Duration) error

	roundMu sync.Mutex
	mu      sync.Mutex
	active  map[int64]string
	baseCtx context.Context
	wg      sync.WaitGroup
}

type credentialPayload struct {
	Access  string `json:"access,omitempty"`
	Refresh string `json:"refresh,omitempty"`
	Session string `json:"session,omitempty"`
}

type accountRecord struct {
	ID         int64
	ProviderID string
	TokenType  string
	Envelope   []byte
	AuthExpiry time.Time
	Status     string
	ImportTime time.Time
	LastCheck  string
	Paused     bool
	EpochID    int64
}

type pollResult struct {
	status   *chatgpt.StatusResult
	typed    *chatgpt.TypedError
	envelope []byte
	endpoint string
	code     string
}

func New(db *sql.DB, client Client, cipher Cipher, cfg Config) (*Service, error) {
	if db == nil || client == nil || cipher == nil {
		return nil, errors.New("monitor dependencies are required")
	}
	defaults := DefaultConfig()
	if cfg.Workers == 0 {
		cfg.Workers = defaults.Workers
	}
	if cfg.RequestTimeout == 0 {
		cfg.RequestTimeout = defaults.RequestTimeout
	}
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = defaults.MaxRetries
	}
	if cfg.MinInterval == 0 {
		cfg.MinInterval = defaults.MinInterval
	}
	if cfg.DefaultInterval == 0 {
		cfg.DefaultInterval = defaults.DefaultInterval
	}
	if cfg.NearExpiryDays == 0 {
		cfg.NearExpiryDays = defaults.NearExpiryDays
	}
	if cfg.RefreshBefore == 0 {
		cfg.RefreshBefore = defaults.RefreshBefore
	}
	if cfg.ParserVersion == "" {
		cfg.ParserVersion = defaults.ParserVersion
	}
	if cfg.ManualWait == 0 {
		cfg.ManualWait = defaults.ManualWait
	}
	if cfg.Workers != 3 || cfg.RequestTimeout > 15*time.Second || cfg.MaxRetries > 2 || cfg.MinInterval < 15*time.Minute || cfg.DefaultInterval < cfg.MinInterval || cfg.ManualWait <= 0 || cfg.ManualWait > 20*time.Second {
		return nil, errors.New("monitor configuration violates resource limits")
	}
	service := &Service{db: db, client: client, cipher: cipher, cfg: cfg, now: time.Now, active: make(map[int64]string), baseCtx: context.Background()}
	service.sleep = sleepContext
	return service, nil
}

func (s *Service) SetBaseContext(ctx context.Context) {
	if ctx != nil {
		s.baseCtx = ctx
	}
}

func (s *Service) RecoverInterrupted(ctx context.Context) error {
	now := s.now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.ExecContext(ctx, `UPDATE poll_runs SET state='failed',finished_at=?,error_code='startup_interrupted'
		WHERE state='running'`, now); err != nil {
		return err
	}
	return s.finalizeLegacyPendingBans(ctx)
}

func (s *Service) RunScheduled(ctx context.Context) (Run, error) {
	if !s.roundMu.TryLock() {
		return s.recordSkipped(ctx)
	}
	defer s.roundMu.Unlock()
	accounts, err := s.loadDueAccounts(ctx, 0)
	if err != nil {
		return Run{}, err
	}
	if len(accounts) == 0 {
		return Run{}, nil
	}
	run, err := s.createRun(ctx, "scheduled", nil, len(accounts))
	if err != nil {
		return Run{}, err
	}
	s.mu.Lock()
	claimed := accounts[:0]
	for _, record := range accounts {
		if s.active[record.ID] == "" {
			s.active[record.ID] = run.ID
			claimed = append(claimed, record)
		}
	}
	s.mu.Unlock()
	interval := s.currentPollInterval(ctx)
	s.executeRun(ctx, &run, claimed, interval)
	s.mu.Lock()
	for _, record := range claimed {
		if s.active[record.ID] == run.ID {
			delete(s.active, record.ID)
		}
	}
	s.mu.Unlock()
	return s.GetRun(ctx, run.ID)
}

func (s *Service) RefreshNow(ctx context.Context, accountID int64) (Run, bool, error) {
	s.mu.Lock()
	if runID := s.active[accountID]; runID != "" {
		s.mu.Unlock()
		return Run{}, false, &ConflictError{RunID: runID}
	}
	accounts, err := s.loadDueAccounts(ctx, accountID)
	if err != nil {
		s.mu.Unlock()
		return Run{}, false, err
	}
	if len(accounts) == 0 {
		s.mu.Unlock()
		return Run{}, false, &NotFoundError{}
	}
	if accounts[0].Paused {
		s.mu.Unlock()
		return Run{}, false, &PausedError{}
	}
	run, err := s.createRun(ctx, "manual", &accountID, 1)
	if err != nil {
		s.mu.Unlock()
		return Run{}, false, err
	}
	s.active[accountID] = run.ID
	s.mu.Unlock()
	done := make(chan struct{})
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer close(done)
		s.executeRun(s.baseCtx, &run, accounts, s.currentPollInterval(context.Background()))
		s.mu.Lock()
		delete(s.active, accountID)
		s.mu.Unlock()
	}()
	timer := time.NewTimer(s.cfg.ManualWait)
	defer timer.Stop()
	select {
	case <-done:
		result, getErr := s.GetRun(context.Background(), run.ID)
		return result, true, getErr
	case <-timer.C:
		return run, false, nil
	case <-ctx.Done():
		return run, false, nil
	}
}

func (s *Service) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) executeRun(ctx context.Context, run *Run, accounts []accountRecord, interval time.Duration) {
	type result struct {
		ok   bool
		code string
	}
	jobs := make(chan accountRecord)
	results := make(chan result, len(accounts))
	var workers sync.WaitGroup
	for i := 0; i < s.cfg.Workers; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for record := range jobs {
				outcome := s.pollWithRetry(ctx, record)
				if err := s.applyResultWithBusyRetry(ctx, run.ID, record, outcome, interval); err != nil {
					results <- result{code: "state_update_failed"}
				} else if outcome.typed != nil {
					results <- result{code: outcome.code}
				} else {
					results <- result{ok: true}
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, account := range accounts {
			select {
			case jobs <- account:
			case <-ctx.Done():
				return
			}
		}
	}()
	workers.Wait()
	close(results)
	counts := make(map[string]int)
	ok, failed := 0, 0
	for item := range results {
		if item.ok {
			ok++
		} else {
			failed++
			counts[item.code]++
		}
	}
	skipped := run.AccountsTotal - ok - failed
	state, code := "completed", ""
	if ctx.Err() != nil {
		state, code = "cancelled", "shutdown_cancelled"
	}
	encoded, _ := json.Marshal(counts)
	_, _ = s.db.ExecContext(context.Background(), `UPDATE poll_runs SET finished_at=?,state=?,accounts_ok=?,accounts_failed=?,accounts_skipped=?,error_counts_json=?,error_code=? WHERE id=?`,
		s.now().UTC().Format(time.RFC3339Nano), state, ok, failed, skipped, string(encoded), nullable(code), run.ID)
}

func (s *Service) applyResultWithBusyRetry(ctx context.Context, runID string, record accountRecord, outcome pollResult, interval time.Duration) error {
	var err error
	for attempt := 0; attempt < 6; attempt++ {
		err = s.applyResult(ctx, runID, record, outcome, interval)
		if err == nil || (!strings.Contains(strings.ToLower(err.Error()), "locked") && !strings.Contains(strings.ToLower(err.Error()), "busy")) {
			return err
		}
		delay := time.Duration(1<<attempt) * 10 * time.Millisecond
		if delay > 250*time.Millisecond {
			delay = 250 * time.Millisecond
		}
		if sleepContext(ctx, delay) != nil {
			return err
		}
	}
	return err
}

func (s *Service) pollWithRetry(ctx context.Context, record accountRecord) pollResult {
	now := s.now().UTC()
	if !now.Before(record.AuthExpiry) {
		return pollResult{status: &chatgpt.StatusResult{AccountState: chatgpt.StateActive, EvidenceCode: "auth_expiry_reached", EvidenceLevel: chatgpt.EvidenceLiveVerified}, endpoint: "local_clock", code: "normal_expiry"}
	}
	plaintext, err := s.cipher.Open(record.Envelope, credentialcrypto.CredentialAAD(record.ID, record.TokenType))
	if err != nil {
		return internalPollError("credential_decrypt")
	}
	defer zero(plaintext)
	var credentials credentialPayload
	if json.Unmarshal(plaintext, &credentials) != nil {
		return internalPollError("credential_decode")
	}
	access := credentials.Access
	if accessNeedsRefresh(access, now.Add(s.cfg.RefreshBefore)) {
		var tokens chatgpt.TokenSet
		var exchangeErr error
		for attempt := 0; attempt <= s.cfg.MaxRetries; attempt++ {
			requestCtx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
			switch {
			case credentials.Refresh != "":
				tokens, exchangeErr = s.client.ExchangeCredential(requestCtx, chatgpt.CredentialRefresh, credentials.Refresh)
			case credentials.Session != "":
				tokens, exchangeErr = s.client.ExchangeCredential(requestCtx, chatgpt.CredentialSession, credentials.Session)
			default:
				cancel()
				return internalPollError("access_expired_no_refresh")
			}
			cancel()
			if exchangeErr == nil {
				break
			}
			outcome := errorPollResult("oauth_token", exchangeErr)
			if outcome.typed == nil || !outcome.typed.Retryable || attempt == s.cfg.MaxRetries || s.retryPause(ctx, outcome.typed, attempt) != nil {
				return outcome
			}
		}
		credentials.Access = tokens.AccessToken
		if tokens.RefreshToken != "" {
			credentials.Refresh = tokens.RefreshToken
		}
		access = tokens.AccessToken
	}
	var status chatgpt.StatusResult
	for attempt := 0; attempt <= s.cfg.MaxRetries; attempt++ {
		requestCtx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
		var statusErr error
		status, statusErr = s.client.FetchStatus(requestCtx, access)
		cancel()
		if statusErr == nil {
			break
		}
		outcome := errorPollResult("accounts_check", statusErr)
		if outcome.typed == nil || !outcome.typed.Retryable || attempt == s.cfg.MaxRetries || s.retryPause(ctx, outcome.typed, attempt) != nil {
			return outcome
		}
	}
	if status.ProviderAccountID != record.ProviderID {
		return pollResult{typed: &chatgpt.TypedError{Kind: chatgpt.ErrorContractChanged, EvidenceCode: "provider_account_mismatch", EvidenceLevel: chatgpt.EvidenceUnverified, PreserveBusinessState: true}, endpoint: "accounts_check", code: "provider_account_mismatch"}
	}
	encoded, err := json.Marshal(credentials)
	if err != nil {
		return internalPollError("credential_encode")
	}
	defer zero(encoded)
	envelope, err := s.cipher.Seal(encoded, credentialcrypto.CredentialAAD(record.ID, record.TokenType))
	if err != nil {
		return internalPollError("credential_encrypt")
	}
	credentials.Access, credentials.Refresh, credentials.Session = "", "", ""
	return pollResult{status: &status, envelope: envelope, endpoint: "accounts_check", code: status.EvidenceCode}
}

func (s *Service) retryPause(ctx context.Context, typed *chatgpt.TypedError, attempt int) error {
	delay := time.Duration(1<<attempt) * time.Second
	if typed.RetryAfter > delay {
		delay = typed.RetryAfter
	}
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	return s.sleep(ctx, delay)
}

func internalPollError(code string) pollResult {
	return pollResult{typed: &chatgpt.TypedError{Kind: chatgpt.ErrorUpstreamTransient, EvidenceCode: code, EvidenceLevel: chatgpt.EvidenceUnverified, Retryable: false, PreserveBusinessState: true}, endpoint: "local_monitor", code: code}
}

func errorPollResult(endpoint string, err error) pollResult {
	var typed *chatgpt.TypedError
	if errors.As(err, &typed) {
		return pollResult{typed: typed, endpoint: endpoint, code: typed.EvidenceCode}
	}
	return internalPollError("upstream_unknown")
}

func EvidenceSignature(code, parserVersion string) string {
	return EvidenceSignatureFor("accounts_check", code, parserVersion)
}

func EvidenceSignatureFor(endpoint, code, parserVersion string) string {
	canonical := "endpoint=" + strings.TrimSpace(endpoint) + "|code=" + strings.TrimSpace(code) + "|parser=" + strings.TrimSpace(parserVersion)
	sum := sha256.Sum256([]byte(canonical))
	return "ev1:" + hex.EncodeToString(sum[:])
}

func accessNeedsRefresh(token string, threshold time.Time) bool {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return true
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return true
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp == 0 {
		return true
	}
	return !time.Unix(claims.Exp, 0).After(threshold)
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func zero(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
func nullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func randomID() (string, error) {
	value := make([]byte, 24)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func parseTime(value string) (time.Time, error) { return time.Parse(time.RFC3339Nano, value) }
func formatTime(value time.Time) string         { return value.UTC().Format(time.RFC3339Nano) }
func pointer[T any](value T) *T                 { return &value }
