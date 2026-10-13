package repository

import (
	"context"
	"database/sql"
	"time"
)

// passwordChangeExpiryLeadDays is the minimum number of days a GPT account's
// own subscription must still have left before it is flagged. The rule: the
// passenger's subscription expired, but the GPT account they were assigned to
// still has more than this many days to run, so the account is worth keeping
// (and therefore worth re-securing by changing its password).
const passwordChangeExpiryLeadDays = 3

// PasswordChangeDueAccount is a read-only projection of an account that needs a
// password change. It deliberately carries no credentials or token material.
type PasswordChangeDueAccount struct {
	ID                 int64      `json:"id"`
	DisplayUsername    string     `json:"display_username"`
	Status             string     `json:"status"`
	MonitorStatus      string     `json:"monitor_status"`
	AccountExpiry      time.Time  `json:"account_expiry"`
	LastCardExpiry     time.Time  `json:"last_expired_card_expires_at"`
	ExpiredCardCount   int        `json:"expired_card_count"`
	ClearedAt          *time.Time `json:"cleared_at,omitempty"`
	DaysAccountRemains int        `json:"days_account_remains"`
	DaysCardExpired    int        `json:"days_expired_card_expired_ago"`
}

// passwordChangeDueQuery implements the dashboard rule. A card only counts as
// the passenger's subscription of record when it is that card's latest
// allocation, so passengers who were rotated onto a different account are not
// double-counted against the old one.
//
// latest_allocation relies on SQLite's documented bare-column MAX behaviour:
// card_id/account_id are taken from the row holding the max allocated_at. This
// mirrors the "linked" CTE already used by ListExpiredCards.
// The card timestamp is derived from the single julianday MAX rather than a
// second string MAX: formatTime emits variable-precision RFC3339, so raw string
// ordering is not chronological ("...00.5Z" sorts below "...00Z").
const passwordChangeDueQuery = `
WITH latest_allocation AS (
	SELECT a.card_id, a.account_id, max(julianday(a.allocated_at)) AS last_at
	FROM allocations a GROUP BY a.card_id
), due AS (
	SELECT la.account_id,
	       count(DISTINCT la.card_id) AS card_count,
	       max(julianday(c.expires_at)) AS max_card_expiry,
	       strftime('%Y-%m-%dT%H:%M:%fZ', max(julianday(c.expires_at))) AS last_card_expiry
	FROM latest_allocation la
	JOIN cards c ON c.id = la.card_id
	WHERE c.expires_at IS NOT NULL
	  AND julianday(c.expires_at) <= julianday(?)
	GROUP BY la.account_id
)
SELECT ac.id, ac.display_username, ac.status, ac.monitor_status, ac.account_expiry,
       due.last_card_expiry, due.card_count, ac.password_change_cleared_at
FROM due
JOIN chatgpt_accounts ac ON ac.id = due.account_id
WHERE ac.archived_at IS NULL
  AND julianday(ac.account_expiry) > julianday(?) + ?
  AND (ac.password_change_cleared_at IS NULL
       OR julianday(ac.password_change_cleared_at) <= due.max_card_expiry)
ORDER BY julianday(ac.account_expiry) DESC, ac.id ASC`

// ListPasswordChangeDue returns every non-archived account that currently needs
// a password change. An account stays out of the list until a later card
// expiry re-arms the condition after it was cleared.
func (r *Repository) ListPasswordChangeDue(ctx context.Context, now time.Time) ([]PasswordChangeDueAccount, error) {
	stamp := formatTime(now.UTC())
	rows, err := r.db.QueryContext(ctx, passwordChangeDueQuery,
		stamp, stamp, float64(passwordChangeExpiryLeadDays))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]PasswordChangeDueAccount, 0)
	for rows.Next() {
		var item PasswordChangeDueAccount
		var accountExpiry, lastCardExpiry string
		var clearedAt sql.NullString
		if err := rows.Scan(&item.ID, &item.DisplayUsername, &item.Status, &item.MonitorStatus,
			&accountExpiry, &lastCardExpiry, &item.ExpiredCardCount, &clearedAt); err != nil {
			return nil, err
		}
		expiry, err := parseTime(accountExpiry)
		if err != nil {
			return nil, err
		}
		cardExpiry, err := parseTime(lastCardExpiry)
		if err != nil {
			return nil, err
		}
		item.AccountExpiry = expiry
		item.LastCardExpiry = cardExpiry
		if clearedAt.Valid && clearedAt.String != "" {
			cleared, err := parseTime(clearedAt.String)
			if err != nil {
				return nil, err
			}
			item.ClearedAt = &cleared
		}
		item.DaysAccountRemains = wholeDaysBetween(now.UTC(), expiry)
		item.DaysCardExpired = wholeDaysBetween(cardExpiry, now.UTC())
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// MarkPasswordChangeCleared records that the operator has changed this
// account's password, which hides it from the dashboard until a later card
// expiry makes the condition true again.
func (r *Repository) MarkPasswordChangeCleared(ctx context.Context, accountID int64, now time.Time) error {
	result, err := r.db.ExecContext(ctx, `
		UPDATE chatgpt_accounts
		SET password_change_cleared_at = ?, updated_at = ?
		WHERE id = ? AND archived_at IS NULL`,
		formatTime(now.UTC()), formatTime(now.UTC()), accountID)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// wholeDaysBetween floors the number of whole days from start to end. It is
// positive when end lies in the future.
func wholeDaysBetween(start, end time.Time) int {
	return int(end.Sub(start).Hours() / 24)
}
