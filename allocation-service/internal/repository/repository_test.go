package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"allocation-service/internal/credential"
	"allocation-service/internal/store"

	"allocation-service/accountsync"
)

func TestRedeemThirtyConcurrentNoLocksNoOversell(t *testing.T) {
	db := openStore(t)
	defer db.Close()
	repo := New(db.DB(), testCredentialKeyring(t))
	now := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return now })
	accountID, err := repo.CreateAccount(context.Background(), AccountSeed{
		DisplayUsername: "pool-1", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp", AccountExpiry: now.Add(30 * 24 * time.Hour), MaxConcurrentUsers: 10, MonitorStatus: "alive", Status: "available",
	})
	if err != nil {
		t.Fatal(err)
	}
	cardIDs := make([]int64, 30)
	for i := range cardIDs {
		id, err := repo.CreateCard(context.Background(), CardSeed{CodeHash: hashFor(i), CodeSuffix: suffixFor(i), DurationDays: 30})
		if err != nil {
			t.Fatal(err)
		}
		cardIDs[i] = id
	}
	started := time.Now()
	var wg sync.WaitGroup
	results := make(chan error, len(cardIDs))
	for _, cardID := range cardIDs {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			_, err := repo.RedeemCard(context.Background(), id)
			if err != nil && strings.Contains(strings.ToLower(err.Error()), "database is locked") {
				results <- err
				return
			}
			results <- err
		}(cardID)
	}
	wg.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		if strings.Contains(strings.ToLower(err.Error()), "locked") {
			t.Fatalf("database lock error: %v", err)
		}
	}
	elapsed := time.Since(started)
	if successes != 10 {
		t.Fatalf("successes=%d want 10", successes)
	}
	if elapsed >= 3*time.Second {
		t.Fatalf("concurrent redeem exceeded budget: %s", elapsed)
	}
	account, err := repo.Account(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	activeCount, err := repo.ActiveAllocationCount(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	if account.CurrentAllocations != 10 || activeCount != 10 || account.CurrentAllocations > account.MaxConcurrentUsers {
		t.Fatalf("capacity mismatch account=%+v active=%d", account, activeCount)
	}
	var duplicatePrimaryCards int
	if err := db.DB().QueryRow(`SELECT count(*) FROM (
		SELECT card_id FROM allocations WHERE active=1 AND allocation_state='primary' GROUP BY card_id HAVING count(*) > 1
	)`).Scan(&duplicatePrimaryCards); err != nil {
		t.Fatal(err)
	}
	if duplicatePrimaryCards != 0 {
		t.Fatalf("duplicate active primary cards=%d", duplicatePrimaryCards)
	}
}

func TestListActiveAllocationsUsesStoredRelationshipsAndOrdering(t *testing.T) {
	db := openStore(t)
	defer db.Close()
	repo := New(db.DB(), testCredentialKeyring(t))
	ctx := context.Background()
	base := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return base })

	accountIDs := make([]int64, 4)
	for i, username := range []string{"first-account", "fund-outlier", "flagon_snap", "revoked-account"} {
		id, err := repo.CreateAccount(ctx, AccountSeed{
			DisplayUsername: username, DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp",
			AccountExpiry: base.Add(20 * 24 * time.Hour), MaxConcurrentUsers: 4, MonitorStatus: "alive", Status: "available",
		})
		if err != nil {
			t.Fatal(err)
		}
		accountIDs[i] = id
	}
	cardIDs := make([]int64, 4)
	for i := range cardIDs {
		id, err := repo.CreateCard(ctx, CardSeed{CodeHash: hashFor(700 + i), CodeSuffix: suffixFor(700 + i), DurationDays: 7})
		if err != nil {
			t.Fatal(err)
		}
		cardIDs[i] = id
	}

	insert := func(cardID, accountID int64, allocatedAt time.Time, state string, active int, graceUntil any, superseded any) int64 {
		t.Helper()
		result, err := db.DB().ExecContext(ctx, `INSERT INTO allocations
			(card_id,account_id,allocated_at,valid_until,grace_until,allocation_state,active,superseded_by_allocation_id,created_at,updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?)`,
			cardID, accountID, formatTime(allocatedAt), formatTime(allocatedAt.Add(7*24*time.Hour)), graceUntil,
			state, active, superseded, formatTime(allocatedAt), formatTime(allocatedAt))
		if err != nil {
			t.Fatal(err)
		}
		id, err := result.LastInsertId()
		if err != nil {
			t.Fatal(err)
		}
		return id
	}

	revokedID := insert(cardIDs[3], accountIDs[3], base.Add(4*time.Hour), "revoked", 0, nil, nil)
	primaryOld := insert(cardIDs[0], accountIDs[0], base.Add(time.Hour), "primary", 1, nil, nil)
	primaryNew := insert(cardIDs[2], accountIDs[2], base.Add(3*time.Hour), "primary", 1, nil, nil)
	graceTime := base.Add(10 * time.Hour)
	graceID := insert(cardIDs[1], accountIDs[1], base.Add(2*time.Hour), "grace", 1, formatTime(graceTime), primaryNew)

	views, err := repo.ListActiveAllocations(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 3 {
		t.Fatalf("active allocations=%d want 3: %+v", len(views), views)
	}
	wantIDs := []int64{primaryNew, graceID, primaryOld}
	wantAccounts := []int64{accountIDs[2], accountIDs[1], accountIDs[0]}
	for i, view := range views {
		if view.Allocation.ID != wantIDs[i] || view.Allocation.AccountID != wantAccounts[i] || view.Account.ID != wantAccounts[i] {
			t.Fatalf("view[%d]=%+v want allocation=%d account=%d", i, view, wantIDs[i], wantAccounts[i])
		}
		if view.Card.ID != view.Allocation.CardID || view.Card.CodeSuffix == "" || view.Account.DisplayUsername == "" {
			t.Fatalf("incomplete joined view[%d]=%+v", i, view)
		}
	}
	if views[1].Allocation.GraceUntil == nil || views[1].Allocation.AllocationState != "grace" {
		t.Fatalf("grace view=%+v", views[1])
	}
	for _, view := range views {
		if view.Allocation.ID == revokedID || !view.Allocation.Active {
			t.Fatalf("terminal allocation leaked into active list: %+v", view)
		}
	}
}

func TestConcurrentQueriesNoLocks(t *testing.T) {
	db := openStore(t)
	defer db.Close()
	repo := New(db.DB(), testCredentialKeyring(t))
	now := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return now })
	accountID, err := repo.CreateAccount(context.Background(), AccountSeed{
		DisplayUsername: "query-pool", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp", AccountExpiry: now.Add(30 * 24 * time.Hour), MaxConcurrentUsers: 30, MonitorStatus: "alive", Status: "available",
	})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 30)
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := repo.Account(context.Background(), accountID)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "database is locked") {
				t.Fatalf("database lock error: %v", err)
			}
			t.Fatal(err)
		}
	}
}

func TestAccountCredentialsEncryptedAndAADBound(t *testing.T) {
	db := openStore(t)
	defer db.Close()
	keyring := testCredentialKeyring(t)
	repo := New(db.DB(), keyring)
	now := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return now })
	accountID, err := repo.CreateAccount(context.Background(), AccountSeed{
		DisplayUsername: "leak-check", DisplayPassword: "LEAK_PASSWORD_SENTINEL", DisplayTOTPSecret: "LEAK_TOTP_SENTINEL", SourceURL: "https://accounts.example.test/LEAK_SOURCE_SENTINEL", AccountExpiry: now.Add(24 * time.Hour), MaxConcurrentUsers: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	passwordKeyID, passwordCiphertext, totpKeyID, totpCiphertext, err := repo.EncryptedCredentials(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(passwordCiphertext, []byte("LEAK_PASSWORD_SENTINEL")) || bytes.Contains(totpCiphertext, []byte("LEAK_TOTP_SENTINEL")) {
		t.Fatal("plaintext credential found in ciphertext")
	}
	password, err := keyring.Open(accountID, credential.CredentialPassword, passwordKeyID, passwordCiphertext)
	if err != nil {
		t.Fatal(err)
	}
	if string(password) != "LEAK_PASSWORD_SENTINEL" {
		t.Fatalf("password=%q", password)
	}
	if _, err := keyring.Open(accountID, credential.CredentialTOTP, passwordKeyID, passwordCiphertext); err == nil {
		t.Fatal("wrong AAD decrypted password")
	}
	if _, err := keyring.Open(accountID+1, credential.CredentialTOTP, totpKeyID, totpCiphertext); err == nil {
		t.Fatal("wrong account AAD decrypted totp")
	}
	var sourceKeyID string
	var sourceCiphertext []byte
	if err := db.DB().QueryRow(`SELECT source_url_key_id,source_url_secret FROM chatgpt_accounts WHERE id=?`, accountID).Scan(&sourceKeyID, &sourceCiphertext); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sourceCiphertext, []byte("LEAK_SOURCE_SENTINEL")) {
		t.Fatal("plaintext source URL found in ciphertext")
	}
	sourceURL, err := keyring.Open(accountID, credential.CredentialSourceURL, sourceKeyID, sourceCiphertext)
	if err != nil || string(sourceURL) != "https://accounts.example.test/LEAK_SOURCE_SENTINEL" {
		t.Fatalf("source URL=%q err=%v", sourceURL, err)
	}
	if _, err := keyring.Open(accountID, credential.CredentialPassword, sourceKeyID, sourceCiphertext); err == nil {
		t.Fatal("wrong AAD decrypted source URL")
	}
	account, err := repo.Account(context.Background(), accountID)
	if err != nil || account.SourceURL != "https://accounts.example.test/LEAK_SOURCE_SENTINEL" {
		t.Fatalf("account source=%q err=%v", account.SourceURL, err)
	}
}

func TestCardCodeEncryptedRevealAndAADBound(t *testing.T) {
	db := openStore(t)
	defer db.Close()
	keyring := testCredentialKeyring(t)
	repo := New(db.DB(), keyring)
	ctx := context.Background()
	code := "CDEF-HJKM-NPQR"
	cardID, err := repo.CreateCard(ctx, CardSeed{
		CodeHash: cardHashForCode(code), CodeSuffix: code[len(code)-4:], CodePlaintext: code, DurationDays: 30,
	})
	if err != nil {
		t.Fatal(err)
	}
	var keyID string
	var ciphertext []byte
	if err := db.DB().QueryRowContext(ctx, "SELECT encrypted_code_key_id,encrypted_code FROM cards WHERE id=?", cardID).Scan(&keyID, &ciphertext); err != nil {
		t.Fatal(err)
	}
	if keyID != keyring.ActiveKeyID() || len(ciphertext) == 0 || bytes.Contains(ciphertext, []byte(code)) {
		t.Fatalf("card code encryption invalid key=%q len=%d", keyID, len(ciphertext))
	}
	revealed, err := repo.RevealCardCode(ctx, cardID)
	if err != nil {
		t.Fatal(err)
	}
	if !revealed.Available || revealed.Code != code || !revealed.Card.PlaintextAvailable {
		t.Fatalf("bad reveal result=%+v", revealed)
	}
	if _, err := keyring.OpenWithAAD(credential.AAD(cardID, credential.CredentialPassword), keyID, ciphertext); err == nil {
		t.Fatal("wrong AAD decrypted card code")
	}
	wrongKey, err := credential.NewKeyring(map[string][]byte{keyID: bytes.Repeat([]byte{8}, 32)}, keyID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wrongKey.OpenWithAAD(credential.CardAAD(cardID), keyID, ciphertext); err == nil {
		t.Fatal("wrong key decrypted card code")
	}
	tampered := append([]byte(nil), ciphertext...)
	tampered[len(tampered)-1] ^= 0x01
	if _, err := db.DB().ExecContext(ctx, "UPDATE cards SET encrypted_code=? WHERE id=?", tampered, cardID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RevealCardCode(ctx, cardID); err == nil {
		t.Fatal("tampered card code decrypted")
	}
	legacyID, err := repo.CreateCard(ctx, CardSeed{CodeHash: cardHashForCode("STUV-WXYZ-2345"), CodeSuffix: "2345", DurationDays: 7})
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := repo.RevealCardCode(ctx, legacyID)
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Available || legacy.Code != "" || legacy.Card.PlaintextAvailable {
		t.Fatalf("legacy card should not reveal plaintext: %+v", legacy)
	}
}

func TestUpdateAccountOptionallyRotatesCredentials(t *testing.T) {
	db := openStore(t)
	defer db.Close()
	repo := New(db.DB(), testCredentialKeyring(t))
	now := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return now })
	accountID, err := repo.CreateAccount(context.Background(), AccountSeed{
		DisplayUsername: "editable@example.test", DisplayPassword: "old-password", DisplayTOTPSecret: "OLD-TOTP", AccountExpiry: now.Add(24 * time.Hour), MaxConcurrentUsers: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.UpdateAccount(context.Background(), accountID, AccountUpdate{
		DisplayUsername: "renamed@example.test", AccountExpiry: now.Add(48 * time.Hour), MaxConcurrentUsers: 3, Status: "available", MonitorStatus: "unknown_monitor",
	}); err != nil {
		t.Fatal(err)
	}
	unchanged, err := repo.Credentials(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.Password != "old-password" || unchanged.TOTPSecret != "OLD-TOTP" {
		t.Fatalf("blank credential update changed secrets: %+v", unchanged)
	}
	if _, err := repo.UpdateAccount(context.Background(), accountID, AccountUpdate{
		DisplayUsername: "renamed@example.test", DisplayPassword: "new-password", DisplayTOTPSecret: "NEW-TOTP", AccountExpiry: now.Add(72 * time.Hour), MaxConcurrentUsers: 3, Status: "available", MonitorStatus: "unknown_monitor",
	}); err != nil {
		t.Fatal(err)
	}
	changed, err := repo.Credentials(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Password != "new-password" || changed.TOTPSecret != "NEW-TOTP" {
		t.Fatalf("credential update did not rotate secrets: %+v", changed)
	}
}

func TestDefaultAccountCapacityCreateAndApplyBelowCurrentAllocations(t *testing.T) {
	db := openStore(t)
	defer db.Close()
	repo := New(db.DB(), testCredentialKeyring(t))
	now := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return now })
	initial, err := repo.DefaultAccountCapacity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if initial != DefaultAccountCapacity {
		t.Fatalf("default capacity=%d want %d", initial, DefaultAccountCapacity)
	}
	if _, err := repo.SetDefaultAccountCapacity(context.Background(), 4); err != nil {
		t.Fatal(err)
	}
	accountID, err := repo.CreateAccount(context.Background(), AccountSeed{
		DisplayUsername: "default-capacity@example.test", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp", AccountExpiry: now.Add(30 * 24 * time.Hour), MonitorStatus: "alive", Status: "available",
	})
	if err != nil {
		t.Fatal(err)
	}
	account, err := repo.Account(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	if account.MaxConcurrentUsers != 4 {
		t.Fatalf("created account capacity=%d want 4", account.MaxConcurrentUsers)
	}
	for i := 0; i < 2; i++ {
		if _, err := repo.CreateCard(context.Background(), CardSeed{CodeHash: hashFor(900 + i), CodeSuffix: suffixFor(900 + i), DurationDays: 30}); err != nil {
			t.Fatal(err)
		}
		if _, err := repo.RedeemCode(context.Background(), hashFor(900+i), true); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := repo.SetDefaultAccountCapacity(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	applied, err := repo.ApplyDefaultAccountCapacity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if applied.DefaultAccountCapacity != 1 || applied.UpdatedAccounts != 1 {
		t.Fatalf("apply result=%+v", applied)
	}
	account, err = repo.Account(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	if account.MaxConcurrentUsers != 1 || account.CurrentAllocations != 2 || account.Status != "full" {
		t.Fatalf("downshift did not preserve allocations and mark full: %+v", account)
	}
	if _, err := repo.CreateCard(context.Background(), CardSeed{CodeHash: hashFor(910), CodeSuffix: suffixFor(910), DurationDays: 30}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RedeemCode(context.Background(), hashFor(910), true); !errors.Is(err, ErrNoAccountCapacity) {
		t.Fatalf("downshifted full account accepted new allocation err=%v", err)
	}
	active, err := repo.ActiveAllocationCount(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	if active != 2 {
		t.Fatalf("existing allocations changed after downshift: %d", active)
	}
}

func TestUpsertSyncedAccountPendingCredentialsAndDoesNotOverwriteCredentials(t *testing.T) {
	db := openStore(t)
	defer db.Close()
	repo := New(db.DB(), testCredentialKeyring(t))
	now := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return now })
	if _, err := repo.SetDefaultAccountCapacity(context.Background(), 5); err != nil {
		t.Fatal(err)
	}
	first, created, err := repo.UpsertSyncedAccount(context.Background(), SyncedAccount{
		MonitorAccountID: "phase-one-sync-1",
		DisplayUsername:  "pulled@example.test",
		AccountExpiry:    now.Add(90 * 24 * time.Hour),
		MonitorStatus:    "alive",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !created || first.Status != "pending_credentials" || first.MaxConcurrentUsers != 5 || first.DisplayUsername != "pulled@example.test" {
		t.Fatalf("bad first pull account=%+v created=%v", first, created)
	}
	updated, err := repo.UpdateAccount(context.Background(), first.ID, AccountUpdate{
		DisplayUsername: first.DisplayUsername, DisplayPassword: "filled-password", DisplayTOTPSecret: "JBSWY3DPEHPK3PXP",
		AccountExpiry: first.AccountExpiry, MaxConcurrentUsers: first.MaxConcurrentUsers, Status: "available", MonitorStatus: "alive", MonitorAccountID: first.MonitorAccountID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "available" {
		t.Fatalf("filled account status=%q", updated.Status)
	}
	cardID, err := repo.CreateCard(context.Background(), CardSeed{CodeSuffix: "SYNC", DurationDays: 7})
	if err != nil {
		t.Fatal(err)
	}
	allocation, err := repo.RedeemCard(context.Background(), cardID)
	if err != nil {
		t.Fatal(err)
	}
	if allocation.AccountID != first.ID {
		t.Fatalf("allocation account=%d, want %d", allocation.AccountID, first.ID)
	}
	creds, err := repo.Credentials(context.Background(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if creds.Password != "filled-password" || creds.TOTPSecret != "JBSWY3DPEHPK3PXP" {
		t.Fatalf("bad credentials after fill: %+v", creds)
	}
	second, created, err := repo.UpsertSyncedAccount(context.Background(), SyncedAccount{
		MonitorAccountID: "phase-one-sync-1",
		DisplayUsername:  "renamed@example.test",
		AccountExpiry:    now.Add(-24 * time.Hour),
		MonitorStatus:    "dead_normal",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created || second.ID != first.ID || second.DisplayUsername != "renamed@example.test" || second.Status != "expired" || second.MonitorStatus != "dead_normal" || second.MaxConcurrentUsers != 5 || second.CurrentAllocations != 1 {
		t.Fatalf("bad second pull account=%+v created=%v", second, created)
	}
	active, err := repo.ActiveAllocationCount(context.Background(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if active != 1 {
		t.Fatalf("active allocations after pull=%d, want 1", active)
	}
	after, err := repo.Credentials(context.Background(), first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Password != creds.Password || after.TOTPSecret != creds.TOTPSecret {
		t.Fatalf("pull overwrote credentials before=%+v after=%+v", creds, after)
	}
	banned, created, err := repo.UpsertSyncedAccount(context.Background(), SyncedAccount{
		MonitorAccountID: "phase-one-banned", DisplayUsername: "banned@example.test",
		AccountExpiry: now.Add(-48 * time.Hour), MonitorStatus: "dead_banned",
	})
	if err != nil || !created || banned.Status != "banned" {
		t.Fatalf("past banned pull account=%+v created=%v err=%v", banned, created, err)
	}
	if _, _, err := repo.UpsertSyncedAccount(context.Background(), SyncedAccount{
		MonitorAccountID: "phase-one-conflict", DisplayUsername: "conflict@example.test",
		AccountExpiry: now.Add(-time.Hour), MonitorStatus: "alive",
	}); !errors.Is(err, ErrAccountExpiryTooLong) {
		t.Fatalf("past alive error=%v, want ErrAccountExpiryTooLong", err)
	}
}

func TestCredentialKeyRotationReencryptAndOldKeyRemovalFailClosed(t *testing.T) {
	db := openStore(t)
	defer db.Close()
	ctx := context.Background()
	oldOnly, err := credential.NewKeyring(map[string][]byte{"alloc-old": bytes.Repeat([]byte{1}, 32)}, "alloc-old")
	if err != nil {
		t.Fatal(err)
	}
	repo := New(db.DB(), oldOnly)
	now := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return now })
	firstID, err := repo.CreateAccount(ctx, AccountSeed{
		DisplayUsername: "old-key", DisplayPassword: "old-password", DisplayTOTPSecret: "old-totp", SourceURL: "https://old.example.test/account", AccountExpiry: now.Add(24 * time.Hour), MaxConcurrentUsers: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	firstPasswordKey, firstPasswordCiphertext, _, _, err := repo.EncryptedCredentials(ctx, firstID)
	if err != nil {
		t.Fatal(err)
	}
	if firstPasswordKey != "alloc-old" {
		t.Fatalf("first password key=%q", firstPasswordKey)
	}
	rotating, err := credential.NewKeyring(map[string][]byte{
		"alloc-old": bytes.Repeat([]byte{1}, 32),
		"alloc-new": bytes.Repeat([]byte{2}, 32),
	}, "alloc-new")
	if err != nil {
		t.Fatal(err)
	}
	rotatingRepo := New(db.DB(), rotating)
	rotatingRepo.SetNow(func() time.Time { return now.Add(time.Hour) })
	secondID, err := rotatingRepo.CreateAccount(ctx, AccountSeed{
		DisplayUsername: "new-key", DisplayPassword: "new-password", DisplayTOTPSecret: "new-totp", SourceURL: "https://new.example.test/account", AccountExpiry: now.Add(24 * time.Hour), MaxConcurrentUsers: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	secondPasswordKey, secondPasswordCiphertext, _, _, err := rotatingRepo.EncryptedCredentials(ctx, secondID)
	if err != nil {
		t.Fatal(err)
	}
	if secondPasswordKey != "alloc-new" {
		t.Fatalf("second password key=%q", secondPasswordKey)
	}
	if _, err := rotating.Open(firstID, credential.CredentialPassword, firstPasswordKey, firstPasswordCiphertext); err != nil {
		t.Fatalf("old key ciphertext not readable during rotation: %v", err)
	}
	if _, err := oldOnly.Open(secondID, credential.CredentialPassword, secondPasswordKey, secondPasswordCiphertext); err == nil {
		t.Fatal("old-only keyring opened new-key ciphertext")
	}
	changed, err := rotatingRepo.ReencryptCredentials(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if changed != 2 {
		t.Fatalf("reencrypted accounts=%d", changed)
	}
	newOnly, err := credential.NewKeyring(map[string][]byte{"alloc-new": bytes.Repeat([]byte{2}, 32)}, "alloc-new")
	if err != nil {
		t.Fatal(err)
	}
	newOnlyRepo := New(db.DB(), newOnly)
	for _, accountID := range []int64{firstID, secondID} {
		passwordKey, passwordCiphertext, totpKey, totpCiphertext, err := newOnlyRepo.EncryptedCredentials(ctx, accountID)
		if err != nil {
			t.Fatal(err)
		}
		if passwordKey != "alloc-new" || totpKey != "alloc-new" {
			t.Fatalf("account %d not reencrypted to active key: password=%q totp=%q", accountID, passwordKey, totpKey)
		}
		if _, err := newOnly.Open(accountID, credential.CredentialPassword, passwordKey, passwordCiphertext); err != nil {
			t.Fatalf("new-only keyring cannot read password for account %d: %v", accountID, err)
		}
		if _, err := newOnly.Open(accountID, credential.CredentialTOTP, totpKey, totpCiphertext); err != nil {
			t.Fatalf("new-only keyring cannot read totp for account %d: %v", accountID, err)
		}
		if _, err := oldOnly.Open(accountID, credential.CredentialPassword, passwordKey, passwordCiphertext); err == nil {
			t.Fatalf("old-only keyring opened reencrypted password for account %d", accountID)
		}
	}
	creds, err := newOnlyRepo.Credentials(ctx, firstID)
	if err != nil {
		t.Fatal(err)
	}
	if creds.Password != "old-password" || creds.TOTPSecret != "old-totp" {
		t.Fatalf("credentials changed after reencrypt: %+v", creds)
	}
	rotatedAccount, err := newOnlyRepo.Account(ctx, firstID)
	if err != nil || rotatedAccount.SourceURL != "https://old.example.test/account" {
		t.Fatalf("source URL changed after reencrypt: account=%+v err=%v", rotatedAccount, err)
	}
}

func TestAccountExpiryValidationAndRetireWithoutAllocations(t *testing.T) {
	db := openStore(t)
	defer db.Close()
	repo := New(db.DB(), testCredentialKeyring(t))
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return now })
	if _, err := repo.CreateAccount(ctx, AccountSeed{
		DisplayUsername: "past", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp", AccountExpiry: now.Add(-time.Hour), MaxConcurrentUsers: 1,
	}); !errors.Is(err, ErrAccountExpiryTooLong) {
		t.Fatalf("expiry error=%v want %v", err, ErrAccountExpiryTooLong)
	}
	if _, err := repo.CreateAccount(ctx, AccountSeed{
		DisplayUsername: "phase-one-long", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp", AccountExpiry: now.Add(90 * 24 * time.Hour), MaxConcurrentUsers: 1,
	}); err != nil {
		t.Fatalf("phase-one governed long expiry rejected: %v", err)
	}
	accountID, err := repo.CreateAccount(ctx, AccountSeed{
		DisplayUsername: "retire-empty", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp", SourceURL: "https://example.test/source", AccountExpiry: now.Add(30 * 24 * time.Hour), MaxConcurrentUsers: 3, MonitorAccountID: "monitor-retired", MonitorStatus: "alive",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := repo.RetireAccount(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Archived || result.ReplacedAllocations != 0 || result.ClosedAllocations != 0 {
		t.Fatalf("retire result=%+v", result)
	}
	if _, err := repo.Account(ctx, accountID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("archived account lookup err=%v", err)
	}
	accounts, err := repo.ListAccounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, account := range accounts {
		if account.ID == accountID {
			t.Fatalf("archived account remained in list: %+v", account)
		}
	}
	var archivedAt sql.NullString
	var status, monitorID, passwordKey, totpKey string
	var password, totp []byte
	var sourceKey sql.NullString
	var source []byte
	if err := db.DB().QueryRow(`SELECT archived_at,status,monitor_account_id,display_password_key_id,display_password_secret,display_2fa_key_id,display_2fa_secret,source_url_key_id,source_url_secret
		FROM chatgpt_accounts WHERE id=?`, accountID).Scan(&archivedAt, &status, &monitorID, &passwordKey, &password, &totpKey, &totp, &sourceKey, &source); err != nil {
		t.Fatal(err)
	}
	if !archivedAt.Valid || status != "disabled" || monitorID != "monitor-retired" || passwordKey != "" || totpKey != "" || len(password) != 0 || len(totp) != 0 || sourceKey.Valid || len(source) != 0 {
		t.Fatalf("archived row not sanitized: archived=%v status=%s monitor=%s password_key=%q password=%x totp_key=%q totp=%x source_key=%v source=%x", archivedAt.Valid, status, monitorID, passwordKey, password, totpKey, totp, sourceKey, source)
	}
	if _, err := repo.RetireAccount(ctx, accountID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("repeated retirement err=%v", err)
	}
	if _, _, err := repo.UpsertSyncedAccount(ctx, SyncedAccount{MonitorAccountID: monitorID, DisplayUsername: "restored@example.test", AccountExpiry: now.Add(24 * time.Hour), MonitorStatus: "alive"}); !errors.Is(err, ErrAccountArchived) {
		t.Fatalf("archived monitor account was not skipped: %v", err)
	}
	if _, err := db.DB().Exec(`UPDATE chatgpt_accounts SET status='available' WHERE id=?`, accountID); err != nil {
		t.Fatal(err)
	}
	metrics, err := repo.InventoryMetrics(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if metrics.EligibleAccounts != 1 || metrics.Capacity != 1 {
		t.Fatalf("archived account counted in inventory: %+v", metrics)
	}
	backupID, err := repo.CreateAccount(ctx, AccountSeed{DisplayUsername: "active-backup", DisplayPassword: "password", DisplayTOTPSecret: "totp", AccountExpiry: now.Add(30 * 24 * time.Hour), MaxConcurrentUsers: 1, MonitorStatus: "alive"})
	if err != nil {
		t.Fatal(err)
	}
	cardID, err := repo.CreateCard(ctx, CardSeed{CodeHash: hashFor(100), CodeSuffix: suffixFor(100), DurationDays: 7})
	if err != nil {
		t.Fatal(err)
	}
	allocation, err := repo.RedeemCard(ctx, cardID)
	if err != nil {
		t.Fatal(err)
	}
	if allocation.AccountID != backupID || allocation.AccountID == accountID {
		t.Fatalf("archived account selected for redemption: allocation=%+v archived=%d backup=%d", allocation, accountID, backupID)
	}
}

func TestRetireAccountAtomicallyMigratesMultiplePrimaryAllocations(t *testing.T) {
	db := openStore(t)
	defer db.Close()
	repo := New(db.DB(), testCredentialKeyring(t))
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return now })
	retiredID, err := repo.CreateAccount(ctx, AccountSeed{DisplayUsername: "retired", DisplayPassword: "password", DisplayTOTPSecret: "totp", AccountExpiry: now.Add(30 * 24 * time.Hour), MaxConcurrentUsers: 2, MonitorStatus: "alive"})
	if err != nil {
		t.Fatal(err)
	}
	oldAllocationIDs := make([]int64, 0, 2)
	for i := 0; i < 2; i++ {
		cardID, err := repo.CreateCard(ctx, CardSeed{CodeHash: hashFor(110 + i), CodeSuffix: suffixFor(110 + i), DurationDays: 7})
		if err != nil {
			t.Fatal(err)
		}
		allocation, err := repo.RedeemCard(ctx, cardID)
		if err != nil {
			t.Fatal(err)
		}
		oldAllocationIDs = append(oldAllocationIDs, allocation.ID)
	}
	backupID, err := repo.CreateAccount(ctx, AccountSeed{DisplayUsername: "backup", DisplayPassword: "password", DisplayTOTPSecret: "totp", AccountExpiry: now.Add(30 * 24 * time.Hour), MaxConcurrentUsers: 2, MonitorStatus: "alive"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := repo.RetireAccount(ctx, retiredID)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Archived || result.ReplacedAllocations != 2 || result.ClosedAllocations != 0 {
		t.Fatalf("retire result=%+v", result)
	}
	var activeOld, activeBackup, historyCount int
	if err := db.DB().QueryRow(`SELECT count(*) FROM allocations WHERE account_id=? AND active=1`, retiredID).Scan(&activeOld); err != nil {
		t.Fatal(err)
	}
	if err := db.DB().QueryRow(`SELECT count(*) FROM allocations WHERE account_id=? AND active=1 AND allocation_state='primary'`, backupID).Scan(&activeBackup); err != nil {
		t.Fatal(err)
	}
	if err := db.DB().QueryRow(`SELECT count(*) FROM replacement_history WHERE old_account_id=? AND new_account_id=? AND reason='account_retired' AND operator='admin'`, retiredID, backupID).Scan(&historyCount); err != nil {
		t.Fatal(err)
	}
	if activeOld != 0 || activeBackup != 2 || historyCount != 2 {
		t.Fatalf("migration old_active=%d backup_active=%d history=%d", activeOld, activeBackup, historyCount)
	}
	for _, allocationID := range oldAllocationIDs {
		var state, reason string
		var active int
		var superseded sql.NullInt64
		if err := db.DB().QueryRow(`SELECT allocation_state,active,replacement_reason,superseded_by_allocation_id FROM allocations WHERE id=?`, allocationID).Scan(&state, &active, &reason, &superseded); err != nil {
			t.Fatal(err)
		}
		if state != "replaced" || active != 0 || reason != "account_retired" || !superseded.Valid {
			t.Fatalf("old allocation %d not linked: state=%s active=%d reason=%s superseded=%v", allocationID, state, active, reason, superseded)
		}
	}
	var violations int
	rows, err := db.DB().Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		violations++
	}
	rows.Close()
	if violations != 0 {
		t.Fatalf("foreign key violations=%d", violations)
	}
}

func TestRetireAccountClosesGraceAndRollsBackWhenReplacementCapacityIsInsufficient(t *testing.T) {
	t.Run("closes active grace", func(t *testing.T) {
		db := openStore(t)
		defer db.Close()
		repo := New(db.DB(), testCredentialKeyring(t))
		ctx := context.Background()
		now := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)
		repo.SetNow(func() time.Time { return now })
		retiredID, err := repo.CreateAccount(ctx, AccountSeed{DisplayUsername: "grace", DisplayPassword: "password", DisplayTOTPSecret: "totp", AccountExpiry: now.Add(12 * time.Hour), MaxConcurrentUsers: 1, MonitorStatus: "alive"})
		if err != nil {
			t.Fatal(err)
		}
		cardID, err := repo.CreateCard(ctx, CardSeed{CodeHash: hashFor(120), CodeSuffix: suffixFor(120), DurationDays: 7})
		if err != nil {
			t.Fatal(err)
		}
		oldAllocation, err := repo.RedeemCard(ctx, cardID)
		if err != nil {
			t.Fatal(err)
		}
		backupID, err := repo.CreateAccount(ctx, AccountSeed{DisplayUsername: "grace-backup", DisplayPassword: "password", DisplayTOTPSecret: "totp", AccountExpiry: now.Add(30 * 24 * time.Hour), MaxConcurrentUsers: 1, MonitorStatus: "alive"})
		if err != nil {
			t.Fatal(err)
		}
		run, err := repo.ProcessReplacements(ctx, now)
		if err != nil || len(run.Replaced) != 1 {
			t.Fatalf("replacement run=%+v err=%v", run, err)
		}
		result, err := repo.RetireAccount(ctx, retiredID)
		if err != nil {
			t.Fatal(err)
		}
		if result.ReplacedAllocations != 0 || result.ClosedAllocations != 1 {
			t.Fatalf("retire result=%+v", result)
		}
		var state, reason string
		var active, backupCapacity int
		if err := db.DB().QueryRow(`SELECT allocation_state,active,replacement_reason FROM allocations WHERE id=?`, oldAllocation.ID).Scan(&state, &active, &reason); err != nil {
			t.Fatal(err)
		}
		if err := db.DB().QueryRow(`SELECT current_allocations FROM chatgpt_accounts WHERE id=?`, backupID).Scan(&backupCapacity); err != nil {
			t.Fatal(err)
		}
		if state != "replaced" || active != 0 || reason != "account_retired" || backupCapacity != 1 {
			t.Fatalf("grace close state=%s active=%d reason=%s backup_capacity=%d", state, active, reason, backupCapacity)
		}
	})

	t.Run("rolls back all partial replacements", func(t *testing.T) {
		db := openStore(t)
		defer db.Close()
		repo := New(db.DB(), testCredentialKeyring(t))
		ctx := context.Background()
		now := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)
		repo.SetNow(func() time.Time { return now })
		retiredID, err := repo.CreateAccount(ctx, AccountSeed{DisplayUsername: "rollback", DisplayPassword: "password", DisplayTOTPSecret: "totp", AccountExpiry: now.Add(30 * 24 * time.Hour), MaxConcurrentUsers: 2, MonitorStatus: "alive"})
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 2; i++ {
			cardID, err := repo.CreateCard(ctx, CardSeed{CodeHash: hashFor(130 + i), CodeSuffix: suffixFor(130 + i), DurationDays: 7})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := repo.RedeemCard(ctx, cardID); err != nil {
				t.Fatal(err)
			}
		}
		backupID, err := repo.CreateAccount(ctx, AccountSeed{DisplayUsername: "small-backup", DisplayPassword: "password", DisplayTOTPSecret: "totp", AccountExpiry: now.Add(30 * 24 * time.Hour), MaxConcurrentUsers: 1, MonitorStatus: "alive"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repo.RetireAccount(ctx, retiredID); !errors.Is(err, ErrAccountReplacementUnavailable) {
			t.Fatalf("retire err=%v", err)
		}
		var archived sql.NullString
		var oldActive, backupActive, historyCount int
		var password []byte
		if err := db.DB().QueryRow(`SELECT archived_at,display_password_secret FROM chatgpt_accounts WHERE id=?`, retiredID).Scan(&archived, &password); err != nil {
			t.Fatal(err)
		}
		if err := db.DB().QueryRow(`SELECT count(*) FROM allocations WHERE account_id=? AND active=1 AND allocation_state='primary'`, retiredID).Scan(&oldActive); err != nil {
			t.Fatal(err)
		}
		if err := db.DB().QueryRow(`SELECT current_allocations FROM chatgpt_accounts WHERE id=?`, backupID).Scan(&backupActive); err != nil {
			t.Fatal(err)
		}
		if err := db.DB().QueryRow(`SELECT count(*) FROM replacement_history WHERE reason='account_retired'`).Scan(&historyCount); err != nil {
			t.Fatal(err)
		}
		if archived.Valid || oldActive != 2 || backupActive != 0 || historyCount != 0 || len(password) == 0 {
			t.Fatalf("partial changes survived rollback: archived=%v old_active=%d backup=%d history=%d password_len=%d", archived.Valid, oldActive, backupActive, historyCount, len(password))
		}
	})
}

func TestCardStateTransitionsRevokeReleaseAndExtend(t *testing.T) {
	db := openStore(t)
	defer db.Close()
	repo := New(db.DB(), testCredentialKeyring(t))
	now := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return now })
	accountID, err := repo.CreateAccount(context.Background(), AccountSeed{
		DisplayUsername: "card-flow", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp", AccountExpiry: now.Add(30 * 24 * time.Hour), MaxConcurrentUsers: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	cardID, err := repo.CreateCard(context.Background(), CardSeed{CodeHash: hashFor(200), CodeSuffix: suffixFor(200), DurationDays: 7})
	if err != nil {
		t.Fatal(err)
	}
	card, err := repo.CardByID(context.Background(), cardID)
	if err != nil {
		t.Fatal(err)
	}
	if card.Status != "unused" {
		t.Fatalf("initial status=%s", card.Status)
	}
	allocation, err := repo.RedeemCard(context.Background(), cardID)
	if err != nil {
		t.Fatal(err)
	}
	if allocation.ValidUntil != now.Add(7*24*time.Hour) {
		t.Fatalf("valid_until=%s", allocation.ValidUntil)
	}
	extended, err := repo.ExtendCard(context.Background(), cardID, 45)
	if err != nil {
		t.Fatal(err)
	}
	if extended.ExpiresAt == nil || !extended.ExpiresAt.Equal(now.Add(52*24*time.Hour)) {
		t.Fatalf("extended expires_at=%v", extended.ExpiresAt)
	}
	var allocationUntil string
	if err := db.DB().QueryRow("SELECT valid_until FROM allocations WHERE id=?", allocation.ID).Scan(&allocationUntil); err != nil {
		t.Fatal(err)
	}
	if allocationUntil != formatTime(now.Add(52*24*time.Hour)) {
		t.Fatalf("allocation valid_until=%s", allocationUntil)
	}
	extended, err = repo.ExtendCard(context.Background(), cardID, 38)
	if err != nil || extended.ExpiresAt == nil || !extended.ExpiresAt.Equal(now.Add(90*24*time.Hour)) {
		t.Fatalf("extend to maximum card=%+v err=%v", extended, err)
	}
	if _, err := repo.ExtendCard(context.Background(), cardID, 1); !errors.Is(err, ErrCardDurationLimit) {
		t.Fatalf("extend beyond maximum err=%v want=%v", err, ErrCardDurationLimit)
	}
	var cardUntilAfterLimit string
	if err := db.DB().QueryRow("SELECT expires_at FROM cards WHERE id=?", cardID).Scan(&cardUntilAfterLimit); err != nil {
		t.Fatal(err)
	}
	if err := db.DB().QueryRow("SELECT valid_until FROM allocations WHERE id=?", allocation.ID).Scan(&allocationUntil); err != nil {
		t.Fatal(err)
	}
	if want := formatTime(now.Add(90 * 24 * time.Hour)); cardUntilAfterLimit != want || allocationUntil != want {
		t.Fatalf("over-limit extension partially updated card=%s allocation=%s want=%s", cardUntilAfterLimit, allocationUntil, want)
	}
	revoked, err := repo.RevokeCard(context.Background(), cardID)
	if err != nil {
		t.Fatal(err)
	}
	if revoked.Status != "revoked" || revoked.RevokedAt == nil {
		t.Fatalf("revoked card=%+v", revoked)
	}
	if _, err := repo.ExtendCard(context.Background(), cardID, 1); !errors.Is(err, ErrCardStateConflict) {
		t.Fatalf("revoked extension err=%v want=%v", err, ErrCardStateConflict)
	}
	active, err := repo.ActiveAllocationCount(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	account, err := repo.Account(context.Background(), accountID)
	if err != nil {
		t.Fatal(err)
	}
	if active != 0 || account.CurrentAllocations != 0 {
		t.Fatalf("capacity not released active=%d account=%+v", active, account)
	}
	var allocationState string
	var allocationActive int
	if err := db.DB().QueryRow("SELECT allocation_state,active FROM allocations WHERE id=?", allocation.ID).Scan(&allocationState, &allocationActive); err != nil {
		t.Fatal(err)
	}
	if allocationState != "revoked" || allocationActive != 0 {
		t.Fatalf("allocation state=%s active=%d", allocationState, allocationActive)
	}
	expiringID, err := repo.CreateCard(context.Background(), CardSeed{CodeHash: hashFor(201), CodeSuffix: suffixFor(201), DurationDays: 7})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ExtendCard(context.Background(), expiringID, 1); !errors.Is(err, ErrCardStateConflict) {
		t.Fatalf("unused extension err=%v want=%v", err, ErrCardStateConflict)
	}
	repo.SetNow(func() time.Time { return now })
	expiringAllocation, err := repo.RedeemCard(context.Background(), expiringID)
	if err != nil {
		t.Fatal(err)
	}
	repo.SetNow(func() time.Time { return now.Add(8 * 24 * time.Hour) })
	if _, err := repo.ExtendCard(context.Background(), expiringID, 1); !errors.Is(err, ErrCardStateConflict) {
		t.Fatalf("elapsed extension err=%v want=%v", err, ErrCardStateConflict)
	}
	expiredCount, err := repo.ExpireDueCards(context.Background(), now.Add(8*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if expiredCount != 1 {
		t.Fatalf("expired count=%d want 1", expiredCount)
	}
	expiredCard, err := repo.CardByID(context.Background(), expiringID)
	if err != nil {
		t.Fatal(err)
	}
	if expiredCard.Status != "expired" {
		t.Fatalf("expired status=%s", expiredCard.Status)
	}
	if _, err := repo.ExtendCard(context.Background(), expiringID, 1); !errors.Is(err, ErrCardStateConflict) {
		t.Fatalf("expired extension err=%v want=%v", err, ErrCardStateConflict)
	}
	if err := db.DB().QueryRow("SELECT allocation_state,active FROM allocations WHERE id=?", expiringAllocation.ID).Scan(&allocationState, &allocationActive); err != nil {
		t.Fatal(err)
	}
	if allocationState != "expired" || allocationActive != 0 {
		t.Fatalf("expired allocation state=%s active=%d", allocationState, allocationActive)
	}
	ninetyID, err := repo.CreateCard(context.Background(), CardSeed{CodeHash: hashFor(202), CodeSuffix: suffixFor(202), DurationDays: 90})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RedeemCard(context.Background(), ninetyID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ExtendCard(context.Background(), ninetyID, 1); !errors.Is(err, ErrCardDurationLimit) {
		t.Fatalf("90-day card extension err=%v want=%v", err, ErrCardDurationLimit)
	}
}

func TestRedeemUsesExactDurationDaysThroughNinety(t *testing.T) {
	db := openStore(t)
	defer db.Close()
	repo := New(db.DB(), testCredentialKeyring(t))
	now := time.Date(2026, 8, 1, 12, 34, 56, 0, time.UTC)
	repo.SetNow(func() time.Time { return now })
	if _, err := repo.CreateAccount(context.Background(), AccountSeed{
		DisplayUsername: "duration-range", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp",
		AccountExpiry: now.Add(30 * 24 * time.Hour), MaxConcurrentUsers: 3,
	}); err != nil {
		t.Fatal(err)
	}
	for index, days := range []int{31, 45, 90} {
		cardID, err := repo.CreateCard(context.Background(), CardSeed{
			CodeHash: hashFor(300 + index), CodeSuffix: suffixFor(300 + index), DurationDays: days,
		})
		if err != nil {
			t.Fatal(err)
		}
		allocation, err := repo.RedeemCard(context.Background(), cardID)
		if err != nil {
			t.Fatalf("redeem duration=%d: %v", days, err)
		}
		want := now.Add(time.Duration(days) * 24 * time.Hour)
		card, err := repo.CardByID(context.Background(), cardID)
		if err != nil {
			t.Fatal(err)
		}
		if card.ExpiresAt == nil || !card.ExpiresAt.Equal(want) || !allocation.ValidUntil.Equal(want) {
			t.Fatalf("duration=%d card_expires=%v allocation_until=%s want=%s", days, card.ExpiresAt, allocation.ValidUntil, want)
		}
	}
}

func TestRedeemSelectsWasteProximityOptimalAccountOQ22(t *testing.T) {
	db := openStore(t)
	defer db.Close()
	repo := New(db.DB(), testCredentialKeyring(t))
	now := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return now })
	aliveWasteHigh, err := repo.CreateAccount(context.Background(), AccountSeed{
		DisplayUsername: "alive-waste-high", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp", AccountExpiry: now.Add(20 * 24 * time.Hour), MaxConcurrentUsers: 5, MonitorStatus: "alive",
	})
	if err != nil {
		t.Fatal(err)
	}
	unknownWasteLow, err := repo.CreateAccount(context.Background(), AccountSeed{
		DisplayUsername: "unknown-waste-low", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp", AccountExpiry: now.Add(8 * 24 * time.Hour), MaxConcurrentUsers: 5, MonitorStatus: "unknown",
	})
	if err != nil {
		t.Fatal(err)
	}
	firstRedeem, err := redeemCodeForTest(t, repo, "2345-6789-ABCD", 7, true)
	if err != nil {
		t.Fatal(err)
	}
	allocation, err := repo.RedeemCode(context.Background(), cardHashForCode("2345-6789-ABCD"), true)
	if err != nil {
		t.Fatalf("duplicate redeem should be idempotent: %v", err)
	}
	if allocation.Allocation.ID != firstRedeem.Allocation.ID || allocation.Account.ID != firstRedeem.Account.ID {
		t.Fatalf("duplicate redeem returned different allocation first=%+v duplicate=%+v", firstRedeem.Allocation, allocation.Allocation)
	}
	count, err := repo.ActiveAllocationCount(context.Background(), firstRedeem.Account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("duplicate redeem changed active allocation count=%d", count)
	}
	first, err := repo.CardByHash(context.Background(), cardHashForCode("2345-6789-ABCD"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != "redeemed" {
		t.Fatalf("first status=%s", first.Status)
	}
	var accountID int64
	if err := db.DB().QueryRow("SELECT account_id FROM allocations WHERE card_id=?", first.ID).Scan(&accountID); err != nil {
		t.Fatal(err)
	}
	if accountID != aliveWasteHigh || accountID == unknownWasteLow {
		t.Fatalf("monitor_rank must win before waste: got account=%d alive=%d unknown=%d", accountID, aliveWasteHigh, unknownWasteLow)
	}

	bestWaste, err := repo.CreateAccount(context.Background(), AccountSeed{
		DisplayUsername: "best-waste-high-load", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp", AccountExpiry: now.Add(8 * 24 * time.Hour), MaxConcurrentUsers: 10, MonitorStatus: "alive",
	})
	if err != nil {
		t.Fatal(err)
	}
	lowLoadWorseWaste, err := repo.CreateAccount(context.Background(), AccountSeed{
		DisplayUsername: "low-load-worse-waste", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp", AccountExpiry: now.Add(25 * 24 * time.Hour), MaxConcurrentUsers: 10, MonitorStatus: "alive",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec("UPDATE chatgpt_accounts SET current_allocations=5 WHERE id=?", bestWaste); err != nil {
		t.Fatal(err)
	}
	result, err := redeemCodeForTest(t, repo, "EFGH-JKMN-PQRS", 7, true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Account.ID != bestWaste || result.Account.ID == lowLoadWorseWaste {
		t.Fatalf("waste_days must beat current_allocations: got=%d bestWaste=%d lowLoad=%d", result.Account.ID, bestWaste, lowLoadWorseWaste)
	}

	perfect, err := repo.CreateAccount(context.Background(), AccountSeed{
		DisplayUsername: "monthly-perfect", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp", AccountExpiry: now.Add(30 * 24 * time.Hour), MaxConcurrentUsers: 1, MonitorStatus: "alive",
	})
	if err != nil {
		t.Fatal(err)
	}
	near, err := repo.CreateAccount(context.Background(), AccountSeed{
		DisplayUsername: "monthly-near", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp", AccountExpiry: now.Add(29 * 24 * time.Hour), MaxConcurrentUsers: 1, MonitorStatus: "alive",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err = redeemCodeForTest(t, repo, "TUVW-XYZ2-3456", 30, true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Account.ID != perfect || result.Account.ID == near {
		t.Fatalf("perfect 30-day match should win proximity: got=%d perfect=%d near=%d", result.Account.ID, perfect, near)
	}

	quarterClosest, err := repo.CreateAccount(context.Background(), AccountSeed{
		DisplayUsername: "quarter-closest", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp", AccountExpiry: now.Add(30 * 24 * time.Hour), MaxConcurrentUsers: 1, MonitorStatus: "alive",
	})
	if err != nil {
		t.Fatal(err)
	}
	quarterFarther, err := repo.CreateAccount(context.Background(), AccountSeed{
		DisplayUsername: "quarter-farther", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp", AccountExpiry: now.Add(10 * 24 * time.Hour), MaxConcurrentUsers: 1, MonitorStatus: "alive",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err = redeemCodeForTest(t, repo, "789A-BCDE-FGHJ", 90, true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Account.ID != quarterClosest || result.Account.ID == quarterFarther {
		t.Fatalf("quarter card should choose closest under-filled account: got=%d closest=%d farther=%d", result.Account.ID, quarterClosest, quarterFarther)
	}
}

func TestRedeemAlwaysExcludesDeadBanned(t *testing.T) {
	db := openStore(t)
	defer db.Close()
	repo := New(db.DB(), testCredentialKeyring(t))
	now := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return now })
	deadBanned, err := repo.CreateAccount(context.Background(), AccountSeed{
		DisplayUsername: "dead-banned", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp", AccountExpiry: now.Add(7 * 24 * time.Hour), MaxConcurrentUsers: 1, MonitorStatus: "dead_banned",
	})
	if err != nil {
		t.Fatal(err)
	}
	alive, err := repo.CreateAccount(context.Background(), AccountSeed{
		DisplayUsername: "alive", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp", AccountExpiry: now.Add(20 * 24 * time.Hour), MaxConcurrentUsers: 2, MonitorStatus: "alive",
	})
	if err != nil {
		t.Fatal(err)
	}
	online, err := redeemCodeForTest(t, repo, "KLMN-PQRS-TUVW", 7, true)
	if err != nil {
		t.Fatal(err)
	}
	if online.Account.ID != alive || online.Account.ID == deadBanned {
		t.Fatalf("online must exclude dead_banned: got=%d alive=%d dead=%d", online.Account.ID, alive, deadBanned)
	}
	offline, err := redeemCodeForTest(t, repo, "WXYZ-2345-6789", 7, false)
	if err != nil {
		t.Fatal(err)
	}
	if offline.Account.ID != alive || offline.Account.ID == deadBanned {
		t.Fatalf("offline must also exclude dead_banned: got=%d alive=%d dead=%d", offline.Account.ID, alive, deadBanned)
	}
}

func TestRedeemExcludesPendingCredentials(t *testing.T) {
	db := openStore(t)
	defer db.Close()
	repo := New(db.DB(), testCredentialKeyring(t))
	now := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return now })
	pending, created, err := repo.UpsertSyncedAccount(context.Background(), SyncedAccount{
		MonitorAccountID: "pending-best", DisplayUsername: "pending-best@example.test", AccountExpiry: now.Add(7 * 24 * time.Hour), MonitorStatus: "alive", MaxConcurrentUsers: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !created || pending.Status != "pending_credentials" {
		t.Fatalf("bad pending fixture account=%+v created=%v", pending, created)
	}
	available, err := repo.CreateAccount(context.Background(), AccountSeed{
		DisplayUsername: "available-worse", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp", AccountExpiry: now.Add(20 * 24 * time.Hour), MaxConcurrentUsers: 1, MonitorStatus: "alive", Status: "available",
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := redeemCodeForTest(t, repo, "CDEF-HJKM-NPQR", 7, true)
	if err != nil {
		t.Fatal(err)
	}
	if result.Account.ID == pending.ID || result.Account.ID != available {
		t.Fatalf("pending account selected: got=%d pending=%d available=%d", result.Account.ID, pending.ID, available)
	}
}

func TestInventoryMetricsWarningFixturesAndZeroRate(t *testing.T) {
	tests := []struct {
		name       string
		capacity   int
		used       int
		redeemed7  int
		wantLevel  string
		wantWindow string
	}{
		{name: "safe", capacity: 20, used: 4, redeemed7: 7, wantLevel: "safe", wantWindow: "16.0"},
		{name: "notice", capacity: 12, used: 2, redeemed7: 7, wantLevel: "notice", wantWindow: "10.0"},
		{name: "urgent", capacity: 8, used: 2, redeemed7: 7, wantLevel: "urgent", wantWindow: "6.0"},
		{name: "exhausted", capacity: 3, used: 3, redeemed7: 7, wantLevel: "exhausted", wantWindow: "0"},
		{name: "zero-rate", capacity: 5, used: 1, redeemed7: 0, wantLevel: "safe", wantWindow: "∞"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openStore(t)
			defer db.Close()
			repo := New(db.DB(), testCredentialKeyring(t))
			now := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)
			repo.SetNow(func() time.Time { return now })
			if _, err := repo.CreateAccount(context.Background(), AccountSeed{
				DisplayUsername: "metrics-account", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp", AccountExpiry: now.Add(30 * 24 * time.Hour), MaxConcurrentUsers: tt.capacity, MonitorStatus: "alive", Status: "available",
			}); err != nil {
				t.Fatal(err)
			}
			status := "available"
			if tt.used >= tt.capacity {
				status = "full"
			}
			if _, err := db.DB().Exec("UPDATE chatgpt_accounts SET current_allocations=?, status=? WHERE display_username='metrics-account'", tt.used, status); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < tt.redeemed7; i++ {
				if _, err := db.DB().Exec(`INSERT INTO cards(code_hash,code_suffix,duration_days,status,redeemed_at,expires_at,created_at,updated_at)
					VALUES (?,?,30,'redeemed',?,?,?,?)`, hashFor(700+i), suffixFor(700+i), formatTime(now.Add(-time.Duration(i)*24*time.Hour)), formatTime(now.Add(30*24*time.Hour)), formatTime(now), formatTime(now)); err != nil {
					t.Fatal(err)
				}
			}
			metrics, err := repo.InventoryMetrics(context.Background(), now)
			if err != nil {
				t.Fatal(err)
			}
			if metrics.WarningLevel != tt.wantLevel || metrics.ExhaustionWindow != tt.wantWindow {
				t.Fatalf("warning=%s window=%s want %s %s metrics=%+v", metrics.WarningLevel, metrics.ExhaustionWindow, tt.wantLevel, tt.wantWindow, metrics)
			}
			if metrics.Capacity != tt.capacity || metrics.Used != tt.used || metrics.AvailableCapacity != maxInt(0, tt.capacity-tt.used) || metrics.RedeemedLast7Days != tt.redeemed7 {
				t.Fatalf("metrics mismatch: %+v fixture=%+v", metrics, tt)
			}
			if tt.redeemed7 == 0 && metrics.DaysToExhaust != nil {
				t.Fatalf("zero-rate days_to_exhaust=%v want nil/infinity label", *metrics.DaysToExhaust)
			}
		})
	}
}

func TestInventoryMetricsDashboardFixtureValues(t *testing.T) {
	db := openStore(t)
	defer db.Close()
	repo := New(db.DB(), testCredentialKeyring(t))
	now := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return now })
	if _, err := repo.CreateAccount(context.Background(), AccountSeed{
		DisplayUsername: "eligible-a", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp", AccountExpiry: now.Add(30 * 24 * time.Hour), MaxConcurrentUsers: 10, MonitorStatus: "alive", Status: "available",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateAccount(context.Background(), AccountSeed{
		DisplayUsername: "eligible-b", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp", AccountExpiry: now.Add(20 * 24 * time.Hour), MaxConcurrentUsers: 5, MonitorStatus: "dead_normal", Status: "available",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateAccount(context.Background(), AccountSeed{
		DisplayUsername: "excluded-banned", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp", AccountExpiry: now.Add(20 * 24 * time.Hour), MaxConcurrentUsers: 99, MonitorStatus: "dead_banned", Status: "available",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec("UPDATE chatgpt_accounts SET current_allocations=6 WHERE display_username='eligible-a'"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec("UPDATE chatgpt_accounts SET current_allocations=1 WHERE display_username='eligible-b'"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 14; i++ {
		if _, err := db.DB().Exec(`INSERT INTO cards(code_hash,code_suffix,duration_days,status,redeemed_at,expires_at,created_at,updated_at)
			VALUES (?,?,30,'redeemed',?,?,?,?)`, hashFor(800+i), suffixFor(800+i), formatTime(now.Add(-time.Duration(i%7)*24*time.Hour)), formatTime(now.Add(30*24*time.Hour)), formatTime(now), formatTime(now)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.DB().Exec(`INSERT INTO cards(code_hash,code_suffix,duration_days,status,created_at,updated_at) VALUES (?,?,7,'unused',?,?)`, hashFor(900), suffixFor(900), formatTime(now), formatTime(now)); err != nil {
		t.Fatal(err)
	}
	metrics, err := repo.InventoryMetrics(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if metrics.Capacity != 15 || metrics.Used != 7 || metrics.AvailableCapacity != 8 || metrics.EligibleAccounts != 2 || metrics.UnusedCards != 1 || metrics.RedeemedLast7Days != 14 {
		t.Fatalf("dashboard metrics mismatch: %+v", metrics)
	}
	if metrics.WarningLevel != "urgent" || metrics.ExhaustionWindow != "4.0" || metrics.RecommendedAccountAdd != 4 {
		t.Fatalf("dashboard warning/recommendation mismatch: %+v", metrics)
	}
}

func TestReplacementNinetyDayCardMultipleWasteMinimizedReplacements(t *testing.T) {
	db := openStore(t)
	defer db.Close()
	repo := New(db.DB(), testCredentialKeyring(t))
	ctx := context.Background()
	start := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return start })
	firstAccount, err := repo.CreateAccount(ctx, AccountSeed{
		DisplayUsername: "month-1", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp", AccountExpiry: start.Add(30 * 24 * time.Hour), MaxConcurrentUsers: 1, MonitorStatus: "alive",
	})
	if err != nil {
		t.Fatal(err)
	}
	cardID, err := repo.CreateCard(ctx, CardSeed{CodeHash: cardHashForCode("2345-6789-ABCD"), CodeSuffix: "ABCD", DurationDays: 90})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RedeemCard(ctx, cardID); err != nil {
		t.Fatal(err)
	}
	var selected []int64
	for _, step := range []struct {
		now        time.Time
		bestName   string
		bestExpiry time.Time
		worseName  string
		worseExp   time.Time
	}{
		{now: start.Add(29 * 24 * time.Hour), bestName: "month-2-best", bestExpiry: start.Add(59 * 24 * time.Hour), worseName: "month-2-worse", worseExp: start.Add(50 * 24 * time.Hour)},
		{now: start.Add(58 * 24 * time.Hour), bestName: "month-3-best", bestExpiry: start.Add(88 * 24 * time.Hour), worseName: "month-3-worse", worseExp: start.Add(70 * 24 * time.Hour)},
		{now: start.Add(87 * 24 * time.Hour), bestName: "final-perfect", bestExpiry: start.Add(90 * 24 * time.Hour), worseName: "final-worse", worseExp: start.Add(89 * 24 * time.Hour)},
	} {
		repo.SetNow(func() time.Time { return step.now })
		best, err := repo.CreateAccount(ctx, AccountSeed{
			DisplayUsername: step.bestName, DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp", AccountExpiry: step.bestExpiry, MaxConcurrentUsers: 1, MonitorStatus: "alive",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repo.CreateAccount(ctx, AccountSeed{
			DisplayUsername: step.worseName, DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp", AccountExpiry: step.worseExp, MaxConcurrentUsers: 1, MonitorStatus: "alive",
		}); err != nil {
			t.Fatal(err)
		}
		run, err := repo.ProcessReplacements(ctx, step.now)
		if err != nil {
			t.Fatal(err)
		}
		if len(run.Replaced) != 1 || run.Replaced[0].NewAccountID != best {
			t.Fatalf("replacement at %s selected %+v want account %d", step.now, run.Replaced, best)
		}
		selected = append(selected, best)
		if run.Replaced[0].Reason != "account_expiring" || run.Replaced[0].GraceUntil == nil {
			t.Fatalf("expiry replacement should create grace: %+v", run.Replaced[0])
		}
		if _, err := repo.ProcessReplacements(ctx, step.now.Add(25*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if selected[0] == firstAccount || selected[1] == selected[0] || selected[2] == selected[1] {
		t.Fatalf("replacement chain reused an active account unexpectedly: first=%d selected=%v", firstAccount, selected)
	}
	var historyCount int
	if err := db.DB().QueryRow("SELECT count(*) FROM replacement_history WHERE card_id=? AND reason='account_expiring'", cardID).Scan(&historyCount); err != nil {
		t.Fatal(err)
	}
	if historyCount != 3 {
		t.Fatalf("history count=%d want 3", historyCount)
	}
}

func TestBannedReplacementImmediateNoGraceAndRetryFailureAudited(t *testing.T) {
	db := openStore(t)
	defer db.Close()
	repo := New(db.DB(), testCredentialKeyring(t))
	ctx := context.Background()
	now := time.Date(2026, 7, 24, 0, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return now })
	oldAccount, err := repo.CreateAccount(ctx, AccountSeed{
		DisplayUsername: "banned-old", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp", AccountExpiry: now.Add(30 * 24 * time.Hour), MaxConcurrentUsers: 1, MonitorStatus: "alive",
	})
	if err != nil {
		t.Fatal(err)
	}
	cardID, err := repo.CreateCard(ctx, CardSeed{CodeHash: cardHashForCode("EFGH-JKMN-PQRS"), CodeSuffix: "PQRS", DurationDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	allocation, err := repo.RedeemCard(ctx, cardID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec("UPDATE chatgpt_accounts SET monitor_status='dead_banned' WHERE id=?", oldAccount); err != nil {
		t.Fatal(err)
	}
	run, err := repo.ProcessReplacements(ctx, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if run.Failed != 1 || len(run.Replaced) != 0 {
		t.Fatalf("replacement without spare capacity should fail and retry later: %+v", run)
	}
	var failedAudit int
	if err := db.DB().QueryRow("SELECT count(*) FROM audit_events WHERE action='replacement.failed' AND target_id=?", cardID).Scan(&failedAudit); err != nil {
		t.Fatal(err)
	}
	if failedAudit != 1 {
		t.Fatalf("failed audit count=%d want 1", failedAudit)
	}
	newAccount, err := repo.CreateAccount(ctx, AccountSeed{
		DisplayUsername: "banned-new", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp", AccountExpiry: now.Add(30 * 24 * time.Hour), MaxConcurrentUsers: 1, MonitorStatus: "alive",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err = repo.ProcessReplacements(ctx, now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Replaced) != 1 || run.Replaced[0].Reason != "banned" || run.Replaced[0].GraceUntil != nil || run.Replaced[0].NewAccountID != newAccount {
		t.Fatalf("banned replacement result=%+v new=%d", run.Replaced, newAccount)
	}
	var state string
	var active int
	var graceUntil sql.NullString
	if err := db.DB().QueryRow("SELECT allocation_state,active,grace_until FROM allocations WHERE id=?", allocation.ID).Scan(&state, &active, &graceUntil); err != nil {
		t.Fatal(err)
	}
	if state != "replaced" || active != 0 || graceUntil.Valid {
		t.Fatalf("old allocation state=%s active=%d grace=%+v", state, active, graceUntil)
	}
	if len(run.Retired) != 1 || run.Retired[0].AccountID != oldAccount || run.Retired[0].Reason != RetireReasonBanned {
		t.Fatalf("banned account should be taken offline automatically: %+v", run.Retired)
	}
	var archivedAt sql.NullString
	var oldStatus string
	var oldAllocations int
	if err := db.DB().QueryRow("SELECT archived_at,status,current_allocations FROM chatgpt_accounts WHERE id=?", oldAccount).
		Scan(&archivedAt, &oldStatus, &oldAllocations); err != nil {
		t.Fatal(err)
	}
	if !archivedAt.Valid || oldStatus != "disabled" || oldAllocations != 0 {
		t.Fatalf("banned account offline state archived=%v status=%s allocations=%d", archivedAt.Valid, oldStatus, oldAllocations)
	}
	if _, err := repo.Account(ctx, oldAccount); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("retired account should leave the active account list: %v", err)
	}
}

func openStore(t *testing.T) *store.Store {
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

func testCredentialKeyring(t *testing.T) *credential.Keyring {
	t.Helper()
	keyring, err := credential.NewKeyring(map[string][]byte{"alloc-test-k1": bytes.Repeat([]byte{7}, 32)}, "alloc-test-k1")
	if err != nil {
		t.Fatal(err)
	}
	return keyring
}

func redeemCodeForTest(t *testing.T, repo *Repository, code string, durationDays int, monitorAvailable bool) (RedeemResult, error) {
	t.Helper()
	if _, err := repo.CreateCard(context.Background(), CardSeed{CodeHash: cardHashForCode(code), CodeSuffix: code[len(code)-4:], DurationDays: durationDays}); err != nil {
		return RedeemResult{}, err
	}
	return repo.RedeemCode(context.Background(), cardHashForCode(code), monitorAvailable)
}

func cardHashForCode(code string) []byte {
	sum := sha256.Sum256([]byte("allocation-card-v1:" + strings.ToUpper(strings.TrimSpace(code))))
	return sum[:]
}

func hashFor(i int) []byte {
	sum := sha256.Sum256([]byte(suffixFor(i)))
	return sum[:]
}

func suffixFor(i int) string {
	return fmt.Sprintf("%04d", i)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// 复现 8/5–8/6 的线上故障：卡在宽限期内又碰上新账号临期，
// 链式换号必须成功，而不是撞 allocations_active_grace_card_uq 让整轮扫描持续失败。
func TestChainedReplacementSupersedesGraceInsteadOfBlockingRun(t *testing.T) {
	db := openStore(t)
	defer db.Close()
	repo := New(db.DB(), testCredentialKeyring(t))
	ctx := context.Background()
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return start })

	first, err := repo.CreateAccount(ctx, AccountSeed{
		DisplayUsername: "chain-1", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp",
		AccountExpiry: start.Add(48 * time.Hour), MaxConcurrentUsers: 1, MonitorStatus: "alive",
	})
	if err != nil {
		t.Fatal(err)
	}
	cardID, err := repo.CreateCard(ctx, CardSeed{CodeHash: cardHashForCode("CHAI-NGRA-CE01"), CodeSuffix: "CE01", DurationDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RedeemCard(ctx, cardID); err != nil {
		t.Fatal(err)
	}
	// 第二个账号只比第一个晚 1 小时到期：换过去之后马上又该换号，宽限期还没结束。
	second, err := repo.CreateAccount(ctx, AccountSeed{
		DisplayUsername: "chain-2", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp",
		AccountExpiry: start.Add(49 * time.Hour), MaxConcurrentUsers: 1, MonitorStatus: "alive",
	})
	if err != nil {
		t.Fatal(err)
	}
	firstRun, err := repo.ProcessReplacements(ctx, start.Add(25*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(firstRun.Replaced) != 1 || firstRun.Replaced[0].NewAccountID != second || firstRun.Replaced[0].GraceUntil == nil {
		t.Fatalf("first replacement=%+v want grace move to %d", firstRun.Replaced, second)
	}

	third, err := repo.CreateAccount(ctx, AccountSeed{
		DisplayUsername: "chain-3", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp",
		AccountExpiry: start.Add(30 * 24 * time.Hour), MaxConcurrentUsers: 1, MonitorStatus: "alive",
	})
	if err != nil {
		t.Fatal(err)
	}
	// 仍在第一段宽限期内（grace_until = start+49h）就触发第二次换号。
	secondRun, err := repo.ProcessReplacements(ctx, start.Add(26*time.Hour))
	if err != nil {
		t.Fatalf("chained replacement must not fail the whole run: %v", err)
	}
	if secondRun.Failed != 0 || len(secondRun.Replaced) != 1 || secondRun.Replaced[0].NewAccountID != third {
		t.Fatalf("second replacement=%+v failed=%d want move to %d", secondRun.Replaced, secondRun.Failed, third)
	}

	var activeGrace int
	if err := db.DB().QueryRow("SELECT count(*) FROM allocations WHERE card_id=? AND active=1 AND allocation_state='grace'", cardID).Scan(&activeGrace); err != nil {
		t.Fatal(err)
	}
	if activeGrace != 1 {
		t.Fatalf("card should carry exactly one grace allocation, got %d", activeGrace)
	}
	var superseded string
	if err := db.DB().QueryRow("SELECT replacement_reason FROM allocations WHERE card_id=? AND account_id=? AND active=0", cardID, first).Scan(&superseded); err != nil {
		t.Fatal(err)
	}
	if superseded != "grace_superseded" {
		t.Fatalf("first grace allocation reason=%s want grace_superseded", superseded)
	}
	// 老账号的容量必须随宽限期结算一并释放。
	var firstAllocations int
	if err := db.DB().QueryRow("SELECT current_allocations FROM chatgpt_accounts WHERE id=?", first).Scan(&firstAllocations); err != nil {
		t.Fatal(err)
	}
	if firstAllocations != 0 {
		t.Fatalf("superseded grace did not release capacity: %d", firstAllocations)
	}
}

// 一张卡换号失败不得影响同一轮里其它卡，也不得留下"老分配已下线、新分配没建成"的半截状态。
func TestReplacementRunIsolatesSingleCardFailure(t *testing.T) {
	db := openStore(t)
	defer db.Close()
	repo := New(db.DB(), testCredentialKeyring(t))
	ctx := context.Background()
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return start })

	expiring, err := repo.CreateAccount(ctx, AccountSeed{
		DisplayUsername: "expiring", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp",
		AccountExpiry: start.Add(30 * time.Hour), MaxConcurrentUsers: 2, MonitorStatus: "alive",
	})
	if err != nil {
		t.Fatal(err)
	}
	var cardIDs []int64
	for _, code := range []string{"ISOL-ATE0-0001", "ISOL-ATE0-0002"} {
		cardID, err := repo.CreateCard(ctx, CardSeed{CodeHash: cardHashForCode(code), CodeSuffix: code[len(code)-4:], DurationDays: 30})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repo.RedeemCard(ctx, cardID); err != nil {
			t.Fatal(err)
		}
		cardIDs = append(cardIDs, cardID)
	}
	// 只留一个空位：两张卡里只有一张能换成功，另一张必须被单独记为失败。
	spare, err := repo.CreateAccount(ctx, AccountSeed{
		DisplayUsername: "spare", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp",
		AccountExpiry: start.Add(30 * 24 * time.Hour), MaxConcurrentUsers: 1, MonitorStatus: "alive",
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := repo.ProcessReplacements(ctx, start.Add(7*time.Hour))
	if err != nil {
		t.Fatalf("a single card failure must not abort the run: %v", err)
	}
	if len(run.Replaced) != 1 || run.Failed != 1 || run.Replaced[0].NewAccountID != spare {
		t.Fatalf("run=%+v want 1 replaced onto %d and 1 failed", run, spare)
	}
	// 失败那张卡必须原封不动地留在老账号上，顾客不能掉线。
	failedCard := cardIDs[0]
	if run.Replaced[0].CardID == failedCard {
		failedCard = cardIDs[1]
	}
	var state string
	var accountID int64
	if err := db.DB().QueryRow("SELECT allocation_state,account_id FROM allocations WHERE card_id=? AND active=1", failedCard).Scan(&state, &accountID); err != nil {
		t.Fatalf("failed card lost its allocation: %v", err)
	}
	if state != "primary" || accountID != expiring {
		t.Fatalf("failed card allocation state=%s account=%d want primary on %d", state, accountID, expiring)
	}
	var failedAudit int
	if err := db.DB().QueryRow("SELECT count(*) FROM audit_events WHERE action='replacement.failed' AND target_id=?", failedCard).Scan(&failedAudit); err != nil {
		t.Fatal(err)
	}
	if failedAudit != 1 {
		t.Fatalf("failed audit count=%d want 1", failedAudit)
	}
}

// 还有顾客没迁走的封禁账号必须留在线上，等下一轮补齐后才自动下线。
func TestBannedAccountStaysOnlineUntilEveryCustomerMigrated(t *testing.T) {
	db := openStore(t)
	defer db.Close()
	repo := New(db.DB(), testCredentialKeyring(t))
	ctx := context.Background()
	now := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return now })

	banned, err := repo.CreateAccount(ctx, AccountSeed{
		DisplayUsername: "to-ban", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp",
		AccountExpiry: now.Add(30 * 24 * time.Hour), MaxConcurrentUsers: 2, MonitorStatus: "alive",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{"BANN-EDA0-0001", "BANN-EDA0-0002"} {
		cardID, err := repo.CreateCard(ctx, CardSeed{CodeHash: cardHashForCode(code), CodeSuffix: code[len(code)-4:], DurationDays: 30})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := repo.RedeemCard(ctx, cardID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.DB().Exec("UPDATE chatgpt_accounts SET monitor_status='dead_banned',status='banned' WHERE id=?", banned); err != nil {
		t.Fatal(err)
	}
	first, err := repo.CreateAccount(ctx, AccountSeed{
		DisplayUsername: "rescue-1", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp",
		AccountExpiry: now.Add(30 * 24 * time.Hour), MaxConcurrentUsers: 1, MonitorStatus: "alive",
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = first
	run, err := repo.ProcessReplacements(ctx, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(run.Replaced) != 1 || run.Failed != 1 || len(run.Retired) != 0 {
		t.Fatalf("run=%+v want 1 replaced, 1 failed, nothing retired yet", run)
	}
	var archivedAt sql.NullString
	if err := db.DB().QueryRow("SELECT archived_at FROM chatgpt_accounts WHERE id=?", banned).Scan(&archivedAt); err != nil {
		t.Fatal(err)
	}
	if archivedAt.Valid {
		t.Fatal("banned account was taken offline while a customer was still on it")
	}

	if _, err := repo.CreateAccount(ctx, AccountSeed{
		DisplayUsername: "rescue-2", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp",
		AccountExpiry: now.Add(30 * 24 * time.Hour), MaxConcurrentUsers: 1, MonitorStatus: "alive",
	}); err != nil {
		t.Fatal(err)
	}
	second, err := repo.ProcessReplacements(ctx, now.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Replaced) != 1 || second.Failed != 0 || len(second.Retired) != 1 ||
		second.Retired[0].AccountID != banned || second.Retired[0].Reason != RetireReasonBanned {
		t.Fatalf("run=%+v want the banned account retired once everyone moved", second)
	}
	var status string
	var allocations int
	if err := db.DB().QueryRow("SELECT status,current_allocations,archived_at FROM chatgpt_accounts WHERE id=?", banned).
		Scan(&status, &allocations, &archivedAt); err != nil {
		t.Fatal(err)
	}
	if status != "disabled" || allocations != 0 || !archivedAt.Valid {
		t.Fatalf("offline state status=%s allocations=%d archived=%v", status, allocations, archivedAt.Valid)
	}
}

// 订阅正常到期的账号：顾客迁走后自动下线，迁移未完成前必须留在线上。
func TestExpiredAccountRetiredOnlyAfterCustomersMoveOff(t *testing.T) {
	db := openStore(t)
	defer db.Close()
	repo := New(db.DB(), testCredentialKeyring(t))
	ctx := context.Background()
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return start })

	expiring, err := repo.CreateAccount(ctx, AccountSeed{
		DisplayUsername: "expiring", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp",
		AccountExpiry: start.Add(30 * time.Hour), MaxConcurrentUsers: 1, MonitorStatus: "alive",
	})
	if err != nil {
		t.Fatal(err)
	}
	cardID, err := repo.CreateCard(ctx, CardSeed{CodeHash: cardHashForCode("EXPI-REDA-0001"), CodeSuffix: "0001", DurationDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RedeemCard(ctx, cardID); err != nil {
		t.Fatal(err)
	}
	spare, err := repo.CreateAccount(ctx, AccountSeed{
		DisplayUsername: "spare", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp",
		AccountExpiry: start.Add(30 * 24 * time.Hour), MaxConcurrentUsers: 1, MonitorStatus: "alive",
	})
	if err != nil {
		t.Fatal(err)
	}

	// 到期前 24 小时内换号：老账号进入宽限期，顾客还在上面，不能下线。
	first, err := repo.ProcessReplacements(ctx, start.Add(7*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Replaced) != 1 || first.Replaced[0].NewAccountID != spare || len(first.Retired) != 0 {
		t.Fatalf("run=%+v want one replacement onto %d and nothing retired yet", first, spare)
	}
	var archivedAt sql.NullString
	if err := db.DB().QueryRow("SELECT archived_at FROM chatgpt_accounts WHERE id=?", expiring).Scan(&archivedAt); err != nil {
		t.Fatal(err)
	}
	if archivedAt.Valid {
		t.Fatal("expiring account was taken offline while a customer was still in the grace window")
	}

	// 宽限期结束（此时账号也已过期）：应当自动下线。
	second, err := repo.ProcessReplacements(ctx, start.Add(32*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if second.GraceExpired != 1 {
		t.Fatalf("grace should have expired: %+v", second)
	}
	if len(second.Retired) != 1 || second.Retired[0].AccountID != expiring || second.Retired[0].Reason != RetireReasonExpired {
		t.Fatalf("run=%+v want account %d retired as expired", second.Retired, expiring)
	}
	var status string
	var allocations int
	var passwordLen int
	if err := db.DB().QueryRow("SELECT status,current_allocations,archived_at,length(display_password_secret) FROM chatgpt_accounts WHERE id=?", expiring).
		Scan(&status, &allocations, &archivedAt, &passwordLen); err != nil {
		t.Fatal(err)
	}
	if status != "disabled" || allocations != 0 || !archivedAt.Valid || passwordLen != 0 {
		t.Fatalf("offline state status=%s allocations=%d archived=%v password_len=%d", status, allocations, archivedAt.Valid, passwordLen)
	}
	var retireAudit int
	if err := db.DB().QueryRow("SELECT count(*) FROM audit_events WHERE action='account.retired' AND target_id=? AND metadata_json LIKE '%expired%'", expiring).Scan(&retireAudit); err != nil {
		t.Fatal(err)
	}
	if retireAudit != 1 {
		t.Fatalf("expired retirement audit count=%d want 1", retireAudit)
	}
	// 换到新账号的顾客不受影响。
	var activeAccount int64
	if err := db.DB().QueryRow("SELECT account_id FROM allocations WHERE card_id=? AND active=1 AND allocation_state='primary'", cardID).Scan(&activeAccount); err != nil {
		t.Fatal(err)
	}
	if activeAccount != spare {
		t.Fatalf("customer allocation moved to %d want %d", activeAccount, spare)
	}
}

// 监控推来的 dead_normal 事件应当让空闲账号立刻下线。
func TestMonitorDeadNormalEventRetiresIdleAccount(t *testing.T) {
	ctx := context.Background()
	database := openStore(t)
	defer database.Close()
	repo := New(database.DB(), testCredentialKeyring(t))
	now := time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return now })

	if _, err := repo.ApplyMonitorAccountEvent(ctx, accountsync.Event{
		EventID: "expired-created", Version: 1, Type: accountsync.EventAccountCreated, OccurredAt: now,
		ProviderAccountID: "monitor-expiring", Email: "expiring@example.test", Plan: "plus",
		SubscriptionExpiry: now.Add(24 * time.Hour), Status: "alive",
	}); err != nil {
		t.Fatal(err)
	}
	result, err := repo.ApplyMonitorAccountEvent(ctx, accountsync.Event{
		EventID: "expired-dead", Version: 2, Type: accountsync.EventAccountUpdated, OccurredAt: now,
		ProviderAccountID: "monitor-expiring", Email: "expiring@example.test", Plan: "plus",
		SubscriptionExpiry: now.Add(24 * time.Hour), Status: "dead_normal",
	})
	if err != nil || !result.Retired {
		t.Fatalf("dead_normal event should retire an idle account: %+v err=%v", result, err)
	}
	var status string
	var archivedAt sql.NullString
	if err := database.DB().QueryRow("SELECT status,archived_at FROM chatgpt_accounts WHERE monitor_account_id='monitor-expiring'").
		Scan(&status, &archivedAt); err != nil {
		t.Fatal(err)
	}
	if status != "disabled" || !archivedAt.Valid {
		t.Fatalf("status=%s archived=%v", status, archivedAt.Valid)
	}
}

func TestReplacementScanExpiresDueCardsAndFreesCapacity(t *testing.T) {
	db := openStore(t)
	defer db.Close()
	repo := New(db.DB(), testCredentialKeyring(t))
	ctx := context.Background()
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return start })

	accountID, err := repo.CreateAccount(ctx, AccountSeed{
		DisplayUsername: "expiry-sweep", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp",
		AccountExpiry: start.Add(90 * 24 * time.Hour), MaxConcurrentUsers: 3, MonitorStatus: "alive",
	})
	if err != nil {
		t.Fatal(err)
	}
	cardID, err := repo.CreateCard(ctx, CardSeed{CodeHash: cardHashForCode("EXPI-REDU-E001"), CodeSuffix: "E001", DurationDays: 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RedeemCard(ctx, cardID); err != nil {
		t.Fatal(err)
	}

	// 卡到期 5 天后才跑扫描：过去没有任何后台任务会结算它，坑位被永久占住。
	run, err := repo.ProcessReplacements(ctx, start.Add(9*24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if run.CardsExpired != 1 {
		t.Fatalf("cards_expired=%d want 1", run.CardsExpired)
	}
	var cardStatus, allocationState string
	var active int
	if err := db.DB().QueryRow("SELECT status FROM cards WHERE id=?", cardID).Scan(&cardStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.DB().QueryRow("SELECT allocation_state,active FROM allocations WHERE card_id=?", cardID).Scan(&allocationState, &active); err != nil {
		t.Fatal(err)
	}
	if cardStatus != "expired" || allocationState != "expired" || active != 0 {
		t.Fatalf("card=%s allocation=%s active=%d want expired/expired/0", cardStatus, allocationState, active)
	}
	account, err := repo.Account(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if account.CurrentAllocations != 0 {
		t.Fatalf("current_allocations=%d want 0 after card expiry", account.CurrentAllocations)
	}
}

func TestStaleFullAccountIsReconciledAndRedeemableAgain(t *testing.T) {
	db := openStore(t)
	defer db.Close()
	repo := New(db.DB(), testCredentialKeyring(t))
	ctx := context.Background()
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return start })

	accountID, err := repo.CreateAccount(ctx, AccountSeed{
		DisplayUsername: "stale-full", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp",
		AccountExpiry: start.Add(90 * 24 * time.Hour), MaxConcurrentUsers: 1, MonitorStatus: "alive",
	})
	if err != nil {
		t.Fatal(err)
	}
	firstCard, err := repo.CreateCard(ctx, CardSeed{CodeHash: cardHashForCode("STAL-EFUL-L001"), CodeSuffix: "L001", DurationDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RedeemCard(ctx, firstCard); err != nil {
		t.Fatal(err)
	}
	// 容量上调没有重算 status：账号停在 'full'，选号 SQL 永远选不中它，
	// 而库存看板仍把这 2 个坑算成空余容量——线上"有容量却兑不了"就是这么来的。
	if _, err := db.DB().Exec("UPDATE chatgpt_accounts SET max_concurrent_users=3 WHERE id=?", accountID); err != nil {
		t.Fatal(err)
	}
	blocked, err := repo.CreateCard(ctx, CardSeed{CodeHash: cardHashForCode("STAL-EFUL-L002"), CodeSuffix: "L002", DurationDays: 30})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.RedeemCard(ctx, blocked); !errors.Is(err, ErrNoAccountCapacity) {
		t.Fatalf("redeem before reconcile err=%v want ErrNoAccountCapacity", err)
	}

	run, err := repo.ProcessReplacements(ctx, start.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if run.AvailabilityRestored != 1 || run.MarkedFull != 0 {
		t.Fatalf("restored=%d marked_full=%d want 1/0", run.AvailabilityRestored, run.MarkedFull)
	}
	account, err := repo.Account(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != "available" {
		t.Fatalf("status=%s want available", account.Status)
	}
	if _, err := repo.RedeemCard(ctx, blocked); err != nil {
		t.Fatalf("redeem after reconcile: %v", err)
	}
}

func TestReconcileMarksOversubscribedAccountFull(t *testing.T) {
	db := openStore(t)
	defer db.Close()
	repo := New(db.DB(), testCredentialKeyring(t))
	ctx := context.Background()
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return start })

	accountID, err := repo.CreateAccount(ctx, AccountSeed{
		DisplayUsername: "over-subscribed", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp",
		AccountExpiry: start.Add(90 * 24 * time.Hour), MaxConcurrentUsers: 5, MonitorStatus: "alive",
	})
	if err != nil {
		t.Fatal(err)
	}
	// 容量下调同样不会重算 status，反向漂移必须一并纠正。
	if _, err := db.DB().Exec("UPDATE chatgpt_accounts SET current_allocations=5,max_concurrent_users=2,status='available' WHERE id=?", accountID); err != nil {
		t.Fatal(err)
	}
	run, err := repo.ProcessReplacements(ctx, start.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if run.MarkedFull != 1 || run.AvailabilityRestored != 0 {
		t.Fatalf("marked_full=%d restored=%d want 1/0", run.MarkedFull, run.AvailabilityRestored)
	}
	account, err := repo.Account(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if account.Status != "full" {
		t.Fatalf("status=%s want full", account.Status)
	}
}

func TestReconcileLeavesSuspectedAndArchivedAccountsAlone(t *testing.T) {
	db := openStore(t)
	defer db.Close()
	repo := New(db.DB(), testCredentialKeyring(t))
	ctx := context.Background()
	start := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return start })

	suspected, err := repo.CreateAccount(ctx, AccountSeed{
		DisplayUsername: "suspect-hold", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp",
		AccountExpiry: start.Add(90 * 24 * time.Hour), MaxConcurrentUsers: 5, MonitorStatus: "alive",
	})
	if err != nil {
		t.Fatal(err)
	}
	// 疑似封禁的账号有空余坑位，但必须留在分配池外，等人工确认。
	if _, err := db.DB().Exec("UPDATE chatgpt_accounts SET status='suspected',current_allocations=1 WHERE id=?", suspected); err != nil {
		t.Fatal(err)
	}
	archived, err := repo.CreateAccount(ctx, AccountSeed{
		DisplayUsername: "archived-hold", DisplayPassword: "secret-password", DisplayTOTPSecret: "secret-totp",
		AccountExpiry: start.Add(90 * 24 * time.Hour), MaxConcurrentUsers: 5, MonitorStatus: "alive",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB().Exec("UPDATE chatgpt_accounts SET status='full',current_allocations=0,archived_at=? WHERE id=?", formatTime(start), archived); err != nil {
		t.Fatal(err)
	}

	run, err := repo.ProcessReplacements(ctx, start.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if run.AvailabilityRestored != 0 || run.MarkedFull != 0 {
		t.Fatalf("restored=%d marked_full=%d want 0/0", run.AvailabilityRestored, run.MarkedFull)
	}
	var suspectedStatus, archivedStatus string
	if err := db.DB().QueryRow("SELECT status FROM chatgpt_accounts WHERE id=?", suspected).Scan(&suspectedStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.DB().QueryRow("SELECT status FROM chatgpt_accounts WHERE id=?", archived).Scan(&archivedStatus); err != nil {
		t.Fatal(err)
	}
	if suspectedStatus != "suspected" || archivedStatus != "full" {
		t.Fatalf("suspected=%s archived=%s want suspected/full", suspectedStatus, archivedStatus)
	}
}
