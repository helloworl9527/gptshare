package main

import (
	"context"
	"errors"
	"log/slog"
	"os"

	allocationmodule "allocation-service/module"
	"chatgpt-monitor/internal/store"
	"chatgpt-monitor/internal/vitalsconfig"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	validateOnly := len(os.Args) == 2 && os.Args[1] == "--validate-only"
	if len(os.Args) > 1 && !validateOnly {
		logger.Error("migration failed", "error_code", "unsupported_argument")
		os.Exit(1)
	}
	if err := run(context.Background(), validateOnly); err != nil {
		logger.Error("migration failed", "error_code", err.Error())
		os.Exit(1)
	}
	logger.Info("migration complete", "monitor", "ok", "allocation", "ok")
}

func run(ctx context.Context, validateOnly bool) error {
	cfg, err := vitalsconfig.Load(os.LookupEnv)
	if err != nil {
		return errors.New("configuration_rejected")
	}
	if validateOnly {
		return nil
	}
	monitorDB, err := store.Open(ctx, cfg.MonitorDBPath, os.DirFS(cfg.MonitorMigrationsDir))
	if err != nil {
		return errors.New("monitor_migration_failed")
	}
	if err := monitorDB.Close(); err != nil {
		return errors.New("monitor_migration_close_failed")
	}
	if err := allocationmodule.Migrate(ctx, cfg.AllocationDBPath); err != nil {
		return errors.New("allocation_migration_failed")
	}
	return nil
}
