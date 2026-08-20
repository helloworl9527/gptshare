package repository

import (
	"context"
	"database/sql"
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
	if err != nil || banned.Migrated != 1 || banned.Pending != 0 || !banned.Retired {
		t.Fatalf("banned=%+v err=%v", banned, err)
	}
	var oldStatus string
	var oldArchived sql.NullString
	var oldAllocations int
	var replacementAccountID int64
	if err := database.DB().QueryRow("SELECT status,archived_at,current_allocations FROM chatgpt_accounts WHERE id=?", oldID).
		Scan(&oldStatus, &oldArchived, &oldAllocations); err != nil {
		t.Fatal(err)
	}
	if err := database.DB().QueryRow("SELECT account_id FROM allocations WHERE card_id=? AND active=1 AND allocation_state='primary'", redeemed.Card.ID).Scan(&replacementAccountID); err != nil {
		t.Fatal(err)
	}
	// 顾客迁走后账号必须自动下线：归档 + disabled + 容量清零。
	if oldStatus != "disabled" || !oldArchived.Valid || oldAllocations != 0 || replacementAccountID != targetID {
		t.Fatalf("old_status=%s archived=%v allocations=%d replacement=%d target=%d", oldStatus, oldArchived.Valid, oldAllocations, replacementAccountID, targetID)
	}
	// 账号下线后监控侧再来的事件不能变成永久重投的投递失败。
	followUp, err := repo.ApplyMonitorAccountEvent(ctx, accountsync.Event{
		EventID: "event-after-retire", Version: 2, Type: accountsync.EventAccountUpdated, OccurredAt: now,
		ProviderAccountID: "monitor-old", Email: "old@example.test", Plan: "plus",
		SubscriptionExpiry: now.Add(10 * 24 * time.Hour), Status: "alive",
	})
	if err != nil || followUp.Disposition != "ignored_archived" {
		t.Fatalf("follow-up=%+v err=%v", followUp, err)
	}
	duplicate, err := repo.ApplyMonitorAccountEvent(ctx, banEvent)
	if err != nil || duplicate.Disposition != "duplicate" {
		t.Fatalf("duplicate=%+v err=%v", duplicate, err)
	}
}

// 疑似封禁：退出分配池，但存量顾客、状态和账号本身都不受影响。
func TestSuspectedAccountLeavesAllocationPoolWithoutTouchingCustomers(t *testing.T) {
	ctx := context.Background()
	database := openStore(t)
	defer database.Close()
	repo := New(database.DB(), testCredentialKeyring(t))
	now := time.Date(2026, 8, 20, 12, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return now })

	suspectID, err := repo.CreateAccount(ctx, AccountSeed{
		DisplayUsername: "suspect@example.test", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp",
		AccountExpiry: now.Add(30 * 24 * time.Hour), MaxConcurrentUsers: 5, MonitorAccountID: "monitor-suspect",
		MonitorStatus: "alive", Status: "available",
	})
	if err != nil {
		t.Fatal(err)
	}
	existing, err := redeemCodeForTest(t, repo, "SUSP-ECTA-0001", 30, true)
	if err != nil || existing.Account.ID != suspectID {
		t.Fatalf("redeemed account=%d suspect=%d err=%v", existing.Account.ID, suspectID, err)
	}

	suspectEvent := accountsync.Event{
		EventID: "suspect-on", Version: 1, Type: accountsync.EventAccountUpdated, OccurredAt: now,
		ProviderAccountID: "monitor-suspect", Email: "suspect@example.test", Plan: "plus",
		SubscriptionExpiry: now.Add(30 * 24 * time.Hour), Status: "alive", Suspected: true,
	}
	if _, err := repo.ApplyMonitorAccountEvent(ctx, suspectEvent); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := database.DB().QueryRow("SELECT status FROM chatgpt_accounts WHERE id=?", suspectID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "suspected" {
		t.Fatalf("status=%s want suspected", status)
	}

	// 存量顾客原样留在疑似账号上。
	var state string
	var accountID int64
	if err := database.DB().QueryRow("SELECT allocation_state,account_id FROM allocations WHERE card_id=? AND active=1", existing.Card.ID).
		Scan(&state, &accountID); err != nil {
		t.Fatal(err)
	}
	if state != "primary" || accountID != suspectID {
		t.Fatalf("existing allocation state=%s account=%d want primary on %d", state, accountID, suspectID)
	}

	// 新顾客不会再被分到疑似账号上。
	spareID, err := repo.CreateAccount(ctx, AccountSeed{
		DisplayUsername: "spare@example.test", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp",
		AccountExpiry: now.Add(30 * 24 * time.Hour), MaxConcurrentUsers: 5, MonitorStatus: "alive", Status: "available",
	})
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := redeemCodeForTest(t, repo, "SUSP-ECTA-0002", 30, true)
	if err != nil || fresh.Account.ID != spareID {
		t.Fatalf("new customer landed on account %d want %d err=%v", fresh.Account.ID, spareID, err)
	}

	// 疑似阶段不迁移、不下线。
	run, err := repo.ProcessReplacements(ctx, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Replaced) != 0 || run.Failed != 0 || len(run.Retired) != 0 {
		t.Fatalf("suspicion must not migrate or retire anything: %+v", run)
	}
	var archivedAt sql.NullString
	if err := database.DB().QueryRow("SELECT archived_at FROM chatgpt_accounts WHERE id=?", suspectID).Scan(&archivedAt); err != nil {
		t.Fatal(err)
	}
	if archivedAt.Valid {
		t.Fatal("suspected account must stay online")
	}

	// 监控恢复后解除疑似，重新参与分配。
	clearEvent := suspectEvent
	clearEvent.EventID = "suspect-off"
	clearEvent.Version = 2
	clearEvent.Suspected = false
	if _, err := repo.ApplyMonitorAccountEvent(ctx, clearEvent); err != nil {
		t.Fatal(err)
	}
	if err := database.DB().QueryRow("SELECT status FROM chatgpt_accounts WHERE id=?", suspectID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "available" {
		t.Fatalf("recovered status=%s want available", status)
	}
}
