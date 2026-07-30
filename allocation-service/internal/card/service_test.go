package card

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"allocation-service/internal/credential"
	"allocation-service/internal/repository"
	"allocation-service/internal/store"
)

func TestGenerateOneHundredUniqueFormatAndHashOnlyStorage(t *testing.T) {
	database := openCardStore(t)
	defer database.Close()
	repo := repository.New(database.DB(), repositoryTestCredentialKeyring(t))
	service := NewService(repo)
	result, err := service.Generate(context.Background(), 100, 30)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Cards) != 100 {
		t.Fatalf("cards=%d want 100", len(result.Cards))
	}
	pattern := regexp.MustCompile(`^[2-9A-HJKMNP-Z]{4}-[2-9A-HJKMNP-Z]{4}-[2-9A-HJKMNP-Z]{4}$`)
	seen := make(map[string]struct{}, 100)
	for _, card := range result.Cards {
		if !pattern.MatchString(card.Code) || !ValidCode(card.Code) {
			t.Fatalf("invalid code format: %s", card.Code)
		}
		if strings.ContainsAny(card.Code, "0OIL1") {
			t.Fatalf("ambiguous character present: %s", card.Code)
		}
		if _, exists := seen[card.Code]; exists {
			t.Fatalf("duplicate code: %s", card.Code)
		}
		seen[card.Code] = struct{}{}
	}
	rows, err := database.DB().QueryContext(context.Background(), "SELECT code_hash,code_suffix FROM cards ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var hash []byte
		var suffix string
		if err := rows.Scan(&hash, &suffix); err != nil {
			t.Fatal(err)
		}
		if len(hash) != 32 {
			t.Fatalf("hash length=%d want 32", len(hash))
		}
		for code := range seen {
			if bytes.Equal(hash, []byte(code)) || bytes.Contains(hash, []byte(code)) || suffix == code {
				t.Fatalf("database stored recoverable card code: suffix=%q code=%q hash=%x", suffix, code, hash)
			}
		}
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if count != 100 {
		t.Fatalf("db cards=%d want 100", count)
	}
}

func TestGenerateAcceptsEveryDayWithinThirtyAndRejectsLegacyNewDurations(t *testing.T) {
	database := openCardStore(t)
	defer database.Close()
	service := NewService(repository.New(database.DB(), repositoryTestCredentialKeyring(t)))
	for _, days := range []int{1, 2, 5, 29, 30} {
		result, err := service.Generate(context.Background(), 1, days)
		if err != nil || len(result.Cards) != 1 || result.Cards[0].DurationDays != days {
			t.Fatalf("duration=%d result=%+v err=%v", days, result, err)
		}
	}
	for _, days := range []int{-1, 0, 31, 90} {
		if _, err := service.Generate(context.Background(), 1, days); !errors.Is(err, ErrValidation) {
			t.Fatalf("duration=%d err=%v want validation", days, err)
		}
	}
}

func repositoryTestCredentialKeyring(t *testing.T) *credential.Keyring {
	t.Helper()
	keyring, err := credential.NewKeyring(map[string][]byte{"alloc-card-test": bytes.Repeat([]byte{4}, 32)}, "alloc-card-test")
	if err != nil {
		t.Fatal(err)
	}
	return keyring
}

func TestExportFormatsContainOnlyGeneratedPlaintext(t *testing.T) {
	cards := []GeneratedCard{
		{Code: "2345-6789-ABCD", DurationDays: 7},
		{Code: "EFGH-JKMN-PQRS", DurationDays: 14},
	}
	contentType, filename, body, err := FormatExport(cards, ExportCSV)
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "text/csv; charset=utf-8" || filename != "cards.csv" || body != "code,duration_days\n2345-6789-ABCD,7\nEFGH-JKMN-PQRS,14\n" {
		t.Fatalf("bad csv export contentType=%q filename=%q body=%q", contentType, filename, body)
	}
	contentType, filename, body, err = FormatExport(cards, ExportTXT)
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "text/plain; charset=utf-8" || filename != "cards.txt" || body != "2345-6789-ABCD\nEFGH-JKMN-PQRS\n" {
		t.Fatalf("bad txt export contentType=%q filename=%q body=%q", contentType, filename, body)
	}
}

func openCardStore(t *testing.T) *store.Store {
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
