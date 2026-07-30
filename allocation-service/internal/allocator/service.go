package allocator

import (
	"context"
	"database/sql"
	"errors"
	"time"

	cardsvc "allocation-service/internal/card"
	"allocation-service/internal/models"
	"allocation-service/internal/repository"
)

var (
	ErrInvalidCode = errors.New("invalid card code")
	ErrNotFound    = errors.New("card not found")
	ErrUnavailable = errors.New("card is not redeemable")
	ErrNoCapacity  = errors.New("no account capacity")
)

type Repository interface {
	RedeemCode(context.Context, []byte, bool) (repository.RedeemResult, error)
	ListActiveAllocations(context.Context) ([]repository.AdminAllocationView, error)
	Audit(context.Context, string, string, int64, map[string]any) error
}

type MonitorAvailability interface {
	Available(context.Context) bool
}

type Service struct {
	repo    Repository
	monitor MonitorAvailability
	now     func() time.Time
}

type RedeemResult struct {
	Allocation models.Allocation
	Card       models.Card
	Account    models.Account
	Warnings   []string
	Elapsed    time.Duration
}

type AdminAllocation struct {
	Allocation models.Allocation
	Card       models.Card
	Account    models.Account
}

func NewService(repo Repository, monitor MonitorAvailability) *Service {
	return &Service{repo: repo, monitor: monitor, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) SetNow(now func() time.Time) {
	if now != nil {
		s.now = now
	}
}

func (s *Service) Redeem(ctx context.Context, code string) (RedeemResult, error) {
	started := s.now()
	if !cardsvc.ValidCode(code) {
		return RedeemResult{}, ErrInvalidCode
	}
	monitorAvailable := s.monitor == nil || s.monitor.Available(ctx)
	warnings := []string(nil)
	if !monitorAvailable {
		warnings = append(warnings, "monitor_unavailable")
	}
	result, err := s.repo.RedeemCode(ctx, cardsvc.HashCode(code), monitorAvailable)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return RedeemResult{}, ErrNotFound
	case errors.Is(err, repository.ErrCardStateConflict):
		return RedeemResult{}, ErrUnavailable
	case errors.Is(err, repository.ErrNoAccountCapacity):
		return RedeemResult{}, ErrNoCapacity
	case err != nil:
		return RedeemResult{}, err
	}
	_ = s.repo.Audit(ctx, "cards.redeem", "card", result.Card.ID, map[string]any{"account_id": result.Account.ID, "monitor_available": monitorAvailable})
	return RedeemResult{Allocation: result.Allocation, Card: result.Card, Account: result.Account, Warnings: warnings, Elapsed: s.now().Sub(started)}, nil
}

func (s *Service) ListActive(ctx context.Context) ([]AdminAllocation, error) {
	views, err := s.repo.ListActiveAllocations(ctx)
	if err != nil {
		return nil, err
	}
	allocations := make([]AdminAllocation, 0, len(views))
	for _, view := range views {
		allocations = append(allocations, AdminAllocation{
			Allocation: view.Allocation,
			Card:       view.Card,
			Account:    view.Account,
		})
	}
	return allocations, nil
}
