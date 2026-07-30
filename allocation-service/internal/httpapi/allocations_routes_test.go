package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	cardsvc "allocation-service/internal/card"
	"allocation-service/internal/repository"
)

func TestAdminAllocationsRequiresSessionAndReturnsEmptyList(t *testing.T) {
	server := testAccountsTLSServer(t, "http://127.0.0.1:1")

	unauthorized := getRaw(t, server.Client(), server.URL+"/api/admin/allocations")
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d body=%s", unauthorized.StatusCode, readBody(t, unauthorized))
	}

	client := authedAccountClient(t, server)
	response := getRaw(t, client, server.URL+"/api/admin/allocations")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("empty list status=%d body=%s", response.StatusCode, readBody(t, response))
	}
	var payload struct {
		Allocations []json.RawMessage `json:"allocations"`
	}
	if err := json.Unmarshal([]byte(readBody(t, response)), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Allocations == nil || len(payload.Allocations) != 0 {
		t.Fatalf("empty allocations=%s", payload.Allocations)
	}
}

func TestAdminAllocationsReturnsRealAccountsSortedWithoutSecrets(t *testing.T) {
	server := testAccountsTLSServer(t, "http://127.0.0.1:1")
	client := authedAccountClient(t, server)
	value, ok := testRepositories.Load(server.URL)
	if !ok {
		t.Fatal("test repository not found")
	}
	repo := value.(*repository.Repository)
	ctx := context.Background()
	base := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return base })

	for _, username := range []string{"fund-outlier@example.test", "flagon_snap@example.test"} {
		if _, err := repo.CreateAccount(ctx, repository.AccountSeed{
			DisplayUsername:    username,
			DisplayPassword:    "password-sentinel",
			DisplayTOTPSecret:  "totp-sentinel",
			AccountExpiry:      base.Add(20 * 24 * time.Hour),
			MaxConcurrentUsers: 1,
			MonitorStatus:      "alive",
			Status:             "available",
		}); err != nil {
			t.Fatal(err)
		}
	}

	codes := []string{"2345-6789-ABCD", "2345-6789-EFGH"}
	results := make([]repository.RedeemResult, 0, len(codes))
	for index, code := range codes {
		at := base.Add(time.Duration(index) * time.Hour)
		repo.SetNow(func() time.Time { return at })
		if _, err := repo.CreateCard(ctx, repository.CardSeed{
			CodeHash: cardsvc.HashCode(code), CodeSuffix: code[len(code)-4:],
			CodePlaintext: code, DurationDays: 7,
		}); err != nil {
			t.Fatal(err)
		}
		result, err := repo.RedeemCode(ctx, cardsvc.HashCode(code), true)
		if err != nil {
			t.Fatal(err)
		}
		results = append(results, result)
	}

	response := getRaw(t, client, server.URL+"/api/admin/allocations")
	body := readBody(t, response)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.StatusCode, body)
	}
	var payload struct {
		Allocations []struct {
			ID              int64  `json:"id"`
			CardID          int64  `json:"card_id"`
			AccountID       int64  `json:"account_id"`
			DisplayUsername string `json:"display_username"`
			AllocationState string `json:"allocation_state"`
			CodeSuffix      string `json:"code_suffix"`
			DurationDays    int    `json:"duration_days"`
			AllocatedAt     string `json:"allocated_at"`
			ValidUntil      string `json:"valid_until"`
			AccountExpiry   string `json:"account_expiry"`
		} `json:"allocations"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Allocations) != 2 {
		t.Fatalf("allocations=%+v", payload.Allocations)
	}
	if payload.Allocations[0].ID != results[1].Allocation.ID ||
		payload.Allocations[0].AccountID != results[1].Account.ID ||
		payload.Allocations[0].DisplayUsername != "flagon_snap@example.test" {
		t.Fatalf("newest allocation mismatch: %+v", payload.Allocations[0])
	}
	if payload.Allocations[1].AccountID != results[0].Account.ID ||
		payload.Allocations[1].DisplayUsername != "fund-outlier@example.test" {
		t.Fatalf("older allocation mismatch: %+v", payload.Allocations[1])
	}
	for _, forbidden := range []string{
		"password", "totp", "2fa", "code_hash", "encrypted_code", "ciphertext",
		"password-sentinel", "totp-sentinel", codes[0], codes[1],
	} {
		if strings.Contains(strings.ToLower(body), strings.ToLower(forbidden)) {
			t.Fatalf("response leaked forbidden value %q: %s", forbidden, body)
		}
	}
}
