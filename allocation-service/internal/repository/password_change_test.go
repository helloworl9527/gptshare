package repository

import (
	"context"
	"testing"
	"time"
)

// 看板标记规则：乘客订阅已过期、但其所在的 GPT 帐户订阅还剩超过 3 天时，
// 该帐户应出现在"需要更改密码"列表中；乘客仍在订阅期内、或帐户剩余不足 3 天时不出现。
func TestListPasswordChangeDueFlagsExpiredCardOnLiveAccount(t *testing.T) {
	db := openStore(t)
	defer db.Close()
	repo := New(db.DB(), testCredentialKeyring(t))
	ctx := context.Background()
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return start })

	accountID, err := repo.CreateAccount(ctx, AccountSeed{
		DisplayUsername: "pw-flag", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp",
		AccountExpiry: start.Add(20 * 24 * time.Hour), MaxConcurrentUsers: 5, MonitorStatus: "alive",
	})
	if err != nil {
		t.Fatal(err)
	}
	cardID, err := repo.CreateCard(ctx, CardSeed{CodeHash: cardHashForCode("PWFL-AG01"), CodeSuffix: "AG01", DurationDays: 7})
	if err != nil {
		t.Fatal(err)
	}
	allocation, err := repo.RedeemCard(ctx, cardID)
	if err != nil {
		t.Fatal(err)
	}
	if allocation.AccountID != accountID {
		t.Fatalf("redeemed onto account %d, want %d", allocation.AccountID, accountID)
	}

	// 乘客仍在订阅期内（起始时刻），不标记。
	if due, err := repo.ListPasswordChangeDue(ctx, start); err != nil {
		t.Fatal(err)
	} else if len(due) != 0 {
		t.Fatalf("active subscription should not be flagged: %+v", due)
	}

	// 卡密到期（第 8 天）后，帐户还剩 12 天 > 3 天，应标记。
	due, err := repo.ListPasswordChangeDue(ctx, start.Add(8*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 {
		t.Fatalf("expired card on live account = %d entries, want 1", len(due))
	}
	if due[0].ID != accountID || due[0].DisplayUsername != "pw-flag" {
		t.Fatalf("flagged account = %d/%q, want %d/pw-flag", due[0].ID, due[0].DisplayUsername, accountID)
	}
	if due[0].ExpiredCardCount != 1 {
		t.Fatalf("expired_card_count = %d, want 1", due[0].ExpiredCardCount)
	}
	if due[0].DaysAccountRemains != 12 {
		t.Fatalf("days_account_remains = %d, want 12", due[0].DaysAccountRemains)
	}
	if due[0].ClearedAt != nil {
		t.Fatalf("cleared_at should be unset before the operator acts: %v", due[0].ClearedAt)
	}
}

// 边界：帐户订阅剩余必须严格大于 3 天，正好 3 天不标记。
func TestListPasswordChangeDueRequiresStrictlyMoreThanThreeDays(t *testing.T) {
	db := openStore(t)
	defer db.Close()
	repo := New(db.DB(), testCredentialKeyring(t))
	ctx := context.Background()
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return now })

	exact, err := repo.CreateAccount(ctx, AccountSeed{
		DisplayUsername: "pw-exact", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp",
		AccountExpiry: now.Add(3 * 24 * time.Hour), MaxConcurrentUsers: 5, MonitorStatus: "alive",
	})
	if err != nil {
		t.Fatal(err)
	}
	over, err := repo.CreateAccount(ctx, AccountSeed{
		DisplayUsername: "pw-over", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp",
		AccountExpiry: now.Add(3*24*time.Hour + time.Hour), MaxConcurrentUsers: 5, MonitorStatus: "alive",
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, username := range []string{"pw-exact", "pw-over"} {
		cardID, err := repo.CreateCard(ctx, CardSeed{
			CodeHash: cardHashForCode("PWBD-" + username + "-0001"), CodeSuffix: "000" + string(rune('1'+i)), DurationDays: 7,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repo.RedeemCard(ctx, cardID); err != nil {
			t.Fatal(err)
		}
	}
	// 把两张卡都改成已过期。
	if _, err := db.DB().ExecContext(ctx, `UPDATE cards SET expires_at=?`, formatTime(now.Add(-24*time.Hour))); err != nil {
		t.Fatal(err)
	}

	due, err := repo.ListPasswordChangeDue(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].ID != over {
		t.Fatalf("flagged = %+v, want only account %d (pw-over)", due, over)
	}
	for _, item := range due {
		if item.ID == exact {
			t.Fatalf("account with exactly three days remaining must not be flagged")
		}
	}
}

// 标记已处理后应从列表消失；此后有更新的卡密再次过期会重新触发。
func TestMarkPasswordChangeClearedHidesUntilNextCardExpiry(t *testing.T) {
	db := openStore(t)
	defer db.Close()
	repo := New(db.DB(), testCredentialKeyring(t))
	ctx := context.Background()
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return start })

	accountID, err := repo.CreateAccount(ctx, AccountSeed{
		DisplayUsername: "pw-cleared", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp",
		AccountExpiry: start.Add(20 * 24 * time.Hour), MaxConcurrentUsers: 5, MonitorStatus: "alive",
	})
	if err != nil {
		t.Fatal(err)
	}
	cardID, err := repo.CreateCard(ctx, CardSeed{CodeHash: cardHashForCode("PWCL-EAR01"), CodeSuffix: "AR01", DurationDays: 7})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RedeemCard(ctx, cardID); err != nil {
		t.Fatal(err)
	}
	check := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	if due, err := repo.ListPasswordChangeDue(ctx, check); err != nil {
		t.Fatal(err)
	} else if len(due) != 1 {
		t.Fatalf("expected the account to be flagged, got %+v", due)
	}

	if err := repo.MarkPasswordChangeCleared(ctx, accountID, check); err != nil {
		t.Fatal(err)
	}
	if due, err := repo.ListPasswordChangeDue(ctx, check); err != nil {
		t.Fatal(err)
	} else if len(due) != 0 {
		t.Fatalf("cleared account should be hidden, got %+v", due)
	}

	// 又一张卡在"已处理"之后过期 → 重新触发标记。
	secondCard, err := repo.CreateCard(ctx, CardSeed{CodeHash: cardHashForCode("PWCL-AR02"), CodeSuffix: "AR02", DurationDays: 7})
	if err != nil {
		t.Fatal(err)
	}
	later := check.Add(time.Hour)
	if _, err := db.DB().ExecContext(ctx, `UPDATE cards SET status='redeemed',redeemed_at=?,expires_at=?,updated_at=? WHERE id=?`,
		formatTime(later), formatTime(later), formatTime(later), secondCard); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `INSERT INTO allocations
		(card_id,account_id,allocated_at,valid_until,allocation_state,active,created_at,updated_at)
		VALUES (?,?,?,?,'primary',1,?,?)`,
		secondCard, accountID, formatTime(later), formatTime(later),
		formatTime(later), formatTime(later)); err != nil {
		t.Fatal(err)
	}
	due, err := repo.ListPasswordChangeDue(ctx, check.Add(24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(due) != 1 || due[0].ID != accountID {
		t.Fatalf("a newer expired card should re-arm the flag, got %+v", due)
	}
	if due[0].ExpiredCardCount != 2 {
		t.Fatalf("expired_card_count = %d, want 2", due[0].ExpiredCardCount)
	}
}

// 已归档的帐户不应出现在列表里；对不存在的帐户标记"已处理"应返回 sql.ErrNoRows。
func TestMarkPasswordChangeClearedRejectsMissingOrArchived(t *testing.T) {
	db := openStore(t)
	defer db.Close()
	repo := New(db.DB(), testCredentialKeyring(t))
	ctx := context.Background()
	now := time.Date(2026, 8, 20, 0, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return now })

	if err := repo.MarkPasswordChangeCleared(ctx, 987654, now); err == nil {
		t.Fatal("expected an error when clearing a non-existent account")
	}
	archivedID, err := repo.CreateAccount(ctx, AccountSeed{
		DisplayUsername: "pw-archived", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp",
		AccountExpiry: now.Add(20 * 24 * time.Hour), MaxConcurrentUsers: 5, MonitorStatus: "alive",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().ExecContext(ctx, `UPDATE chatgpt_accounts SET archived_at=? WHERE id=?`, formatTime(now), archivedID); err != nil {
		t.Fatal(err)
	}
	if err := repo.MarkPasswordChangeCleared(ctx, archivedID, now); err == nil {
		t.Fatal("expected an error when clearing an archived account")
	}
}