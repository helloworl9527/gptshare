package replacement

import (
	"context"
	"log/slog"
	"time"

	"allocation-service/internal/repository"
)

type Repository interface {
	ProcessReplacements(context.Context, time.Time) (repository.ReplacementRun, error)
	Audit(context.Context, string, string, int64, map[string]any) error
}

type Service struct {
	repo   Repository
	logger *slog.Logger
	now    func() time.Time
}

func NewService(repo Repository, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{repo: repo, logger: logger, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) SetNow(now func() time.Time) {
	if now != nil {
		s.now = now
	}
}

func (s *Service) RunOnce(ctx context.Context) (repository.ReplacementRun, error) {
	run, err := s.repo.ProcessReplacements(ctx, s.now().UTC())
	if err != nil {
		_ = s.repo.Audit(ctx, "replacement.run_failed", "replacement", 0, map[string]any{"error_code": "replacement_run_failed"})
		return repository.ReplacementRun{}, err
	}
	return run, nil
}

func (s *Service) StartHourly(ctx context.Context) {
	s.Start(ctx, time.Hour)
}

func (s *Service) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = time.Hour
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	// 启动即扫一次：到期卡的坑位不必再等满一个周期才释放。
	s.scanOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.scanOnce(ctx)
		}
	}
}

func (s *Service) scanOnce(ctx context.Context) {
	run, err := s.RunOnce(ctx)
	if err != nil {
		s.logger.Error("replacement scan failed", "error_code", "replacement_run_failed")
		return
	}
	if len(run.Replaced) == 0 && run.GraceExpired == 0 && run.Failed == 0 && len(run.Retired) == 0 &&
		run.CardsExpired == 0 && run.AvailabilityRestored == 0 && run.MarkedFull == 0 && run.AllocationCountsFixed == 0 {
		return
	}
	retiredBanned, retiredExpired := 0, 0
	for _, item := range run.Retired {
		if item.Reason == repository.RetireReasonBanned {
			retiredBanned++
			continue
		}
		retiredExpired++
	}
	s.logger.Info("replacement scan completed", "replaced", len(run.Replaced), "grace_expired", run.GraceExpired,
		"failed", run.Failed, "retired_banned", retiredBanned, "retired_expired", retiredExpired,
		"cards_expired", run.CardsExpired, "availability_restored", run.AvailabilityRestored,
		"marked_full", run.MarkedFull, "allocation_counts_fixed", run.AllocationCountsFixed)
}
