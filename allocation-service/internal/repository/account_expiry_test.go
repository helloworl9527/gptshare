package repository

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestMonitorSyncKeepsExpiredSubscriptionUnavailable(t *testing.T) {
	db := openStore(t)
	defer db.Close()
	repo := New(db.DB(), testCredentialKeyring(t))
	ctx := context.Background()
	start := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	now := start
	repo.SetNow(func() time.Time { return now })
	accountID, err := repo.CreateAccount(ctx, AccountSeed{
		DisplayUsername: "expired-with-four-seats", DisplayPassword: "test-password", DisplayTOTPSecret: "test-totp",
		AccountExpiry: start.Add(24 * time.Hour), MaxConcurrentUsers: 5, MonitorStatus: "alive",
	})
	if err != nil {
		t.Fatal(err)
	}
	originalCard, err := repo.CreateCard(ctx, CardSeed{CodeHash: cardHashForCode("2345-6789-ABCD"), CodeSuffix: "ABCD", DurationDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RedeemCard(ctx, originalCard); err != nil {
		t.Fatal(err)
	}
	cardID, err := repo.CreateCard(ctx, CardSeed{CodeHash: cardHashForCode("EFGH-JKMN-PQRS"), CodeSuffix: "PQRS", DurationDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	now = start.Add(24 * time.Hour)
	account, err := repo.UpdateAccountMonitorStatus(ctx, accountID, "monitor-test", "alive")
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != "expired" || account.CurrentAllocations != 1 {
		t.Fatalf("account=%+v", account)
	}
	metrics, err := repo.InventoryMetrics(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if metrics.AvailableCapacity != 0 || metrics.BlockedCapacity != 4 {
		t.Fatalf("metrics=%+v", metrics)
	}
	if _, err := repo.RedeemCard(ctx, cardID); !errors.Is(err, ErrNoAccountCapacity) {
		t.Fatalf("redeem error=%v", err)
	}
	card, err := repo.CardByID(ctx, cardID)
	if err != nil {
		t.Fatal(err)
	}
	if card.Status != "unused" || card.RedeemedAt != nil || card.ExpiresAt != nil {
		t.Fatalf("failed redemption consumed card: %+v", card)
	}
	if count, err := repo.ActiveAllocationCount(ctx, accountID); err != nil || count != 1 {
		t.Fatalf("allocations=%d err=%v", count, err)
	}
}

func TestExpiredAccountRetirementDoesNotWaitForGrace(t *testing.T) {
	for _, beforeExpiry := range []bool{true, false} {
		name := "migrated_after_expiry"
		if beforeExpiry {
			name = "legacy_grace_on_expired_account"
		}
		t.Run(name, func(t *testing.T) {
			db := openStore(t)
			defer db.Close()
			repo := New(db.DB(), testCredentialKeyring(t))
			ctx := context.Background()
			start := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
			now := start
			repo.SetNow(func() time.Time { return now })
			oldID, err := repo.CreateAccount(ctx, AccountSeed{DisplayUsername: "expired-old", DisplayPassword: "old-password", DisplayTOTPSecret: "old-totp", AccountExpiry: start.Add(30 * time.Hour), MaxConcurrentUsers: 1, MonitorStatus: "alive"})
			if err != nil {
				t.Fatal(err)
			}
			cardID, err := repo.CreateCard(ctx, CardSeed{CodeHash: cardHashForCode("2345-6789-ABCD"), CodeSuffix: "ABCD", DurationDays: 30})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := repo.RedeemCard(ctx, cardID); err != nil {
				t.Fatal(err)
			}
			newID, err := repo.CreateAccount(ctx, AccountSeed{DisplayUsername: "replacement", DisplayPassword: "new-password", DisplayTOTPSecret: "new-totp", AccountExpiry: start.Add(90 * 24 * time.Hour), MaxConcurrentUsers: 1, MonitorStatus: "alive"})
			if err != nil {
				t.Fatal(err)
			}
			if beforeExpiry {
				now = start.Add(7 * time.Hour)
				run, err := repo.ProcessReplacements(ctx, now)
				if err != nil {
					t.Fatal(err)
				}
				if len(run.Replaced) != 1 || run.Replaced[0].GraceUntil == nil || len(run.Retired) != 0 {
					t.Fatalf("before-expiry run=%+v", run)
				}
			}
			now = start.Add(30 * time.Hour)
			run, err := repo.ProcessReplacements(ctx, now)
			if err != nil {
				t.Fatal(err)
			}
			if len(run.Retired) != 1 || run.Retired[0].AccountID != oldID {
				t.Fatalf("expired run=%+v", run)
			}
			if beforeExpiry {
				if run.GraceExpired != 1 {
					t.Fatalf("expired grace cleanup=%d", run.GraceExpired)
				}
			} else if len(run.Replaced) != 1 || run.Replaced[0].Reason != ReplacementReasonExpired || run.Replaced[0].GraceUntil != nil {
				t.Fatalf("expired replacement incorrectly retains grace: %+v", run)
			}
			var archived sql.NullString
			var status string
			var passwordSize int
			if err := db.DB().QueryRow("SELECT status,archived_at,length(display_password_secret) FROM chatgpt_accounts WHERE id=?", oldID).Scan(&status, &archived, &passwordSize); err != nil {
				t.Fatal(err)
			}
			if status != "disabled" || !archived.Valid || passwordSize != 0 {
				t.Fatalf("old account status=%s archived=%v password_size=%d", status, archived, passwordSize)
			}
			var primaryAccount int64
			if err := db.DB().QueryRow("SELECT account_id FROM allocations WHERE card_id=? AND allocation_state='primary' AND active=1", cardID).Scan(&primaryAccount); err != nil {
				t.Fatal(err)
			}
			if primaryAccount != newID {
				t.Fatalf("primary account=%d want %d", primaryAccount, newID)
			}
			if run, err := repo.ProcessReplacements(ctx, now); err != nil || len(run.Retired) != 0 || run.GraceExpired != 0 {
				t.Fatalf("repeat scan=%+v err=%v", run, err)
			}
		})
	}
}

func TestMonitorSyncDoesNotReopenDisabledOrBannedAccounts(t *testing.T) {
	for _, status := range []string{"available", "full", "disabled", "banned"} {
		t.Run(status, func(t *testing.T) {
			db := openStore(t)
			defer db.Close()
			repo := New(db.DB(), testCredentialKeyring(t))
			ctx := context.Background()
			now := time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)
			repo.SetNow(func() time.Time { return now })
			id, err := repo.CreateAccount(ctx, AccountSeed{DisplayUsername: "sync-state", DisplayPassword: "test-password", DisplayTOTPSecret: "test-totp", AccountExpiry: now.Add(time.Hour), MaxConcurrentUsers: 5})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.DB().Exec("UPDATE chatgpt_accounts SET status=? WHERE id=?", status, id); err != nil {
				t.Fatal(err)
			}
			account, err := repo.UpdateAccountMonitorStatus(ctx, id, "monitor-test", "alive")
			if err != nil || account.Status != status {
				t.Fatalf("account=%+v err=%v", account, err)
			}
		})
	}
}

func TestExpiredAccountIsMarkedUnavailableBeforeGraceRetirement(t *testing.T) {
	db := openStore(t)
	defer db.Close()
	repo := New(db.DB(), testCredentialKeyring(t))
	ctx := context.Background()
	start := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC)
	now := start
	repo.SetNow(func() time.Time { return now })
	accountID, err := repo.CreateAccount(ctx, AccountSeed{
		DisplayUsername: "expired-with-active-user", DisplayPassword: "test-password", DisplayTOTPSecret: "test-totp",
		AccountExpiry: start.Add(24 * time.Hour), MaxConcurrentUsers: 1, MonitorStatus: "alive",
	})
	if err != nil {
		t.Fatal(err)
	}
	cardID, err := repo.CreateCard(ctx, CardSeed{CodeHash: cardHashForCode("KLMN-PQRS-TUVW"), CodeSuffix: "TUVW", DurationDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RedeemCard(ctx, cardID); err != nil {
		t.Fatal(err)
	}
	now = start.Add(24 * time.Hour)
	run, err := repo.ProcessReplacements(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Retired) != 0 {
		t.Fatalf("account with active allocation retired too early: %+v", run.Retired)
	}
	account, err := repo.Account(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != "expired" || account.CurrentAllocations != 1 {
		t.Fatalf("account=%+v want expired with allocation retained during migration", account)
	}
}
