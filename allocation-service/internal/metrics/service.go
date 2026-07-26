package metrics

import (
	"context"
	"time"

	"allocation-service/internal/repository"
)

type Repository interface {
	InventoryMetrics(context.Context, time.Time) (repository.InventoryMetrics, error)
}

type Service struct {
	repo Repository
	now  func() time.Time
}

func NewService(repo Repository) *Service {
	return &Service{repo: repo, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) SetNow(now func() time.Time) {
	if now != nil {
		s.now = now
	}
}

func (s *Service) Dashboard(ctx context.Context) (repository.InventoryMetrics, error) {
	return s.repo.InventoryMetrics(ctx, s.now().UTC())
}
