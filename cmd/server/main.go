package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"allocation-service/platform/supervisor"
	"chatgpt-monitor/internal/account"
	"chatgpt-monitor/internal/allocationsync"
	"chatgpt-monitor/internal/auth"
	"chatgpt-monitor/internal/chatgpt"
	"chatgpt-monitor/internal/config"
	credentialcrypto "chatgpt-monitor/internal/crypto"
	"chatgpt-monitor/internal/httpapi"
	"chatgpt-monitor/internal/monitor"
	"chatgpt-monitor/internal/notify"
	"chatgpt-monitor/internal/store"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := config.Load()
	if err != nil {
		logger.Error("configuration rejected", "error", err)
		os.Exit(1)
	}
	keyring, err := credentialcrypto.NewKeyring(cfg.CredentialMasterKeys, cfg.CredentialActiveKeyID)
	if err != nil {
		logger.Error("credential encryption initialization failed", "error_code", "credential_keyring_invalid")
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	serviceLock, err := monitor.AcquireServiceLock(cfg.DBPath)
	if err != nil {
		logger.Error("service lock rejected", "error_code", "service_already_running")
		os.Exit(1)
	}
	defer serviceLock.Close()
	database, err := store.Open(ctx, cfg.DBPath, os.DirFS(cfg.MigrationsDir))
	if err != nil {
		logger.Error("database initialization failed", "error", err)
		os.Exit(1)
	}
	defer database.Close()
	if len(os.Args) > 1 {
		if len(os.Args) != 2 || os.Args[1] != "reencrypt-credentials" {
			logger.Error("unsupported server command", "error_code", "invalid_command")
			os.Exit(2)
		}
		count, err := keyring.ReencryptAccounts(ctx, database.DB())
		if err != nil {
			logger.Error("credential re-encryption failed", "error_code", "credential_reencrypt_failed")
			os.Exit(1)
		}
		logger.Info("credential re-encryption completed", "records_updated", count, "active_key_id", keyring.ActiveKeyID())
		return
	}
	authManager, err := auth.New(auth.Config{
		DB: database.DB(), Username: cfg.AdminUser, PasswordHash: cfg.AdminPasswordHash,
		TOTPSecret: cfg.AdminTOTPSecret, JWTKey: cfg.JWTSigningKey, RateLimitKey: cfg.RateLimitKey,
	})
	if err != nil {
		logger.Error("authentication initialization failed", "error_code", "auth_config_invalid")
		os.Exit(1)
	}
	chatgptClient := chatgpt.NewClient(chatgpt.Config{EvidenceLevel: chatgpt.EvidenceLiveVerified})
	accountService, err := account.NewService(database.DB(), chatgptClient, keyring)
	if err != nil {
		logger.Error("account service initialization failed", "error_code", "account_config_invalid")
		os.Exit(1)
	}
	monitorService, err := monitor.New(database.DB(), chatgptClient, keyring, monitor.DefaultConfig())
	if err != nil {
		logger.Error("monitor initialization failed", "error_code", "monitor_config_invalid")
		os.Exit(1)
	}
	if err := monitorService.RecoverInterrupted(ctx); err != nil {
		logger.Error("monitor recovery failed", "error_code", "monitor_recovery_failed")
		os.Exit(1)
	}
	recovered, err := monitorService.RecoverMisclassifiedSupplementalDenials(ctx)
	if err != nil {
		logger.Error("monitor supplemental denial recovery failed", "error_code", "monitor_supplemental_recovery_failed")
		os.Exit(1)
	}
	logger.Info("monitor supplemental denial recovery completed", "accounts_recovered", recovered)
	settingsService, err := notify.NewSettingsService(database.DB(), keyring)
	if err != nil {
		logger.Error("settings initialization failed", "error_code", "settings_config_invalid")
		os.Exit(1)
	}
	notificationConsumer, err := notify.NewConsumer(database.DB(), notify.NewLogNotifier(logger))
	if err != nil {
		logger.Error("notification consumer initialization failed", "error_code", "notification_config_invalid")
		os.Exit(1)
	}
	allocationSink, err := allocationsync.NewHTTPSink(cfg.AllocationAccountEventURL, cfg.AllocationAccountEventAPIKey, nil)
	if err != nil {
		logger.Error("allocation account event sink initialization failed", "error_code", "allocation_event_config_invalid")
		os.Exit(1)
	}
	allocationConsumer, err := allocationsync.NewConsumer(database.DB(), allocationSink, logger)
	if err != nil {
		logger.Error("allocation account outbox initialization failed", "error_code", "allocation_event_config_invalid")
		os.Exit(1)
	}
	background := supervisor.New(logger, supervisor.Config{})
	if err := background.GoManaged(ctx, "poller", "monitor", func(taskCtx context.Context) error {
		return monitorService.RunScheduler(taskCtx, logger)
	}); err != nil {
		logger.Error("monitor scheduler registration failed", "error_code", "background_registration_failed")
		os.Exit(1)
	}
	if err := background.GoManaged(ctx, "outbox", "monitor", notificationConsumer.Run); err != nil {
		logger.Error("notification consumer registration failed", "error_code", "background_registration_failed")
		os.Exit(1)
	}
	if err := background.GoManaged(ctx, "allocation-account-outbox", "monitor", allocationConsumer.Run); err != nil {
		logger.Error("allocation account outbox registration failed", "error_code", "background_registration_failed")
		os.Exit(1)
	}

	server := &http.Server{
		Addr: cfg.ListenAddr,
		Handler: httpapi.NewRouter(database, authManager, accountService, httpapi.Config{
			Origin: cfg.AppOrigin, TrustLoopbackProxy: cfg.TrustLoopbackProxy, Monitor: monitorService, Settings: settingsService,
			AllocationServiceAPIKey: cfg.AllocationServiceAPIKey,
		}, logger),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      25 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	errCh := make(chan error, 1)
	go serveHTTP(server, logger, cfg.DevTLSCert, cfg.DevTLSKey, errCh)

	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server stopped unexpectedly", "error", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			logger.Error("server shutdown failed", "error", err)
			os.Exit(1)
		}
		background.Wait()
		if err := monitorService.Wait(shutdownCtx); err != nil {
			logger.Error("monitor shutdown failed", "error_code", "monitor_shutdown_timeout")
			os.Exit(1)
		}
		if err := notificationConsumer.Wait(shutdownCtx); err != nil {
			logger.Error("notification consumer shutdown failed", "error_code", "notification_shutdown_timeout")
			os.Exit(1)
		}
		logger.Info("server stopped")
	}
}

func serveHTTP(server *http.Server, logger *slog.Logger, cert, key string, errCh chan<- error) {
	devTLS := cert != ""
	logger.Info("server started", "address", server.Addr, "dev_tls", devTLS)
	if devTLS {
		errCh <- server.ListenAndServeTLS(cert, key)
		return
	}
	errCh <- server.ListenAndServe()
}
