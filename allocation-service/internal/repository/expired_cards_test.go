package repository

import (
	"context"
	"testing"
	"time"
)

func TestExpiredCardsIncludeHistoricalAccountsAndLiveUsage(t *testing.T) {
	db := openStore(t)
	defer db.Close()
	repo := New(db.DB(), testCredentialKeyring(t))
	ctx := context.Background()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return now })
	account := func(name string) int64 {
		t.Helper()
		id, err := repo.CreateAccount(ctx, AccountSeed{DisplayUsername: name, DisplayPassword: "not-returned-password", DisplayTOTPSecret: "not-returned-totp", AccountExpiry: now.Add(24 * time.Hour), MaxConcurrentUsers: 5})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	oldAccount, newAccount := account("old@example.test"), account("shared@example.test")
	if _, err := db.DB().Exec(`UPDATE chatgpt_accounts SET archived_at=?,status='disabled' WHERE id=?`, formatTime(now), oldAccount); err != nil {
		t.Fatal(err)
	}
	card := func(n int, status string, expires time.Time) int64 {
		t.Helper()
		id, err := repo.CreateCard(ctx, CardSeed{CodeHash: hashFor(9000 + n), CodeSuffix: suffixFor(9000 + n), DurationDays: 7})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB().Exec(`UPDATE cards SET status=?,redeemed_at=?,expires_at=? WHERE id=?`, status, formatTime(expires.Add(-7*24*time.Hour)), formatTime(expires), id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	insert := func(cardID, accountID int64, allocated, validUntil time.Time, state string, active int) int64 {
		t.Helper()
		res, err := db.DB().Exec(`INSERT INTO allocations(card_id,account_id,allocated_at,valid_until,allocation_state,active,created_at,updated_at)
			VALUES (?,?,?,?,?,?,?,?)`, cardID, accountID, formatTime(allocated), formatTime(validUntil), state, active, formatTime(allocated), formatTime(allocated))
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		return id
	}
	expired := card(0, "expired", now.Add(-2*time.Hour))
	due := card(1, "redeemed", now.Add(-time.Hour))
	active := card(2, "redeemed", now.Add(12*time.Hour))
	revoked := card(3, "revoked", now.Add(-time.Hour))
	unused := card(4, "unused", now.Add(-time.Hour))
	orphan := card(5, "expired", now.Add(-3*time.Hour))
	insert(expired, oldAccount, now.Add(-7*24*time.Hour), now.Add(-2*time.Hour), "replaced", 0)
	insert(expired, newAccount, now.Add(-6*24*time.Hour), now.Add(-2*time.Hour), "expired", 0)
	// A second allocation on the same account must not duplicate the account.
	insert(expired, oldAccount, now.Add(-5*24*time.Hour), now.Add(-2*time.Hour), "expired", 0)
	insert(due, newAccount, now.Add(-7*24*time.Hour), now.Add(-time.Hour), "primary", 1)
	primaryID := insert(active, newAccount, now.Add(-time.Hour), now.Add(12*time.Hour), "primary", 1)
	if _, err := db.DB().Exec(`INSERT INTO allocations(card_id,account_id,allocated_at,valid_until,allocation_state,active,grace_until,superseded_by_allocation_id,created_at,updated_at)
		VALUES (?,?,?,?,'grace',1,?,?,?,?)`, active, oldAccount, formatTime(now.Add(-2*time.Hour)), formatTime(now.Add(12*time.Hour)), formatTime(now.Add(time.Hour)), primaryID, formatTime(now), formatTime(now)); err != nil {
		t.Fatal(err)
	}
	insert(revoked, newAccount, now.Add(-7*24*time.Hour), now.Add(-time.Hour), "revoked", 0)
	insert(unused, newAccount, now.Add(-7*24*time.Hour), now.Add(-time.Hour), "expired", 0)

	page, err := repo.ListExpiredCards(ctx, now, 1, 20, "")
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 3 || len(page.Cards) != 3 || page.Cards[0].ID != due || page.Cards[1].ID != expired || page.Cards[2].ID != orphan {
		t.Fatalf("unexpected expired cards: %+v", page)
	}
	if len(page.Cards[2].Accounts) != 0 {
		t.Fatal("card without allocations should have an empty account list")
	}
	accounts := page.Cards[1].Accounts
	if len(accounts) != 2 || accounts[0].ID != oldAccount || !accounts[0].Archived || accounts[0].ActiveCardCount != 1 || accounts[1].ID != newAccount || accounts[1].ActiveCardCount != 1 {
		t.Fatalf("historical accounts or live primary/grace usage incorrect: %+v", accounts)
	}
	filtered, err := repo.ListExpiredCards(ctx, now, 1, 1, "SHARED@EXAMPLE.TEST")
	if err != nil || filtered.Total != 2 || len(filtered.Cards) != 1 || filtered.Cards[0].ID != due {
		t.Fatalf("filtered first page: %+v err=%v", filtered, err)
	}
	filtered, err = repo.ListExpiredCards(ctx, now, 2, 1, "SHARED@EXAMPLE.TEST")
	if err != nil || len(filtered.Cards) != 1 || filtered.Cards[0].ID != expired || len(filtered.Cards[0].Accounts) != 2 {
		t.Fatalf("pagination must retain all accounts for the matched card: %+v err=%v", filtered, err)
	}
	filtered, err = repo.ListExpiredCards(ctx, now, 1, 20, "no-match")
	if err != nil || filtered.Total != 0 || len(filtered.Cards) != 0 {
		t.Fatalf("empty search: %+v err=%v", filtered, err)
	}
	// Reactivation/extension immediately removes a card from the current expiry list.
	if _, err := db.DB().Exec(`UPDATE cards SET status='redeemed',expires_at=? WHERE id=?`, formatTime(now.Add(24*time.Hour)), expired); err != nil {
		t.Fatal(err)
	}
	page, err = repo.ListExpiredCards(ctx, now, 1, 20, "")
	if err != nil || page.Total != 2 {
		t.Fatalf("renewed card is still in expired list: %+v err=%v", page, err)
	}
}

func TestExpiredCardsOnlyIncludeLast72Hours(t *testing.T) {
	db := openStore(t)
	defer db.Close()
	repo := New(db.DB(), testCredentialKeyring(t))
	ctx := context.Background()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	repo.SetNow(func() time.Time { return now })
	testCases := []struct {
		status  string
		expires time.Time
		include bool
	}{
		{"expired", now.Add(-72*time.Hour - time.Second), false},
		{"expired", now.Add(-72 * time.Hour), true},
		{"redeemed", now.Add(-72*time.Hour + time.Second), true},
		{"expired", now.Add(-24 * time.Hour), true},
		{"redeemed", now, true},
		{"redeemed", now.Add(time.Second), false},
		{"unused", now.Add(-time.Hour), false},
		{"revoked", now.Add(-time.Hour), false},
	}
	wantIDs := make(map[int64]bool)
	for i, tc := range testCases {
		id, err := repo.CreateCard(ctx, CardSeed{CodeHash: hashFor(9100 + i), CodeSuffix: suffixFor(9100 + i), DurationDays: 7})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.DB().Exec(`UPDATE cards SET status=?,redeemed_at=?,expires_at=? WHERE id=?`, tc.status, formatTime(tc.expires.Add(-7*24*time.Hour)), formatTime(tc.expires), id); err != nil {
			t.Fatal(err)
		}
		if tc.include {
			wantIDs[id] = true
		}
	}
	for _, search := range []string{"", "91"} {
		for pageNumber := 1; pageNumber <= 2; pageNumber++ {
			page, err := repo.ListExpiredCards(ctx, now, pageNumber, 2, search)
			if err != nil {
				t.Fatal(err)
			}
			if page.Total != 4 || len(page.Cards) != 2 {
				t.Fatalf("window total/pagination incorrect: search=%q page=%+v", search, page)
			}
			for _, card := range page.Cards {
				if !wantIDs[card.ID] {
					t.Fatalf("card outside the expiry window included: %+v", card)
				}
			}
		}
	}
	// Searching for an older card must not bypass the three-day window.
	page, err := repo.ListExpiredCards(ctx, now, 1, 20, "9100")
	if err != nil || page.Total != 0 || len(page.Cards) != 0 {
		t.Fatalf("search included a card older than 72 hours: page=%+v err=%v", page, err)
	}
}
