package allocator

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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
	// ErrBindingReplayed 表示绑定票据的 nonce 已被消耗过。
	ErrBindingReplayed = errors.New("bind ticket replayed")
)

type Repository interface {
	RedeemCodeWithBinding(context.Context, []byte, bool, *repository.RedemptionBinding) (repository.RedeemResult, error)
	ListActiveAllocations(context.Context) ([]repository.AdminAllocationView, error)
	ListReplacementHistory(context.Context, int) ([]repository.ReplacementHistoryEntry, error)
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

// Binding 是公开兑换请求上已通过签名与时效校验的绑定票据。
type Binding struct {
	UserUID string
	Nonce   string
	Exp     int64
}

func (s *Service) Redeem(ctx context.Context, code string) (RedeemResult, error) {
	return s.RedeemWithBinding(ctx, code, nil)
}

// RedeemWithBinding 在 binding 非空时把 nonce 与兑换事件写进扣码的同一事务；
// 卡密已兑换、只返回原结果时不写事件、不消耗 nonce。
func (s *Service) RedeemWithBinding(ctx context.Context, code string, binding *Binding) (RedeemResult, error) {
	started := s.now()
	if !cardsvc.ValidCode(code) {
		return RedeemResult{}, ErrInvalidCode
	}
	monitorAvailable := s.monitor == nil || s.monitor.Available(ctx)
	warnings := []string(nil)
	if !monitorAvailable {
		warnings = append(warnings, "monitor_unavailable")
	}
	var redemptionBinding *repository.RedemptionBinding
	if binding != nil {
		sum := sha256.Sum256([]byte(code))
		redemptionBinding = &repository.RedemptionBinding{
			UserUID:     binding.UserUID,
			Nonce:       binding.Nonce,
			NonceExp:    binding.Exp,
			CodeHashHex:   hex.EncodeToString(sum[:]),
			CodePlaintext: code,
		}
	}
	result, err := s.repo.RedeemCodeWithBinding(ctx, cardsvc.HashCode(code), monitorAvailable, redemptionBinding)
	switch {
	case errors.Is(err, repository.ErrBindNonceReused):
		return RedeemResult{}, ErrBindingReplayed
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

// ListReplacementHistory 返回后台"替换历史"表格所需的换号流水。
func (s *Service) ListReplacementHistory(ctx context.Context, limit int) ([]repository.ReplacementHistoryEntry, error) {
	return s.repo.ListReplacementHistory(ctx, limit)
}
