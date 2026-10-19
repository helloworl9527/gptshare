package repository

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"allocation-service/accountsync"
)

func TestExpiredCleanupGuardRetainsPoolHistoryAndRejectsLateEvents(t *testing.T) {
	db := openStore(t)
	defer db.Close()
	repo := New(db.DB(), testCredentialKeyring(t))
	ctx := context.Background()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return now })
	id, err := repo.CreateAccount(ctx, AccountSeed{DisplayUsername: "expired-cleanup", DisplayPassword: "synthetic-password", DisplayTOTPSecret: "synthetic-totp", AccountExpiry: now.Add(time.Hour), MonitorAccountID: "cleanup-provider", MonitorStatus: "alive", MaxConcurrentUsers: 1})
	if err != nil {
		t.Fatal(err)
	}
	if safe, err := repo.PrepareExpiredAccountCleanup(ctx, "cleanup-provider", now); err != nil || safe {
		t.Fatalf("unexpired account safe=%v err=%v", safe, err)
	}
	if _, err := db.DB().Exec(`UPDATE chatgpt_accounts SET status='expired',monitor_status='dead_normal' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if safe, err := repo.PrepareExpiredAccountCleanup(ctx, "cleanup-provider", now); err != nil || safe {
		t.Fatalf("future expiry with stale terminal status safe=%v err=%v", safe, err)
	}
	now = now.Add(2 * time.Hour)
	if safe, err := repo.PrepareExpiredAccountCleanup(ctx, "cleanup-provider", now); err != nil || !safe {
		t.Fatalf("expired safe=%v err=%v", safe, err)
	}
	var archived sql.NullString
	var password, totp int
	if err := db.DB().QueryRow(`SELECT archived_at,length(display_password_secret),length(display_2fa_secret) FROM chatgpt_accounts WHERE id=?`, id).Scan(&archived, &password, &totp); err != nil || !archived.Valid || password != 0 || totp != 0 {
		t.Fatalf("archive=%v password=%d totp=%d err=%v", archived, password, totp, err)
	}
	if safe, err := repo.PrepareExpiredAccountCleanup(ctx, "cleanup-provider", now); err != nil || !safe {
		t.Fatalf("idempotent guard=%v err=%v", safe, err)
	}
	result, err := repo.ApplyMonitorAccountEvent(ctx, accountsync.Event{EventID: "late-cleanup-event", Version: 1, Type: accountsync.EventAccountUpdated, OccurredAt: now, ProviderAccountID: "cleanup-provider", Email: "cleanup@example.test", Plan: "plus", SubscriptionExpiry: now.Add(24 * time.Hour), Status: "alive"})
	if err != nil || result.Disposition != "ignored_archived" {
		t.Fatalf("late event=%+v err=%v", result, err)
	}
	if safe, err := repo.PrepareExpiredAccountCleanup(ctx, "monitor-only", now); err != nil || !safe {
		t.Fatalf("monitor-only=%v err=%v", safe, err)
	}
	result, err = repo.ApplyMonitorAccountEvent(ctx, accountsync.Event{EventID: "late-monitor-only-event", Version: 1, Type: accountsync.EventAccountCreated, OccurredAt: now, ProviderAccountID: "monitor-only", Email: "monitor-only@example.test", Plan: "plus", SubscriptionExpiry: now.Add(24 * time.Hour), Status: "alive"})
	if err != nil || result.Disposition != "ignored_archived" {
		t.Fatalf("monitor-only tombstone=%+v err=%v", result, err)
	}
	if _, _, err := repo.UpsertSyncedAccount(ctx, SyncedAccount{MonitorAccountID: "monitor-only", DisplayUsername: "late-facade", AccountExpiry: now.Add(24 * time.Hour), MonitorStatus: "alive"}); err != ErrAccountArchived {
		t.Fatalf("stale facade bypassed tombstone: %v", err)
	}
	if safe, err := repo.PrepareExpiredAccountCleanup(ctx, "", now); err != nil || safe {
		t.Fatalf("empty provider=%v err=%v", safe, err)
	}
}

func TestExpiredCleanupGuardProtectsActiveAndGraceAllocations(t *testing.T) {
	for _, state := range []string{"primary", "grace"} {
		t.Run(state, func(t *testing.T) {
			db := openStore(t)
			defer db.Close()
			repo := New(db.DB(), testCredentialKeyring(t))
			ctx := context.Background()
			now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
			repo.SetNow(func() time.Time { return now })
			id, err := repo.CreateAccount(ctx, AccountSeed{DisplayUsername: "occupied", DisplayPassword: "synthetic-password", DisplayTOTPSecret: "synthetic-totp", AccountExpiry: now.Add(time.Hour), MonitorAccountID: "occupied-provider", MonitorStatus: "alive", MaxConcurrentUsers: 1})
			if err != nil {
				t.Fatal(err)
			}
			card, err := repo.CreateCard(ctx, CardSeed{CodeHash: hashFor(301), CodeSuffix: "TEST", DurationDays: 7})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := repo.RedeemCard(ctx, card); err != nil {
				t.Fatal(err)
			}
			if state == "grace" {
				if _, err := db.DB().Exec(`UPDATE allocations SET allocation_state='grace',grace_until=?,superseded_by_allocation_id=id WHERE card_id=?`, formatTime(now.Add(24*time.Hour)), card); err != nil {
					t.Fatal(err)
				}
			}
			now = now.Add(2 * time.Hour)
			if safe, err := repo.PrepareExpiredAccountCleanup(ctx, "occupied-provider", now); err != nil || safe {
				t.Fatalf("occupied safe=%v err=%v", safe, err)
			}
			var archived sql.NullString
			if err := db.DB().QueryRow("SELECT archived_at FROM chatgpt_accounts WHERE id=?", id).Scan(&archived); err != nil || archived.Valid {
				t.Fatalf("occupied account archived=%v err=%v", archived, err)
			}
			// A stale counter must not override the real allocations table.
			if _, err := db.DB().Exec("UPDATE chatgpt_accounts SET current_allocations=0,archived_at=? WHERE id=?", formatTime(now), id); err != nil {
				t.Fatal(err)
			}
			if safe, err := repo.PrepareExpiredAccountCleanup(ctx, "occupied-provider", now); err != nil || safe {
				t.Fatalf("archived with customer safe=%v err=%v", safe, err)
			}
			if _, err := db.DB().Exec("UPDATE allocations SET active=0,allocation_state='revoked' WHERE card_id=?", card); err != nil {
				t.Fatal(err)
			}
			if safe, err := repo.PrepareExpiredAccountCleanup(ctx, "occupied-provider", now); err != nil || !safe {
				t.Fatalf("after migration safe=%v err=%v", safe, err)
			}
			var cards, allocations int
			if err := db.DB().QueryRow("SELECT count(*) FROM cards").Scan(&cards); err != nil {
				t.Fatal(err)
			}
			if err := db.DB().QueryRow("SELECT count(*) FROM allocations").Scan(&allocations); err != nil {
				t.Fatal(err)
			}
			if cards != 1 || allocations != 1 {
				t.Fatal("card/allocation history deleted")
			}
		})
	}
}
