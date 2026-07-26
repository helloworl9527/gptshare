package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	allocationmodule "allocation-service/module"
	"allocation-service/platform/supervisor"
	"chatgpt-monitor/internal/account"
	"chatgpt-monitor/internal/auth"
	"chatgpt-monitor/internal/chatgpt"
	credentialcrypto "chatgpt-monitor/internal/crypto"
	"chatgpt-monitor/internal/httpapi"
	"chatgpt-monitor/internal/monitor"
	"chatgpt-monitor/internal/monitorfacade"
	"chatgpt-monitor/internal/notify"
	"chatgpt-monitor/internal/store"
	"chatgpt-monitor/internal/unifiedui"
	"chatgpt-monitor/internal/vitalsapp"
	"chatgpt-monitor/internal/vitalsconfig"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("vitals stopped", "error_code", errorCode(err))
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := vitalsconfig.Load(os.LookupEnv)
	if err != nil {
		return errors.New("configuration_rejected")
	}
	monitorKeyring, err := credentialcrypto.NewKeyring(cfg.MonitorCredentialKeys, cfg.MonitorCredentialActiveID)
	if err != nil {
		return errors.New("monitor_keyring_invalid")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serviceLock, err := monitor.AcquireServiceLock(cfg.MonitorDBPath)
	if err != nil {
		return errors.New("monitor_service_lock_failed")
	}
	defer serviceLock.Close()

	monitorStore, err := store.Open(ctx, cfg.MonitorDBPath, os.DirFS(cfg.MonitorMigrationsDir))
	if err != nil {
		return errors.New("monitor_database_initialization_failed")
	}
	defer monitorStore.Close()
	unifiedAuth, err := auth.New(auth.Config{
		DB:           monitorStore.DB(),
		Username:     cfg.AdminUser,
		PasswordHash: cfg.AdminPasswordHash,
		TOTPSecret:   cfg.AdminTOTPSecret,
		JWTKey:       cfg.JWTSigningKey,
		RateLimitKey: cfg.RateLimitKey,
	})
	if err != nil {
		return errors.New("unified_auth_initialization_failed")
	}

	chatgptClient := chatgpt.NewClient(chatgpt.Config{EvidenceLevel: chatgpt.EvidenceLiveVerified})
	accountService, err := account.NewService(monitorStore.DB(), chatgptClient, monitorKeyring)
	if err != nil {
		return errors.New("monitor_account_initialization_failed")
	}
	monitorService, err := monitor.New(monitorStore.DB(), chatgptClient, monitorKeyring, monitor.DefaultConfig())
	if err != nil {
		return errors.New("monitor_initialization_failed")
	}
	if err := monitorService.RecoverInterrupted(ctx); err != nil {
		return errors.New("monitor_recovery_failed")
	}
	settingsService, err := notify.NewSettingsService(monitorStore.DB(), monitorKeyring)
	if err != nil {
		return errors.New("notification_settings_initialization_failed")
	}
	outbox, err := notify.NewConsumer(monitorStore.DB(), notify.NewLogNotifier(logger))
	if err != nil {
		return errors.New("notification_outbox_initialization_failed")
	}
	facade, err := monitorfacade.New(accountService)
	if err != nil {
		return errors.New("monitor_facade_initialization_failed")
	}
	allocationModule, err := allocationmodule.Open(ctx, cfg.AllocationDBPath, cfg.AllocationCredentialKeys, cfg.AllocationCredentialActiveID, facade, logger)
	if err != nil {
		return errors.New("allocation_database_initialization_failed")
	}
	defer allocationModule.Close()

	runner := supervisor.New(logger, supervisor.Config{PanicThreshold: 3})
	replacementInterval, err := testReplacementInterval()
	if err != nil {
		return err
	}
	if err := runner.GoManaged(ctx, "poller", "monitor", injectPanic("poller", func(taskCtx context.Context) error {
		return monitorService.RunScheduler(taskCtx, logger)
	})); err != nil {
		return errors.New("monitor_poller_registration_failed")
	}
	if err := runner.GoManaged(ctx, "outbox", "monitor", injectPanic("outbox", outbox.Run)); err != nil {
		return errors.New("monitor_outbox_registration_failed")
	}
	if err := runner.GoManaged(ctx, "replacement", "allocation", injectPanic("replacement", func(taskCtx context.Context) error {
		allocationModule.RunReplacementEvery(taskCtx, replacementInterval)
		return taskCtx.Err()
	})); err != nil {
		return errors.New("allocation_replacement_registration_failed")
	}
	if err := runner.GoManaged(ctx, "facade-sync", "allocation", injectPanic("facade-sync", func(taskCtx context.Context) error {
		stagger := time.NewTimer(5 * time.Minute)
		defer stagger.Stop()
		select {
		case <-taskCtx.Done():
			return taskCtx.Err()
		case <-stagger.C:
		}
		return allocationModule.RunFacadeSync(taskCtx, time.Hour)
	})); err != nil {
		return errors.New("allocation_facade_sync_registration_failed")
	}

	app := vitalsapp.New(monitorStore, allocationModule, runner, allocationModule, logger)
	unifiedui.Register(app.Engine())
	httpapi.RegisterUnifiedAdminRoutes(app.Engine(), unifiedAuth, accountService, httpapi.Config{
		Origin:             cfg.AppOrigin,
		TrustLoopbackProxy: cfg.TrustLoopbackProxy,
		Monitor:            monitorService,
		Settings:           settingsService,
	})
	boundary := httpapi.UnifiedAdminBoundary(unifiedAuth, cfg.AppOrigin)
	if err := allocationModule.RegisterAdminRoutes(app.Engine(), allocationmodule.AdminBoundary{
		RequireSession: boundary.RequireSession,
		RequireCSRF:    boundary.RequireCSRF,
		RequireOrigin:  boundary.RequireOrigin,
	}); err != nil {
		return errors.New("allocation_admin_routes_registration_failed")
	}
	if cfg.CompatHTTP.Enabled {
		limiter := vitalsapp.NewCompatLimiter(60)
		httpapi.RegisterAllocationCompatibilityRoutes(app.Engine(), accountService, httpapi.Config{AllocationServiceAPIKey: cfg.CompatHTTP.APIKey}, logger,
			limiter.Middleware(logger, cfg.CompatHTTP.Consumer, cfg.CompatHTTP.ExpiresAt))
	}

	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      25 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	errCh := make(chan error, 1)
	go serveHTTP(server, logger, errCh)

	select {
	case err := <-errCh:
		stop()
		if !errors.Is(err, http.ErrServerClosed) {
			return errors.New("http_server_failed")
		}
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return errors.New("http_server_shutdown_failed")
	}
	runner.Wait()
	if err := monitorService.Wait(shutdownCtx); err != nil {
		return errors.New("monitor_shutdown_timeout")
	}
	logger.Info("vitals stopped cleanly")
	return nil
}

func serveHTTP(server *http.Server, logger *slog.Logger, errCh chan<- error) {
	logger.Info("vitals started", "address", server.Addr)
	errCh <- server.ListenAndServe()
}

func injectPanic(task string, fn supervisor.TaskFunc) supervisor.TaskFunc {
	configured := os.Getenv("VITALS_TEST_PANIC_TASK")
	var injected atomic.Bool
	return func(ctx context.Context) error {
		if configured == task || (configured == task+":once" && injected.CompareAndSwap(false, true)) {
			panic(struct{ Task string }{Task: task})
		}
		return fn(ctx)
	}
}

func testReplacementInterval() (time.Duration, error) {
	raw := os.Getenv("VITALS_TEST_REPLACEMENT_INTERVAL")
	if raw == "" {
		return time.Hour, nil
	}
	interval, err := time.ParseDuration(raw)
	if err != nil || interval <= 0 {
		return 0, errors.New("test_replacement_interval_invalid")
	}
	return interval, nil
}

func errorCode(err error) string {
	if err == nil || err.Error() == "" {
		return "vitals_failed"
	}
	return err.Error()
}
