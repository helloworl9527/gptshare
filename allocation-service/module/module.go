package module

import (
	"context"
	"errors"
	"log/slog"
	"time"

	accountsvc "allocation-service/internal/account"
	allocatorsvc "allocation-service/internal/allocator"
	cardsvc "allocation-service/internal/card"
	"allocation-service/internal/credential"
	"allocation-service/internal/httpapi"
	metricssvc "allocation-service/internal/metrics"
	replacementsvc "allocation-service/internal/replacement"
	"allocation-service/internal/repository"
	"allocation-service/internal/store"
	userquerysvc "allocation-service/internal/userquery"
	"allocation-service/monitorfacade"
	"github.com/gin-gonic/gin"
)

type Module struct {
	store        *store.Store
	repo         *repository.Repository
	accounts     *accountsvc.Service
	cards        *cardsvc.Service
	allocator    *allocatorsvc.Service
	userQuery    *userquerysvc.Service
	replacements *replacementsvc.Service
	metrics      *metricssvc.Service
	logger       *slog.Logger
}

// Migrate applies only the allocation schema ledger and closes the database.
func Migrate(ctx context.Context, dbPath string) error {
	database, err := store.Open(ctx, dbPath)
	if err != nil {
		return err
	}
	return database.Close()
}

type AdminBoundary struct {
	RequireSession gin.HandlerFunc
	RequireCSRF    gin.HandlerFunc
	RequireOrigin  gin.HandlerFunc
}

func Open(ctx context.Context, dbPath string, keys map[string][]byte, activeKeyID string, monitor monitorfacade.Client, logger *slog.Logger) (*Module, error) {
	if monitor == nil {
		return nil, errors.New("allocation monitor facade is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	keyring, err := credential.NewKeyring(keys, activeKeyID)
	if err != nil {
		return nil, errors.New("allocation credential keyring rejected")
	}
	database, err := store.Open(ctx, dbPath)
	if err != nil {
		return nil, err
	}
	repo := repository.New(database.DB(), keyring)
	return &Module{
		store:        database,
		repo:         repo,
		accounts:     accountsvc.NewService(repo, monitor),
		cards:        cardsvc.NewService(repo),
		allocator:    allocatorsvc.NewService(repo, monitor),
		userQuery:    userquerysvc.NewService(repo),
		replacements: replacementsvc.NewService(repo, logger),
		metrics:      metricssvc.NewService(repo),
		logger:       logger,
	}, nil
}

func (m *Module) Close() error {
	if m == nil || m.store == nil {
		return nil
	}
	return m.store.Close()
}

func (m *Module) Health(ctx context.Context) error {
	if m == nil || m.store == nil {
		return errors.New("allocation module is not initialized")
	}
	return m.store.Health(ctx)
}

func (m *Module) RegisterPublicRoutes(router *gin.Engine) {
	httpapi.RegisterPublicAPIRoutes(router, httpapi.Config{Cards: m.cards, Allocator: m.allocator, UserQuery: m.userQuery})
}

func (m *Module) RegisterAdminRoutes(router *gin.Engine, boundary AdminBoundary) error {
	return httpapi.RegisterAdminRoutes(router, httpapi.Config{
		Accounts:  m.accounts,
		Cards:     m.cards,
		Allocator: m.allocator,
		Metrics:   m.metrics,
	}, httpapi.AdminBoundary{
		RequireSession: boundary.RequireSession,
		RequireCSRF:    boundary.RequireCSRF,
		RequireOrigin:  boundary.RequireOrigin,
	})
}

func (m *Module) RunReplacement(ctx context.Context) {
	m.replacements.StartHourly(ctx)
}

func (m *Module) RunReplacementEvery(ctx context.Context, interval time.Duration) {
	m.replacements.Start(ctx, interval)
}

func (m *Module) RunFacadeSync(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = time.Hour
	}
	syncOnce := func() {
		if err := m.syncFacade(ctx); err != nil {
			kind, ok := monitorfacade.FaultKindOf(err)
			if !ok {
				kind = monitorfacade.FaultUnavailable
			}
			m.logger.Error("allocation monitor facade sync failed", "module", "allocation", "task", "facade-sync", "error_code", "monitor_facade_"+string(kind))
		}
	}
	syncOnce()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			syncOnce()
		}
	}
}

func (m *Module) syncFacade(ctx context.Context) error {
	if _, err := m.accounts.PullFromMonitor(ctx); err != nil {
		return err
	}
	_, err := m.accounts.SyncAll(ctx)
	return err
}
