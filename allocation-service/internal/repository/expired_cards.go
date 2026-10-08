package repository

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// These read-only views deliberately contain no card plaintext or credentials.
type ExpiredCardAccount struct {
	ID              int64     `json:"id"`
	DisplayUsername string    `json:"display_username"`
	Status          string    `json:"status"`
	Archived        bool      `json:"archived"`
	LastAllocatedAt time.Time `json:"last_allocated_at"`
	ActiveCardCount int       `json:"active_card_count"`
}

type ExpiredCard struct {
	ID         int64                `json:"id"`
	CodeSuffix string               `json:"code_suffix"`
	ExpiresAt  time.Time            `json:"expires_at"`
	Accounts   []ExpiredCardAccount `json:"accounts"`
}

type ExpiredCardPage struct {
	Cards    []ExpiredCard `json:"cards"`
	Total    int           `json:"total"`
	Page     int           `json:"page"`
	PageSize int           `json:"page_size"`
}

func (r *Repository) ListExpiredCards(ctx context.Context, now time.Time, page, pageSize int, search string) (ExpiredCardPage, error) {
	result := ExpiredCardPage{Cards: make([]ExpiredCard, 0), Page: page, PageSize: pageSize}
	// Include due cards before the hourly expiry scanner has changed their status.
	// The cleanup window is the last 72 hours, including both endpoints.
	// Searching by one historical account still returns every account for the card.
	where := `c.status IN ('expired','redeemed') AND julianday(c.expires_at)<=julianday(?)
		AND julianday(c.expires_at)>=julianday(?)
		AND (?='' OR instr(lower(c.code_suffix),lower(?))>0 OR CAST(c.id AS TEXT)=?
		OR EXISTS (SELECT 1 FROM allocations a JOIN chatgpt_accounts ac ON ac.id=a.account_id
			WHERE a.card_id=c.id AND instr(lower(ac.display_username),lower(?))>0))`
	search = strings.TrimSpace(search)
	stamp := formatTime(now.UTC())
	args := []any{stamp, formatTime(now.UTC().Add(-72 * time.Hour)), search, search, search, search}
	tx, err := r.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return result, err
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM cards c WHERE `+where, args...).Scan(&result.Total); err != nil {
		return result, err
	}
	queryArgs := append(append([]any{}, args...), pageSize, (page-1)*pageSize, stamp, stamp, stamp)
	rows, err := tx.QueryContext(ctx, `WITH selected AS (
		SELECT c.id,c.code_suffix,c.expires_at FROM cards c WHERE `+where+`
		ORDER BY julianday(c.expires_at) DESC,c.id DESC LIMIT ? OFFSET ?
	), linked AS (
		SELECT a.card_id,a.account_id,max(julianday(a.allocated_at)) AS last_at
		FROM allocations a JOIN selected s ON s.id=a.card_id GROUP BY a.card_id,a.account_id
	), usage AS (
		SELECT a.account_id,count(DISTINCT a.card_id) AS active_cards
		FROM allocations a JOIN cards c ON c.id=a.card_id
		WHERE a.active=1 AND a.allocation_state IN ('primary','grace') AND c.status='redeemed'
		AND julianday(c.expires_at)>julianday(?) AND julianday(a.valid_until)>julianday(?)
		AND (a.allocation_state='primary' OR julianday(a.grace_until)>julianday(?))
		GROUP BY a.account_id
	)
	SELECT s.id,s.code_suffix,s.expires_at,ac.id,ac.display_username,ac.status,ac.archived_at,
		strftime('%Y-%m-%dT%H:%M:%fZ',l.last_at),COALESCE(u.active_cards,0)
	FROM selected s LEFT JOIN linked l ON l.card_id=s.id
	LEFT JOIN chatgpt_accounts ac ON ac.id=l.account_id LEFT JOIN usage u ON u.account_id=ac.id
	ORDER BY julianday(s.expires_at) DESC,s.id DESC,l.last_at DESC,ac.id DESC`, queryArgs...)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var id int64
		var suffix, expires string
		var accountID sql.NullInt64
		var username, status, archived, allocated sql.NullString
		var activeCards int
		if err := rows.Scan(&id, &suffix, &expires, &accountID, &username, &status, &archived, &allocated, &activeCards); err != nil {
			rows.Close()
			return result, err
		}
		if len(result.Cards) == 0 || result.Cards[len(result.Cards)-1].ID != id {
			expiresAt, err := parseTime(expires)
			if err != nil {
				rows.Close()
				return result, err
			}
			result.Cards = append(result.Cards, ExpiredCard{ID: id, CodeSuffix: suffix, ExpiresAt: expiresAt, Accounts: make([]ExpiredCardAccount, 0)})
		}
		if accountID.Valid {
			allocatedAt, err := parseTime(allocated.String)
			if err != nil {
				rows.Close()
				return result, err
			}
			card := &result.Cards[len(result.Cards)-1]
			card.Accounts = append(card.Accounts, ExpiredCardAccount{
				ID: accountID.Int64, DisplayUsername: username.String, Status: status.String,
				Archived: archived.Valid, LastAllocatedAt: allocatedAt, ActiveCardCount: activeCards,
			})
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	return result, tx.Commit()
}
