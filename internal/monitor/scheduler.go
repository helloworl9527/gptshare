package monitor

import (
	"context"
	"log/slog"
	"time"
)

// RunScheduler runs the scheduler in the caller's goroutine. The modular
// monolith passes this function to its managed background supervisor.
func (s *Service) RunScheduler(ctx context.Context, logger *slog.Logger) error {
	if logger == nil {
		logger = slog.Default()
	}
	s.SetBaseContext(ctx)
	timer := time.NewTimer(time.Minute)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			run, err := s.RunScheduled(ctx)
			if err != nil {
				logger.Error("scheduled poll failed", "error_code", "poll_round_failed")
			} else if run.ID != "" {
				logger.Info("scheduled poll finished", "run_id", run.ID, "state", run.State, "accounts_total", run.AccountsTotal, "accounts_ok", run.AccountsOK, "accounts_failed", run.AccountsFailed, "accounts_skipped", run.AccountsSkipped)
			}
			timer.Reset(time.Minute)
		}
	}
}
