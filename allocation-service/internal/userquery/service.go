package userquery

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"net"
	"strings"
	"time"

	cardsvc "allocation-service/internal/card"
	"allocation-service/internal/repository"
)

var (
	ErrInvalidQuery    = errors.New("query not available")
	ErrCaptchaRequired = errors.New("captcha required")
	ErrCaptchaInvalid  = errors.New("captcha invalid")
	ErrInternal        = errors.New("query failed")
)

type Repository interface {
	UserAllocationsByCodeHash(context.Context, []byte, time.Time) ([]repository.UserAllocationView, error)
	QueryFailures(context.Context, []byte, time.Time) (int, error)
	RecordQueryFailure(context.Context, []byte, time.Time) error
	ResetQueryFailures(context.Context, []byte) error
	CreateCaptcha(context.Context, []byte, string, time.Time) (repository.CaptchaChallenge, error)
	VerifyCaptcha(context.Context, []byte, int64, string, time.Time) error
}

type Service struct {
	repo Repository
	now  func() time.Time
}

type QueryInput struct {
	Code          string
	ClientIP      string
	CaptchaID     int64
	CaptchaAnswer string
}

type QueryResult struct {
	View     repository.UserAllocationView
	Views    []repository.UserAllocationView
	Captcha  *repository.CaptchaChallenge
	Warnings []string
	Elapsed  time.Duration
}

func NewService(repo Repository) *Service {
	return &Service{repo: repo, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) SetNow(now func() time.Time) {
	if now != nil {
		s.now = now
	}
}

func (s *Service) Query(ctx context.Context, input QueryInput) (QueryResult, error) {
	started := s.now()
	normalized := strings.ToUpper(strings.TrimSpace(input.Code))
	subject := subjectHash(normalized, input.ClientIP)
	now := s.now().UTC()
	failures, err := s.repo.QueryFailures(ctx, subject, now)
	if err != nil {
		return QueryResult{}, ErrInternal
	}
	if failures >= 3 {
		if input.CaptchaID == 0 || strings.TrimSpace(input.CaptchaAnswer) == "" {
			challenge, err := s.repo.CreateCaptcha(ctx, subject, "", now)
			if err != nil {
				return QueryResult{}, ErrInternal
			}
			challenge.Failures = failures
			return QueryResult{Captcha: &challenge, Elapsed: s.now().Sub(started)}, ErrCaptchaRequired
		}
		if err := s.repo.VerifyCaptcha(ctx, subject, input.CaptchaID, input.CaptchaAnswer, now); err != nil {
			return QueryResult{}, ErrCaptchaInvalid
		}
		if err := s.repo.ResetQueryFailures(ctx, subject); err != nil {
			return QueryResult{}, ErrInternal
		}
	}
	if !cardsvc.ValidCode(normalized) {
		return s.recordFailure(ctx, subject, started)
	}
	views, err := s.repo.UserAllocationsByCodeHash(ctx, cardsvc.HashCode(normalized), now)
	if errors.Is(err, sql.ErrNoRows) {
		return s.recordFailure(ctx, subject, started)
	}
	if err != nil {
		return QueryResult{}, ErrInternal
	}
	if err := s.repo.ResetQueryFailures(ctx, subject); err != nil {
		return QueryResult{}, ErrInternal
	}
	return QueryResult{View: views[0], Views: views, Elapsed: s.now().Sub(started)}, nil
}

func (s *Service) recordFailure(ctx context.Context, subject []byte, started time.Time) (QueryResult, error) {
	now := s.now().UTC()
	if err := s.repo.RecordQueryFailure(ctx, subject, now); err != nil {
		return QueryResult{}, ErrInternal
	}
	failures, err := s.repo.QueryFailures(ctx, subject, now)
	if err != nil {
		return QueryResult{}, ErrInternal
	}
	if failures >= 3 {
		challenge, err := s.repo.CreateCaptcha(ctx, subject, "", now)
		if err != nil {
			return QueryResult{}, ErrInternal
		}
		challenge.Failures = failures
		return QueryResult{Captcha: &challenge, Elapsed: s.now().Sub(started)}, ErrCaptchaRequired
	}
	return QueryResult{Elapsed: s.now().Sub(started)}, ErrInvalidQuery
}

func subjectHash(code, clientIP string) []byte {
	ip := net.ParseIP(clientIP)
	normalizedIP := "invalid"
	if ip != nil {
		normalizedIP = ip.String()
	}
	sum := sha256.Sum256([]byte("user-query-v1:" + strings.ToUpper(strings.TrimSpace(code)) + ":" + normalizedIP))
	return sum[:]
}
