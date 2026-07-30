package card

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/csv"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"time"

	"allocation-service/internal/models"
	"allocation-service/internal/repository"
)

const alphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

var (
	ErrValidation      = errors.New("card validation failed")
	ErrNotFound        = errors.New("card not found")
	ErrConflict        = errors.New("card state conflict")
	ErrCodeUnavailable = errors.New("card plaintext unavailable")
	ErrDurationLimit   = errors.New("card duration limit exceeded")
)

type Repository interface {
	CreateCard(context.Context, repository.CardSeed) (int64, error)
	ListCards(context.Context, repository.CardFilter) ([]models.Card, error)
	CardByID(context.Context, int64) (models.Card, error)
	CardByHash(context.Context, []byte) (models.Card, error)
	RevokeCard(context.Context, int64) (models.Card, error)
	ExtendCard(context.Context, int64, int) (models.Card, error)
	ExpireDueCards(context.Context, time.Time) (int64, error)
	RevealCardCode(context.Context, int64) (repository.RevealedCardCode, error)
	Audit(context.Context, string, string, int64, map[string]any) error
}

type Service struct {
	repo Repository
	now  func() time.Time
}

type GeneratedCard struct {
	ID           int64
	Code         string
	CodeSuffix   string
	DurationDays int
	Status       string
}

type GenerateResult struct {
	Cards []GeneratedCard
}

type ExportFormat string

const (
	ExportCSV ExportFormat = "csv"
	ExportTXT ExportFormat = "txt"
)

func NewService(repo Repository) *Service {
	return &Service{repo: repo, now: func() time.Time { return time.Now().UTC() }}
}

func (s *Service) SetNow(now func() time.Time) {
	if now != nil {
		s.now = now
	}
}

func (s *Service) Generate(ctx context.Context, quantity, durationDays int) (GenerateResult, error) {
	if quantity < 1 || quantity > 1000 || !validGeneratedDuration(durationDays) {
		return GenerateResult{}, ErrValidation
	}
	seen := make(map[string]struct{}, quantity)
	result := GenerateResult{Cards: make([]GeneratedCard, 0, quantity)}
	for len(result.Cards) < quantity {
		code, err := generateCode()
		if err != nil {
			return GenerateResult{}, err
		}
		if _, exists := seen[code]; exists {
			continue
		}
		seen[code] = struct{}{}
		id, err := s.repo.CreateCard(ctx, repository.CardSeed{CodeHash: HashCode(code), CodeSuffix: code[len(code)-4:], CodePlaintext: code, DurationDays: durationDays})
		if err != nil {
			return GenerateResult{}, err
		}
		result.Cards = append(result.Cards, GeneratedCard{ID: id, Code: code, CodeSuffix: code[len(code)-4:], DurationDays: durationDays, Status: "unused"})
	}
	_ = s.repo.Audit(ctx, "cards.generate", "card_batch", 0, map[string]any{"quantity": quantity, "duration_days": durationDays})
	return result, nil
}

func (s *Service) Export(ctx context.Context, quantity, durationDays int, format ExportFormat) (contentType, filename, body string, err error) {
	result, err := s.Generate(ctx, quantity, durationDays)
	if err != nil {
		return "", "", "", err
	}
	contentType, filename, body, err = FormatExport(result.Cards, format)
	if err != nil {
		return "", "", "", err
	}
	_ = s.repo.Audit(ctx, "cards.export", "card_batch", 0, map[string]any{"quantity": quantity, "duration_days": durationDays, "format": string(format)})
	return contentType, filename, body, nil
}

func (s *Service) List(ctx context.Context, filter repository.CardFilter) ([]models.Card, error) {
	if filter.Status != "" && !validStatus(filter.Status) {
		return nil, ErrValidation
	}
	if filter.DurationDays != 0 && !validStoredDuration(filter.DurationDays) {
		return nil, ErrValidation
	}
	return s.repo.ListCards(ctx, filter)
}

func (s *Service) Revoke(ctx context.Context, id int64) (models.Card, error) {
	if id <= 0 {
		return models.Card{}, ErrValidation
	}
	card, err := s.repo.RevokeCard(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return models.Card{}, ErrNotFound
	}
	if err != nil {
		return models.Card{}, err
	}
	_ = s.repo.Audit(ctx, "cards.revoke", "card", id, map[string]any{"status": card.Status})
	return card, nil
}

func (s *Service) Extend(ctx context.Context, id int64, days int) (models.Card, error) {
	if id <= 0 || !validExtensionDuration(days) {
		return models.Card{}, ErrValidation
	}
	card, err := s.repo.ExtendCard(ctx, id, days)
	if errors.Is(err, sql.ErrNoRows) {
		return models.Card{}, ErrNotFound
	}
	if errors.Is(err, repository.ErrCardDurationLimit) {
		return models.Card{}, ErrDurationLimit
	}
	if err != nil {
		return models.Card{}, err
	}
	_ = s.repo.Audit(ctx, "cards.extend", "card", id, map[string]any{"days": days})
	return card, nil
}

func (s *Service) ExpireDue(ctx context.Context) (int64, error) {
	count, err := s.repo.ExpireDueCards(ctx, s.now().UTC())
	if err != nil {
		return 0, err
	}
	_ = s.repo.Audit(ctx, "cards.expire_due", "card_batch", 0, map[string]any{"count": count})
	return count, nil
}

func (s *Service) Reveal(ctx context.Context, id int64) (repository.RevealedCardCode, error) {
	if id <= 0 {
		return repository.RevealedCardCode{}, ErrValidation
	}
	result, err := s.repo.RevealCardCode(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return repository.RevealedCardCode{}, ErrNotFound
	}
	if err != nil {
		return repository.RevealedCardCode{}, err
	}
	_ = s.repo.Audit(ctx, "cards.reveal", "card", id, map[string]any{"plaintext_available": result.Available})
	if !result.Available {
		return result, ErrCodeUnavailable
	}
	return result, nil
}

func (s *Service) UserView(ctx context.Context, code string) (models.Card, error) {
	normalized := strings.ToUpper(strings.TrimSpace(code))
	if !ValidCode(normalized) {
		return models.Card{}, ErrNotFound
	}
	card, err := s.repo.CardByHash(ctx, HashCode(normalized))
	if errors.Is(err, sql.ErrNoRows) {
		return models.Card{}, ErrNotFound
	}
	if err != nil {
		return models.Card{}, err
	}
	if card.Status != "redeemed" || card.ExpiresAt == nil || !card.ExpiresAt.After(s.now().UTC()) {
		return models.Card{}, ErrNotFound
	}
	return card, nil
}

func FormatExport(cards []GeneratedCard, format ExportFormat) (contentType, filename, body string, err error) {
	switch format {
	case ExportCSV:
		var builder strings.Builder
		writer := csv.NewWriter(&builder)
		if err := writer.Write([]string{"code", "duration_days"}); err != nil {
			return "", "", "", err
		}
		for _, card := range cards {
			if err := writer.Write([]string{card.Code, strconv.Itoa(card.DurationDays)}); err != nil {
				return "", "", "", err
			}
		}
		writer.Flush()
		if err := writer.Error(); err != nil {
			return "", "", "", err
		}
		return "text/csv; charset=utf-8", "cards.csv", builder.String(), nil
	case ExportTXT:
		lines := make([]string, 0, len(cards))
		for _, card := range cards {
			lines = append(lines, card.Code)
		}
		return "text/plain; charset=utf-8", "cards.txt", strings.Join(lines, "\n") + "\n", nil
	default:
		return "", "", "", ErrValidation
	}
}

func HashCode(code string) []byte {
	sum := sha256.Sum256([]byte("allocation-card-v1:" + strings.ToUpper(strings.TrimSpace(code))))
	return sum[:]
}

func ValidCode(code string) bool {
	if len(code) != 14 || code[4] != '-' || code[9] != '-' {
		return false
	}
	for _, idx := range []int{0, 1, 2, 3, 5, 6, 7, 8, 10, 11, 12, 13} {
		if !strings.ContainsRune(alphabet, rune(code[idx])) {
			return false
		}
	}
	return true
}

func generateCode() (string, error) {
	chars := make([]byte, 12)
	max := big.NewInt(int64(len(alphabet)))
	for i := range chars {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		chars[i] = alphabet[n.Int64()]
	}
	return fmt.Sprintf("%s-%s-%s", chars[0:4], chars[4:8], chars[8:12]), nil
}

func validExtensionDuration(days int) bool {
	return days >= 1 && days <= 30
}

func validGeneratedDuration(days int) bool {
	return validExtensionDuration(days) || days == 90
}

func validStoredDuration(days int) bool {
	return validGeneratedDuration(days)
}

func validStatus(status string) bool {
	switch status {
	case "unused", "redeemed", "expired", "revoked":
		return true
	default:
		return false
	}
}
