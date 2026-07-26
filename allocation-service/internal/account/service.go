package account

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"allocation-service/internal/models"
	"allocation-service/internal/repository"
	"allocation-service/monitorfacade"
)

var (
	ErrValidation = errors.New("account validation failed")
	ErrNotFound   = errors.New("account not found")
)

type Repository interface {
	CreateAccount(context.Context, repository.AccountSeed) (int64, error)
	UpsertSyncedAccount(context.Context, repository.SyncedAccount) (models.Account, bool, error)
	UpdateAccount(context.Context, int64, repository.AccountUpdate) (models.Account, error)
	UpdateAccountMonitorStatus(context.Context, int64, string, string) (models.Account, error)
	DeleteAccount(context.Context, int64) error
	Account(context.Context, int64) (models.Account, error)
	ListAccounts(context.Context) ([]models.Account, error)
	AccountCapacitySettings(context.Context) (repository.AccountCapacitySettings, error)
	SetDefaultAccountCapacity(context.Context, int) (repository.AccountCapacitySettings, error)
	ApplyDefaultAccountCapacity(context.Context) (repository.ApplyDefaultCapacityResult, error)
	CreateMonitorSyncRun(context.Context, int) (int64, error)
	FinishMonitorSyncRun(context.Context, int64, string, int, int, string) error
	LatestMonitorSyncRun(context.Context) (repository.MonitorSyncRun, error)
}

type MonitorImporter interface {
	ImportForAllocation(context.Context, monitorfacade.ImportRequest) (monitorfacade.ImportResult, error)
	ListAccounts(context.Context) ([]monitorfacade.StatusResult, error)
	Status(context.Context, string) (monitorfacade.ImportResult, error)
	BatchStatus(context.Context, []string) (map[string]monitorfacade.StatusResult, error)
}

type Service struct {
	repo    Repository
	monitor MonitorImporter
	now     func() time.Time
}

type CreateInput struct {
	DisplayUsername    string
	DisplayPassword    string
	DisplayTOTPSecret  string
	AccountExpiry      time.Time
	MaxConcurrentUsers int
	SyncMonitor        bool
	MonitorToken       string
	MonitorTokenType   string
}

type UpdateInput struct {
	DisplayUsername    string
	DisplayPassword    string
	DisplayTOTPSecret  string
	AccountExpiry      time.Time
	MaxConcurrentUsers int
	Status             string
	MonitorStatus      string
	MonitorAccountID   string
}

type CreateResult struct {
	Account  models.Account
	Warnings []string
}

type SyncResult struct {
	Account  models.Account
	Warnings []string
}

type ListResult struct {
	Accounts []models.Account
	Warnings []string
}

type BatchSyncResult struct {
	Accounts []models.Account
	Warnings []string
	Total    int
	OK       int
	Failed   int
}

type PullSyncResult struct {
	Accounts []models.Account
	Created  int
	Updated  int
}

func NewService(repo Repository, monitor MonitorImporter) *Service {
	return &Service{repo: repo, monitor: monitor, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) SetNow(now func() time.Time) {
	if now != nil {
		s.now = now
	}
}

func (s *Service) Create(ctx context.Context, input CreateInput) (CreateResult, error) {
	if err := validateCreateInput(s.now().UTC(), input); err != nil {
		return CreateResult{}, err
	}
	result, err := s.importMonitor(ctx, input)
	if err != nil {
		return CreateResult{}, err
	}
	if strings.TrimSpace(result.MonitorAccountID) == "" || result.AccountExpiry.IsZero() || result.AccountExpiry.Before(s.now().UTC()) {
		return CreateResult{}, monitorfacade.ErrUnavailable
	}
	capacity := input.MaxConcurrentUsers
	if capacity == 0 {
		settings, err := s.repo.AccountCapacitySettings(ctx)
		if err != nil {
			return CreateResult{}, err
		}
		capacity = settings.DefaultAccountCapacity
	}
	displayUsername := firstNonEmpty(result.Email, strings.TrimSpace(input.DisplayUsername), result.MonitorAccountID)
	seed := repository.AccountSeed{
		DisplayUsername:    displayUsername,
		DisplayPassword:    input.DisplayPassword,
		DisplayTOTPSecret:  input.DisplayTOTPSecret,
		AccountExpiry:      result.AccountExpiry,
		MaxConcurrentUsers: capacity,
		Status:             "available",
		MonitorStatus:      normalizeMonitorStatus(defaultString(result.MonitorStatus, "unknown")),
		MonitorAccountID:   result.MonitorAccountID,
	}
	id, err := s.repo.CreateAccount(ctx, seed)
	if err != nil {
		return CreateResult{}, err
	}
	account, err := s.repo.Account(ctx, id)
	if err != nil {
		return CreateResult{}, err
	}
	return CreateResult{Account: account}, nil
}

func (s *Service) PullFromMonitor(ctx context.Context) (PullSyncResult, error) {
	if s.monitor == nil {
		return PullSyncResult{}, monitorfacade.ErrUnavailable
	}
	items, err := s.monitor.ListAccounts(ctx)
	if err != nil {
		return PullSyncResult{}, err
	}
	accounts := make([]models.Account, 0, len(items))
	created := 0
	updated := 0
	for _, item := range items {
		if strings.TrimSpace(item.MonitorAccountID) == "" || item.AccountExpiry.IsZero() {
			return PullSyncResult{}, monitorfacade.NewFault(monitorfacade.FaultContractChanged)
		}
		account, wasCreated, err := s.repo.UpsertSyncedAccount(ctx, repository.SyncedAccount{
			MonitorAccountID: item.MonitorAccountID,
			DisplayUsername:  firstNonEmpty(item.Email, item.MonitorAccountID),
			AccountExpiry:    item.AccountExpiry,
			MonitorStatus:    normalizeMonitorStatus(item.MonitorStatus),
		})
		if err != nil {
			return PullSyncResult{}, err
		}
		if wasCreated {
			created++
		} else {
			updated++
		}
		accounts = append(accounts, account)
	}
	return PullSyncResult{Accounts: accounts, Created: created, Updated: updated}, nil
}

func (s *Service) CapacitySettings(ctx context.Context) (repository.AccountCapacitySettings, error) {
	return s.repo.AccountCapacitySettings(ctx)
}

func (s *Service) SetDefaultCapacity(ctx context.Context, capacity int) (repository.AccountCapacitySettings, error) {
	if capacity < 1 || capacity > repository.MaxAccountCapacity {
		return repository.AccountCapacitySettings{}, ErrValidation
	}
	return s.repo.SetDefaultAccountCapacity(ctx, capacity)
}

func (s *Service) ApplyDefaultCapacity(ctx context.Context) (repository.ApplyDefaultCapacityResult, error) {
	return s.repo.ApplyDefaultAccountCapacity(ctx)
}

func (s *Service) Update(ctx context.Context, id int64, input UpdateInput) (models.Account, error) {
	if id <= 0 || strings.TrimSpace(input.DisplayUsername) == "" || input.MaxConcurrentUsers < 1 {
		return models.Account{}, ErrValidation
	}
	if !validAccountStatus(input.Status) || !validMonitorStatus(input.MonitorStatus) {
		return models.Account{}, ErrValidation
	}
	if input.AccountExpiry.IsZero() || input.AccountExpiry.Before(s.now().UTC()) {
		return models.Account{}, repository.ErrAccountExpiryTooLong
	}
	current, err := s.repo.Account(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return models.Account{}, ErrNotFound
	}
	if err != nil {
		return models.Account{}, err
	}
	status := input.Status
	if current.Status == "pending_credentials" {
		if strings.TrimSpace(input.DisplayPassword) == "" || strings.TrimSpace(input.DisplayTOTPSecret) == "" {
			status = "pending_credentials"
		} else if status == "pending_credentials" {
			status = "available"
		}
	}
	account, err := s.repo.UpdateAccount(ctx, id, repository.AccountUpdate{
		DisplayUsername:    strings.TrimSpace(input.DisplayUsername),
		DisplayPassword:    input.DisplayPassword,
		DisplayTOTPSecret:  input.DisplayTOTPSecret,
		AccountExpiry:      input.AccountExpiry,
		MaxConcurrentUsers: input.MaxConcurrentUsers,
		Status:             status,
		MonitorStatus:      input.MonitorStatus,
		MonitorAccountID:   input.MonitorAccountID,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return models.Account{}, ErrNotFound
	}
	return account, err
}

func (s *Service) Delete(ctx context.Context, id int64) error {
	if id <= 0 {
		return ErrValidation
	}
	err := s.repo.DeleteAccount(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return err
}

func (s *Service) SyncStatus(ctx context.Context, id int64) (SyncResult, error) {
	account, err := s.Get(ctx, id)
	if err != nil {
		return SyncResult{}, err
	}
	if s.monitor == nil || strings.TrimSpace(account.MonitorAccountID) == "" {
		account, updateErr := s.markMonitorUnknown(ctx, account)
		if updateErr != nil {
			return SyncResult{}, updateErr
		}
		return SyncResult{Account: account, Warnings: []string{"phase_one_monitor_unavailable"}}, nil
	}
	result, err := s.monitor.Status(ctx, account.MonitorAccountID)
	if err != nil {
		account, updateErr := s.markMonitorUnknown(ctx, account)
		if updateErr != nil {
			return SyncResult{}, updateErr
		}
		return SyncResult{Account: account, Warnings: []string{"phase_one_monitor_unavailable"}}, nil
	}
	account, err = s.repo.UpdateAccountMonitorStatus(ctx, id, defaultString(result.MonitorAccountID, account.MonitorAccountID), normalizeMonitorStatus(result.MonitorStatus))
	if errors.Is(err, sql.ErrNoRows) {
		return SyncResult{}, ErrNotFound
	}
	if err != nil {
		return SyncResult{}, err
	}
	return SyncResult{Account: account}, nil
}

func (s *Service) SyncAll(ctx context.Context) (BatchSyncResult, error) {
	accounts, err := s.repo.ListAccounts(ctx)
	if err != nil {
		return BatchSyncResult{}, err
	}
	targets := make([]models.Account, 0, len(accounts))
	ids := make([]string, 0, len(accounts))
	for _, account := range accounts {
		if strings.TrimSpace(account.MonitorAccountID) == "" {
			continue
		}
		targets = append(targets, account)
		ids = append(ids, account.MonitorAccountID)
	}
	runID, err := s.repo.CreateMonitorSyncRun(ctx, len(targets))
	if err != nil {
		return BatchSyncResult{}, err
	}
	if len(targets) == 0 {
		if err := s.repo.FinishMonitorSyncRun(ctx, runID, "completed", 0, 0, ""); err != nil {
			return BatchSyncResult{}, err
		}
		return BatchSyncResult{Accounts: accounts}, nil
	}
	if s.monitor == nil {
		updated, updateErr := s.markManyMonitorUnknown(ctx, targets)
		if updateErr != nil {
			return BatchSyncResult{}, updateErr
		}
		_ = s.repo.FinishMonitorSyncRun(ctx, runID, "failed", 0, len(targets), "phase_one_monitor_unavailable")
		return BatchSyncResult{Accounts: updated, Warnings: []string{"phase_one_monitor_unavailable"}, Total: len(targets), Failed: len(targets)}, nil
	}
	results, err := s.monitor.BatchStatus(ctx, ids)
	if err != nil {
		updated, updateErr := s.markManyMonitorUnknown(ctx, targets)
		if updateErr != nil {
			return BatchSyncResult{}, updateErr
		}
		_ = s.repo.FinishMonitorSyncRun(ctx, runID, "failed", 0, len(targets), "phase_one_monitor_unavailable")
		return BatchSyncResult{Accounts: updated, Warnings: []string{"phase_one_monitor_unavailable"}, Total: len(targets), Failed: len(targets)}, nil
	}
	updated := make([]models.Account, 0, len(targets))
	okCount := 0
	failedCount := 0
	for _, account := range targets {
		result, ok := results[account.MonitorAccountID]
		status := "unknown_monitor"
		monitorID := account.MonitorAccountID
		if ok {
			status = normalizeMonitorStatus(result.MonitorStatus)
			monitorID = defaultString(result.MonitorAccountID, account.MonitorAccountID)
			okCount++
		} else {
			failedCount++
		}
		next, err := s.repo.UpdateAccountMonitorStatus(ctx, account.ID, monitorID, status)
		if errors.Is(err, sql.ErrNoRows) {
			return BatchSyncResult{}, ErrNotFound
		}
		if err != nil {
			return BatchSyncResult{}, err
		}
		updated = append(updated, next)
	}
	state := "completed"
	errorCode := ""
	if failedCount > 0 {
		errorCode = "partial_monitor_unknown"
	}
	if err := s.repo.FinishMonitorSyncRun(ctx, runID, state, okCount, failedCount, errorCode); err != nil {
		return BatchSyncResult{}, err
	}
	warnings := []string(nil)
	if failedCount > 0 {
		warnings = append(warnings, "phase_one_monitor_partial")
	}
	return BatchSyncResult{Accounts: updated, Warnings: warnings, Total: len(targets), OK: okCount, Failed: failedCount}, nil
}

func (s *Service) Get(ctx context.Context, id int64) (models.Account, error) {
	if id <= 0 {
		return models.Account{}, ErrValidation
	}
	account, err := s.repo.Account(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return models.Account{}, ErrNotFound
	}
	return account, err
}

func (s *Service) List(ctx context.Context) ([]models.Account, error) {
	return s.repo.ListAccounts(ctx)
}

func (s *Service) ListWithWarnings(ctx context.Context) (ListResult, error) {
	accounts, err := s.repo.ListAccounts(ctx)
	if err != nil {
		return ListResult{}, err
	}
	var warnings []string
	run, err := s.repo.LatestMonitorSyncRun(ctx)
	if err == nil && run.State == "failed" {
		warnings = append(warnings, "phase_one_monitor_unavailable")
	}
	return ListResult{Accounts: accounts, Warnings: warnings}, nil
}

func (s *Service) importMonitor(ctx context.Context, input CreateInput) (monitorfacade.ImportResult, error) {
	if s.monitor == nil {
		return monitorfacade.ImportResult{}, monitorfacade.ErrUnavailable
	}
	tokenType := defaultString(input.MonitorTokenType, "session_token")
	return s.monitor.ImportForAllocation(ctx, monitorfacade.ImportRequest{
		Token:     input.MonitorToken,
		TokenType: tokenType,
		Label:     input.DisplayUsername,
	})
}

func validateCreateInput(now time.Time, input CreateInput) error {
	if input.DisplayPassword == "" || input.DisplayTOTPSecret == "" || strings.TrimSpace(input.MonitorToken) == "" || input.MaxConcurrentUsers < 0 || input.MaxConcurrentUsers > repository.MaxAccountCapacity {
		return ErrValidation
	}
	_ = now
	return nil
}

func validAccountStatus(status string) bool {
	switch status {
	case "available", "pending_credentials", "unknown_monitor", "full", "expired", "banned", "disabled":
		return true
	default:
		return false
	}
}

func validMonitorStatus(status string) bool {
	switch status {
	case "alive", "unknown", "dead_normal", "dead_banned", "unknown_monitor", "not_found":
		return true
	default:
		return false
	}
}

func (s *Service) markMonitorUnknown(ctx context.Context, account models.Account) (models.Account, error) {
	next, err := s.repo.UpdateAccountMonitorStatus(ctx, account.ID, account.MonitorAccountID, "unknown_monitor")
	if errors.Is(err, sql.ErrNoRows) {
		return models.Account{}, ErrNotFound
	}
	if err != nil {
		return models.Account{}, err
	}
	return next, nil
}

func (s *Service) markManyMonitorUnknown(ctx context.Context, accounts []models.Account) ([]models.Account, error) {
	updated := make([]models.Account, 0, len(accounts))
	for _, account := range accounts {
		next, err := s.markMonitorUnknown(ctx, account)
		if err != nil {
			return nil, err
		}
		updated = append(updated, next)
	}
	return updated, nil
}

func normalizeMonitorStatus(status string) string {
	switch status {
	case "alive", "unknown", "dead_normal", "dead_banned", "not_found", "unknown_monitor":
		return status
	default:
		return "unknown"
	}
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
