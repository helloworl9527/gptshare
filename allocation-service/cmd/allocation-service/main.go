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

	accountsvc "allocation-service/internal/account"
	allocatorsvc "allocation-service/internal/allocator"
	"allocation-service/internal/auth"
	cardsvc "allocation-service/internal/card"
	"allocation-service/internal/config"
	"allocation-service/internal/credential"
	"allocation-service/internal/httpapi"
	metricssvc "allocation-service/internal/metrics"
	"allocation-service/internal/monitorclient"
	replacementsvc "allocation-service/internal/replacement"
	"allocation-service/internal/repository"
	"allocation-service/internal/store"
	userquerysvc "allocation-service/internal/userquery"
	"allocation-service/platform/supervisor"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	cfg, err := config.Load()
	if err != nil {
		logger.Error("configuration rejected", "error", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	database, err := store.Open(ctx, cfg.DBPath)
	if err != nil {
		logger.Error("database initialization failed", "error_code", "database_unavailable")
		os.Exit(1)
	}
	defer database.Close()
	authManager, err := auth.New(auth.Config{
		DB: database.DB(), Username: cfg.AdminUser, PasswordHash: cfg.AdminPasswordHash,
		TOTPSecret: cfg.AdminTOTPSecret, SessionKey: cfg.SessionSigningKey, CSRFSigningKey: cfg.CSRFSigningKey,
	})
	if err != nil {
		logger.Error("authentication initialization failed", "error_code", "auth_config_invalid")
		os.Exit(1)
	}
	keyring, err := credential.NewKeyring(cfg.CredentialMasterKeys, cfg.CredentialActiveKeyID)
	if err != nil {
		logger.Error("credential keyring initialization failed", "error_code", "credential_config_invalid")
		os.Exit(1)
	}
	repo := repository.New(database.DB(), keyring)
	monitor, err := monitorclient.New(cfg.MonitorBaseURL, cfg.MonitorAPIKey, nil)
	if err != nil {
		logger.Error("monitor client initialization failed", "error_code", "monitor_config_invalid")
		os.Exit(1)
	}
	accounts := accountsvc.NewService(repo, monitor)
	cards := cardsvc.NewService(repo)
	allocator := allocatorsvc.NewService(repo, monitor)
	userQuery := userquerysvc.NewService(repo)
	metrics := metricssvc.NewService(repo)
	replacements := replacementsvc.NewService(repo, logger)
	background := supervisor.New(logger, supervisor.Config{})
	if err := background.GoManaged(ctx, "replacement", "allocation", func(taskCtx context.Context) error {
		replacements.StartHourly(taskCtx)
		return taskCtx.Err()
	}); err != nil {
		logger.Error("replacement scanner registration failed", "error_code", "background_registration_failed")
		os.Exit(1)
	}
	server := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler: httpapi.NewRouter(database, authManager, httpapi.Config{
			Origin: cfg.AppOrigin, Accounts: accounts, Cards: cards, Allocator: allocator, UserQuery: userQuery, Metrics: metrics,
			AccountEventSink: repo, AccountEventAPIKey: cfg.AccountEventAPIKey,
		}, logger),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	errCh := make(chan error, 1)
	go serveHTTP(server, logger, cfg.Environment, errCh)
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
		logger.Info("allocation service stopped")
	}
}

func serveHTTP(server *http.Server, logger *slog.Logger, environment string, errCh chan<- error) {
	logger.Info("allocation service started", "address", server.Addr, "environment", environment)
	errCh <- server.ListenAndServe()
}
