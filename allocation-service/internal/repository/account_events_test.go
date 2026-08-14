package repository

import (
	"context"
	"testing"
	"time"

	"allocation-service/accountsync"
)

func TestApplyMonitorAccountEventCreatesPendingAndMigratesBannedAccount(t *testing.T) {
	ctx := context.Background()
	database := openStore(t)
	defer database.Close()
	repo := New(database.DB(), testCredentialKeyring(t))
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return now })

	created, err := repo.ApplyMonitorAccountEvent(ctx, accountsync.Event{
		EventID: "event-created", Version: 1, Type: accountsync.EventAccountCreated, OccurredAt: now,
		ProviderAccountID: "monitor-pending", Email: "pending@example.test", Plan: "plus",
		SubscriptionExpiry: now.Add(20 * 24 * time.Hour), Status: "alive",
	})
	if err != nil || !created.Created || created.Disposition != "applied" {
		t.Fatalf("created=%+v err=%v", created, err)
	}
	var pendingStatus string
	if err := database.DB().QueryRow("SELECT status FROM chatgpt_accounts WHERE monitor_account_id='monitor-pending'").Scan(&pendingStatus); err != nil || pendingStatus != "pending_credentials" {
		t.Fatalf("pending status=%q err=%v", pendingStatus, err)
	}

	oldID, err := repo.CreateAccount(ctx, AccountSeed{
		DisplayUsername: "old@example.test", DisplayPassword: "old-password", DisplayTOTPSecret: "old-totp",
		AccountExpiry: now.Add(10 * 24 * time.Hour), MaxConcurrentUsers: 1, MonitorAccountID: "monitor-old", MonitorStatus: "alive", Status: "available",
	})
	if err != nil {
		t.Fatal(err)
	}
	targetID, err := repo.CreateAccount(ctx, AccountSeed{
		DisplayUsername: "target@example.test", DisplayPassword: "target-password", DisplayTOTPSecret: "target-totp",
		AccountExpiry: now.Add(20 * 24 * time.Hour), MaxConcurrentUsers: 2, MonitorStatus: "alive", Status: "available",
	})
	if err != nil {
		t.Fatal(err)
	}
	redeemed, err := redeemCodeForTest(t, repo, "SYNC-EVENT-0001", 7, true)
	if err != nil || redeemed.Account.ID != oldID {
		t.Fatalf("redeemed account=%d old=%d err=%v", redeemed.Account.ID, oldID, err)
	}
	banEvent := accountsync.Event{
		EventID: "event-banned", Version: 1, Type: accountsync.EventAccountBanned, OccurredAt: now,
		ProviderAccountID: "monitor-old", Email: "old@example.test", Plan: "plus",
		SubscriptionExpiry: now.Add(10 * 24 * time.Hour), Status: "dead_banned",
	}
	banned, err := repo.ApplyMonitorAccountEvent(ctx, banEvent)
	if err != nil || banned.Migrated != 1 || banned.Pending != 0 {
		t.Fatalf("banned=%+v err=%v", banned, err)
	}
	var oldStatus string
	var replacementAccountID int64
	if err := database.DB().QueryRow("SELECT status FROM chatgpt_accounts WHERE id=?", oldID).Scan(&oldStatus); err != nil {
		t.Fatal(err)
	}
	if err := database.DB().QueryRow("SELECT account_id FROM allocations WHERE card_id=? AND active=1 AND allocation_state='primary'", redeemed.Card.ID).Scan(&replacementAccountID); err != nil {
		t.Fatal(err)
	}
	if oldStatus != "banned" || replacementAccountID != targetID {
		t.Fatalf("old_status=%s replacement=%d target=%d", oldStatus, replacementAccountID, targetID)
	}
	duplicate, err := repo.ApplyMonitorAccountEvent(ctx, banEvent)
	if err != nil || duplicate.Disposition != "duplicate" {
		t.Fatalf("duplicate=%+v err=%v", duplicate, err)
	}
}
