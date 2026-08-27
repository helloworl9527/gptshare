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
	var pendingID int64
	if err := database.DB().QueryRow("SELECT id FROM chatgpt_accounts WHERE monitor_account_id='monitor-pending'").Scan(&pendingID); err != nil {
		t.Fatal(err)
	}
	pickupAddress := "locker-pending"
	if _, err := repo.UpdateAccount(ctx, pendingID, AccountUpdate{
		DisplayUsername: "pending@example.test", DisplayPassword: "pending-password", PickupAddress: &pickupAddress,
		AccountExpiry: now.Add(20 * 24 * time.Hour), MaxConcurrentUsers: 3, Status: "pending_credentials", MonitorStatus: "alive", MonitorAccountID: "monitor-pending",
	}); err != nil {
		t.Fatal(err)
	}
	updatedPending, err := repo.ApplyMonitorAccountEvent(ctx, accountsync.Event{
		EventID: "event-pending-updated", Version: 2, Type: accountsync.EventAccountUpdated, OccurredAt: now,
		ProviderAccountID: "monitor-pending", Email: "pending@example.test", Plan: "plus",
		SubscriptionExpiry: now.Add(20 * 24 * time.Hour), Status: "alive",
	})
	if err != nil || updatedPending.Disposition != "applied" {
		t.Fatalf("updated pending event=%+v err=%v", updatedPending, err)
	}
	if err := database.DB().QueryRow("SELECT status FROM chatgpt_accounts WHERE id=?", pendingID).Scan(&pendingStatus); err != nil || pendingStatus != "available" {
		t.Fatalf("pickup address should satisfy credentials status=%q err=%v", pendingStatus, err)
	}
	// Keep this fixture out of the later account-replacement scenario.
	if _, err := database.DB().Exec("UPDATE chatgpt_accounts SET status='disabled' WHERE id=?", pendingID); err != nil {
		t.Fatal(err)
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

// 只导入付费账号，所以 plan=free 就是订阅终止：顾客立刻迁走、账号当场下线，
// 而且不能留宽限期（旧号已经没有会员，留着等于让顾客继续用免费号）。
func TestFreePlanEventMigratesCustomersAndRetiresAccount(t *testing.T) {
	ctx := context.Background()
	database := openStore(t)
	defer database.Close()
	repo := New(database.DB(), testCredentialKeyring(t))
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return now })

	downgradedID, err := repo.CreateAccount(ctx, AccountSeed{
		DisplayUsername: "downgraded@example.test", DisplayPassword: "down-password", DisplayTOTPSecret: "down-totp",
		AccountExpiry: now.Add(10 * 24 * time.Hour), MaxConcurrentUsers: 1, MonitorAccountID: "monitor-downgraded",
		MonitorStatus: "alive", Status: "available",
	})
	if err != nil {
		t.Fatal(err)
	}
	spareID, err := repo.CreateAccount(ctx, AccountSeed{
		DisplayUsername: "spare@example.test", DisplayPassword: "spare-password", DisplayTOTPSecret: "spare-totp",
		AccountExpiry: now.Add(20 * 24 * time.Hour), MaxConcurrentUsers: 2, MonitorStatus: "alive", Status: "available",
	})
	if err != nil {
		t.Fatal(err)
	}
	// 7 天卡：选号按"账号到期日最贴合卡有效期"排序，这样才会落在 downgraded 账号上。
	redeemed, err := redeemCodeForTest(t, repo, "FREE-DOWN-0001", 7, true)
	if err != nil || redeemed.Account.ID != downgradedID {
		t.Fatalf("redeemed account=%d downgraded=%d err=%v", redeemed.Account.ID, downgradedID, err)
	}

	// 监控侧发来的降级事件：plan=free，订阅到期时间就是观测时刻。
	result, err := repo.ApplyMonitorAccountEvent(ctx, accountsync.Event{
		EventID: "event-downgraded", Version: 1, Type: accountsync.EventAccountUpdated, OccurredAt: now,
		ProviderAccountID: "monitor-downgraded", Email: "downgraded@example.test", Plan: accountsync.PlanFree,
		SubscriptionExpiry: now, Status: "dead_normal",
	})
	if err != nil || result.Migrated != 1 || result.Pending != 0 || !result.Retired {
		t.Fatalf("downgraded=%+v err=%v", result, err)
	}

	var status string
	var archivedAt sql.NullString
	var allocations int
	if err := database.DB().QueryRow("SELECT status,archived_at,current_allocations FROM chatgpt_accounts WHERE id=?", downgradedID).
		Scan(&status, &archivedAt, &allocations); err != nil {
		t.Fatal(err)
	}
	if status != "disabled" || !archivedAt.Valid || allocations != 0 {
		t.Fatalf("status=%s archived=%v allocations=%d", status, archivedAt.Valid, allocations)
	}
	// 顾客必须已经在新账号上，且旧分配不能还挂着宽限期。
	var newAccountID int64
	if err := database.DB().QueryRow("SELECT account_id FROM allocations WHERE card_id=? AND active=1 AND allocation_state='primary'", redeemed.Card.ID).
		Scan(&newAccountID); err != nil {
		t.Fatal(err)
	}
	if newAccountID != spareID {
		t.Fatalf("new_account=%d spare=%d", newAccountID, spareID)
	}
	var lingering int
	if err := database.DB().QueryRow("SELECT count(*) FROM allocations WHERE account_id=? AND active=1", downgradedID).Scan(&lingering); err != nil {
		t.Fatal(err)
	}
	if lingering != 0 {
		t.Fatalf("downgraded account still holds %d active allocations", lingering)
	}
	// 归因必须是订阅终止而不是封禁：封禁样本要保持干净。
	var reason string
	if err := database.DB().QueryRow("SELECT reason FROM replacement_history WHERE card_id=?", redeemed.Card.ID).Scan(&reason); err != nil {
		t.Fatal(err)
	}
	if reason != ReplacementReasonExpired {
		t.Fatalf("reason=%s want=%s", reason, ReplacementReasonExpired)
	}
}

// 监控状态还没翻成 dead_normal、订阅到期时间也还在未来，但 plan 已经是 free：
// 只看状态和到期时间会把它判成 available 继续分配，必须靠 plan 拦下来。
func TestFreePlanEventRetiresEvenWhenMonitorStatusStillAlive(t *testing.T) {
	ctx := context.Background()
	database := openStore(t)
	defer database.Close()
	repo := New(database.DB(), testCredentialKeyring(t))
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return now })

	if _, err := repo.CreateAccount(ctx, AccountSeed{
		DisplayUsername: "stillalive@example.test", DisplayPassword: "alive-password", DisplayTOTPSecret: "alive-totp",
		AccountExpiry: now.Add(20 * 24 * time.Hour), MaxConcurrentUsers: 2, MonitorAccountID: "monitor-stillalive",
		MonitorStatus: "alive", Status: "available",
	}); err != nil {
		t.Fatal(err)
	}
	result, err := repo.ApplyMonitorAccountEvent(ctx, accountsync.Event{
		EventID: "event-stillalive", Version: 1, Type: accountsync.EventAccountUpdated, OccurredAt: now,
		ProviderAccountID: "monitor-stillalive", Email: "stillalive@example.test", Plan: accountsync.PlanFree,
		SubscriptionExpiry: now.Add(20 * 24 * time.Hour), Status: "alive",
	})
	if err != nil || !result.Retired {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	var status string
	var archivedAt sql.NullString
	if err := database.DB().QueryRow("SELECT status,archived_at FROM chatgpt_accounts WHERE monitor_account_id='monitor-stillalive'").
		Scan(&status, &archivedAt); err != nil {
		t.Fatal(err)
	}
	if status != "disabled" || !archivedAt.Valid {
		t.Fatalf("status=%s archived=%v", status, archivedAt.Valid)
	}
}
