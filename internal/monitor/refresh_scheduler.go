package monitor

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	credentialcrypto "chatgpt-monitor/internal/crypto"
)

// RunRefreshScheduler is a dedicated background loop that rotates OAuth
// credentials before their access token expires, keeping the stored refresh
// token chain alive even when the liveness poll is not running (e.g. during
// the dead-account quiescent windows or between manual checks).
//
// Unlike the liveness poller it does not validate account status via
// accounts/check; it only refreshes and persists credentials.
func (s *Service) RunRefreshScheduler(ctx context.Context, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			s.runRefreshSweep(ctx)
			timer.Reset(s.cfg.RefreshScanEvery)
		}
	}
}

// runRefreshSweep scans all alive accounts whose credentials are approaching
// expiry and rotates them ahead of time. It never writes business state or
// alerts; authorization failures are left for the liveness poller to classify.
func (s *Service) runRefreshSweep(ctx context.Context) {
	accounts, err := s.loadDueAccounts(ctx, 0)
	if err != nil {
		return
	}
	type job struct {
		record accountRecord
	}
	jobs := make(chan job, len(accounts))
	var wg sync.WaitGroup
	for i := 0; i < s.cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobs {
				s.refreshAccountIfDue(ctx, item.record)
			}
		}()
	}
	for _, record := range accounts {
		select {
		case jobs <- job{record}:
		case <-ctx.Done():
			break
		}
	}
	close(jobs)
	wg.Wait()
}

// refreshAccountIfDue decrypts the credential payload, checks whether it needs
// rotation within the lead window, and persists the rotated set via CAS.
func (s *Service) refreshAccountIfDue(ctx context.Context, record accountRecord) {
	// Claim the account so a concurrent poll/refresh for the same account is
	// skipped. The claim (not the mutex) is what prevents races; release it
	// before network I/O so other accounts/work stay unblocked.
	s.mu.Lock()
	if _, active := s.active[record.ID]; active {
		s.mu.Unlock()
		return
	}
	s.active[record.ID] = "refresh"
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.active, record.ID)
		s.mu.Unlock()
	}()

	plaintext, err := s.cipher.Open(record.Envelope, credentialcrypto.CredentialAAD(record.ID, record.TokenType))
	if err != nil {
		return
	}
	defer zero(plaintext)
	var credentials credentialPayload
	if json.Unmarshal(plaintext, &credentials) != nil {
		return
	}
	if credentials.OAuthSource == "" {
		credentials.OAuthSource = legacyOAuthSource(record.TokenType)
	}
	now := s.now().UTC()
	// Rotate when the access token expires within the lead window. A wider
	// window (24h default) keeps the refresh chain warm without waiting for the
	// token to near-expire, mirroring CLIProxyAPI's RefreshLead semantics.
	_, _, outcome := s.refreshCredentialIfApproachingExpiry(ctx, record, credentials, now.Add(s.cfg.RefreshLead))
	if outcome.reloadCredential {
		// The credential changed under us (a manual reauthorize or allocator
		// mutation); the next sweep will see the new version.
		return
	}
	// Outcome is intentionally left unclassified here; authorization failures
	// surface during the regular liveness poll through applyResult.
	_ = outcome
}
