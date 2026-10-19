package card

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"net"
	"strings"
	"time"

	"allocation-service/internal/repository"
)

type ActionRepository interface {
	ApplyCardAction(context.Context, string, []byte, []byte, bool, bool) (repository.CardActionResult, error)
	CardOperations(context.Context, int64) ([]repository.CardOperation, error)
	QueryFailures(context.Context, []byte, time.Time) (int, error)
	RecordQueryFailure(context.Context, []byte, time.Time) error
	ResetQueryFailures(context.Context, []byte) error
	CreateCaptcha(context.Context, []byte, string, time.Time) (repository.CaptchaChallenge, error)
	VerifyCaptcha(context.Context, []byte, int64, string, time.Time) error
}

type ActionInput struct {
	Action        string
	OriginalCode  string
	NewCode       string
	Preview       bool
	ClientIP      string
	CaptchaID     int64
	CaptchaAnswer string
}

type ActionResponse struct {
	Operation repository.CardActionResult
	Captcha   *repository.CaptchaChallenge
}

func (s *Service) PerformAction(ctx context.Context, input ActionInput) (ActionResponse, error) {
	repo, ok := s.repo.(ActionRepository)
	if !ok {
		return ActionResponse{}, errors.New("card actions unavailable")
	}
	ip := net.ParseIP(input.ClientIP)
	key := "invalid"
	if ip != nil {
		key = ip.String()
	}
	subject := sha256.Sum256([]byte("card-actions-v1:" + key))
	now := s.now().UTC()
	failures, err := repo.QueryFailures(ctx, subject[:], now)
	if err != nil {
		return ActionResponse{}, err
	}
	if failures >= 3 {
		if input.CaptchaID == 0 || strings.TrimSpace(input.CaptchaAnswer) == "" {
			challenge, e := repo.CreateCaptcha(ctx, subject[:], "", now)
			return ActionResponse{Captcha: &challenge}, captchaError(e)
		}
		if err = repo.VerifyCaptcha(ctx, subject[:], input.CaptchaID, input.CaptchaAnswer, now); err != nil {
			return ActionResponse{}, repository.ErrCaptchaInvalid
		}
		if err = repo.ResetQueryFailures(ctx, subject[:]); err != nil {
			return ActionResponse{}, err
		}
	}
	original := strings.ToUpper(strings.TrimSpace(input.OriginalCode))
	next := strings.ToUpper(strings.TrimSpace(input.NewCode))
	if !ValidCode(original) || !ValidCode(next) || original == next || input.Action != "renew" && input.Action != "different" {
		err = ErrValidation
	} else {
		available := s.monitor == nil || s.monitor.Available(ctx)
		var result repository.CardActionResult
		result, err = repo.ApplyCardAction(ctx, input.Action, HashCode(original), HashCode(next), input.Preview, available)
		if err == nil {
			if err = repo.ResetQueryFailures(ctx, subject[:]); err != nil {
				return ActionResponse{}, err
			}
			// Internal IDs are retained for audit only; HTTP serialization exposes no group members.
			return ActionResponse{Operation: result}, nil
		}
	}
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, repository.ErrCardStateConflict) || errors.Is(err, repository.ErrCardActionConflict) || errors.Is(err, repository.ErrInvalidCardAction) || errors.Is(err, ErrValidation) {
		if e := repo.RecordQueryFailure(ctx, subject[:], now); e != nil {
			return ActionResponse{}, e
		}
		if failures >= 2 {
			challenge, e := repo.CreateCaptcha(ctx, subject[:], "", now)
			return ActionResponse{Captcha: &challenge}, captchaError(e)
		}
		return ActionResponse{}, ErrValidation
	}
	return ActionResponse{}, err
}

func captchaError(err error) error {
	if err != nil {
		return err
	}
	return repository.ErrCaptchaRequired
}

func (s *Service) OperationHistory(ctx context.Context, id int64) ([]repository.CardOperation, error) {
	repo, ok := s.repo.(ActionRepository)
	if !ok {
		return []repository.CardOperation{}, nil
	}
	return repo.CardOperations(ctx, id)
}
