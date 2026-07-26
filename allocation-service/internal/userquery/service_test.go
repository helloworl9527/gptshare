package userquery

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"allocation-service/internal/repository"
	"allocation-service/internal/store"
)

func TestCaptchaTriggerPassResetAndExpire(t *testing.T) {
	database := openUserQueryStore(t)
	defer database.Close()
	repo := repository.New(database.DB())
	service := NewService(repo)
	now := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)
	service.SetNow(func() time.Time { return now })
	input := QueryInput{Code: "BAD-CARD", ClientIP: "127.0.0.1"}
	for i := 0; i < 2; i++ {
		if _, err := service.Query(context.Background(), input); !errors.Is(err, ErrInvalidQuery) {
			t.Fatalf("failure %d err=%v want %v", i+1, err, ErrInvalidQuery)
		}
	}
	result, err := service.Query(context.Background(), input)
	if !errors.Is(err, ErrCaptchaRequired) || result.Captcha == nil {
		t.Fatalf("third failure err=%v captcha=%+v", err, result.Captcha)
	}
	answer := answerForQuestion(t, result.Captcha.Question)
	input.CaptchaID = result.Captcha.ID
	input.CaptchaAnswer = answer
	if _, err := service.Query(context.Background(), input); !errors.Is(err, ErrInvalidQuery) {
		t.Fatalf("captcha pass should reset then return uniform invalid query, got %v", err)
	}
	failures, err := repo.QueryFailures(context.Background(), subjectHash(input.Code, input.ClientIP), now)
	if err != nil {
		t.Fatal(err)
	}
	if failures != 1 {
		t.Fatalf("failures after captcha reset=%d want 1", failures)
	}
	for i := 0; i < 2; i++ {
		_, _ = service.Query(context.Background(), QueryInput{Code: "EXPIRED-CAPTCHA", ClientIP: "127.0.0.1"})
	}
	expiring, err := service.Query(context.Background(), QueryInput{Code: "EXPIRED-CAPTCHA", ClientIP: "127.0.0.1"})
	if !errors.Is(err, ErrCaptchaRequired) || expiring.Captcha == nil {
		t.Fatalf("expected captcha for expiry path err=%v result=%+v", err, expiring)
	}
	service.SetNow(func() time.Time { return now.Add(3 * time.Minute) })
	_, err = service.Query(context.Background(), QueryInput{Code: "EXPIRED-CAPTCHA", ClientIP: "127.0.0.1", CaptchaID: expiring.Captcha.ID, CaptchaAnswer: answerForQuestion(t, expiring.Captcha.Question)})
	if !errors.Is(err, ErrCaptchaInvalid) {
		t.Fatalf("expired captcha err=%v want %v", err, ErrCaptchaInvalid)
	}
}

func answerForQuestion(t *testing.T, question string) string {
	t.Helper()
	parts := strings.Split(question, "+")
	if len(parts) != 2 {
		t.Fatalf("bad captcha question %q", question)
	}
	left, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil {
		t.Fatal(err)
	}
	right, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil {
		t.Fatal(err)
	}
	return strconv.Itoa(left + right)
}

func openUserQueryStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(context.Background(), filepath.Join(dir, "allocation.db"))
	if err != nil {
		t.Fatal(err)
	}
	return db
}
