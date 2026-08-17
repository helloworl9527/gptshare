package repository

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"allocation-service/internal/credential"
	"allocation-service/internal/models"
)

var (
	ErrAccountExpiryTooLong          = errors.New("account expiry cannot exceed 30 days")
	ErrAccountAllocated              = errors.New("account has active allocations")
	ErrAccountArchived               = errors.New("account is archived")
	ErrAccountReplacementUnavailable = errors.New("account replacement unavailable")
	ErrAccountCredentialsUnavailable = errors.New("account credentials unavailable")
	ErrCapacityTooSmall              = errors.New("max concurrent users is below current allocations")
	ErrCardStateConflict             = errors.New("card state does not allow this operation")
	ErrCardDurationLimit             = errors.New("card duration cannot exceed 90 days from redemption")
	ErrNoAccountCapacity             = errors.New("no account capacity")
	ErrCaptchaRequired               = errors.New("captcha required")
	ErrCaptchaInvalid                = errors.New("captcha invalid")
	ErrInvalidSetting                = errors.New("setting validation failed")
)

const (
	DefaultAccountCapacity       = 3
	MaxAccountCapacity           = 1000
	defaultAccountCapacityKey    = "default_account_capacity"
	defaultAccountCapacityActor  = "admin"
	defaultAccountCapacityTarget = "settings"
)

type Repository struct {
	db          *sql.DB
	now         func() time.Time
	credentials *credential.Keyring
}

type AccountSeed struct {
	DisplayUsername    string
	DisplayPassword    string
	DisplayTOTPSecret  string
	SourceURL          string
	AccountExpiry      time.Time
	MaxConcurrentUsers int
	MonitorAccountID   string
	Status             string
	MonitorStatus      string
}

type AccountUpdate struct {
	DisplayUsername    string
	DisplayPassword    string
	DisplayTOTPSecret  string
	SourceURL          *string
	AccountExpiry      time.Time
	MaxConcurrentUsers int
	Status             string
	MonitorStatus      string
	MonitorAccountID   string
}

type SyncedAccount struct {
	MonitorAccountID   string
	DisplayUsername    string
	AccountExpiry      time.Time
	MaxConcurrentUsers int
	MonitorStatus      string
}

type CardSeed struct {
	CodeHash      []byte
	CodeSuffix    string
	CodePlaintext string
	DurationDays  int
}

type CardFilter struct {
	Status       string
	DurationDays int
}

type RedeemResult struct {
	Allocation models.Allocation
	Card       models.Card
	Account    models.Account
}

type ReplacementResult struct {
	CardID          int64
	OldAllocationID int64
	NewAllocationID int64
	OldAccountID    int64
	NewAccountID    int64
	Reason          string
	GraceUntil      *time.Time
}

type ReplacementRun struct {
	Replaced     []ReplacementResult
	GraceExpired int
	Failed       int
}

type InventoryMetrics struct {
	Capacity               int
	Used                   int
	AvailableCapacity      int
	EligibleAccounts       int
	UnusedCards            int
	RedeemedLast7Days      int
	DailyRedemptionRate    float64
	DaysToExhaust          *float64
	RecommendedAccountAdd  int
	AverageAccountCapacity float64
	WarningLevel           string
	WarningLabel           string
	ExhaustionWindow       string
}

type RevealedCardCode struct {
	Card      models.Card
	Code      string
	Available bool
}

type replacementDueAllocation struct {
	allocationID  int64
	cardID        int64
	oldAccountID  int64
	cardExpiresAt time.Time
	reason        string
}

type AccountCredentials struct {
	Password   string
	TOTPSecret string
}

type MonitorSyncRun struct {
	ID             int64
	State          string
	StartedAt      time.Time
	FinishedAt     *time.Time
	AccountsTotal  int
	AccountsOK     int
	AccountsFailed int
	ErrorCode      string
}

type UserAllocationView struct {
	Allocation  models.Allocation
	Card        models.Card
	Account     models.Account
	Credentials AccountCredentials
}

// AdminAllocationView contains only the non-secret fields needed by the
// administrator allocation table.
type AdminAllocationView struct {
	Allocation models.Allocation
	Card       models.Card
	Account    models.Account
}

type CaptchaChallenge struct {
	ID        int64
	Question  string
	ExpiresAt time.Time
	Required  bool
	Failures  int
}

type AccountCapacitySettings struct {
	DefaultAccountCapacity int
}

type ApplyDefaultCapacityResult struct {
	DefaultAccountCapacity int
	UpdatedAccounts        int64
}

type RetireAccountResult struct {
	Archived            bool
	ReplacedAllocations int
	ClosedAllocations   int
}

type retireAllocation struct {
	id            int64
	cardID        int64
	state         string
	validUntil    time.Time
	cardStatus    string
	cardExpiresAt *time.Time
}

func New(db *sql.DB, credentials ...*credential.Keyring) *Repository {
	var keyring *credential.Keyring
	if len(credentials) > 0 {
		keyring = credentials[0]
	}
	return &Repository{db: db, credentials: keyring, now: func() time.Time { return time.Now().UTC() }}
}

func (r *Repository) SetNow(now func() time.Time) {
	if now != nil {
		r.now = now
	}
}

func (r *Repository) CreateAccount(ctx context.Context, seed AccountSeed) (int64, error) {
	now := r.now().UTC()
	if seed.MaxConcurrentUsers == 0 {
		defaultCapacity, err := r.DefaultAccountCapacity(ctx)
		if err != nil {
			return 0, err
		}
		seed.MaxConcurrentUsers = defaultCapacity
	}
	if err := validateAccountExpiry(now, seed.AccountExpiry); err != nil {
		return 0, err
	}
	if r.credentials == nil {
		return 0, credential.ErrInvalidKeyring
	}
	status := defaultString(seed.Status, "available")
	monitorStatus := defaultString(seed.MonitorStatus, "unknown_monitor")
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO chatgpt_accounts
		(display_username,display_password_secret,display_password_key_id,display_2fa_secret,display_2fa_key_id,account_expiry,max_concurrent_users,monitor_account_id,monitor_status,status,created_at,updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		seed.DisplayUsername, []byte{}, r.credentials.ActiveKeyID(), []byte{}, r.credentials.ActiveKeyID(),
		formatTime(seed.AccountExpiry.UTC()), seed.MaxConcurrentUsers, nullable(seed.MonitorAccountID), monitorStatus, status, formatTime(now), formatTime(now))
	if err != nil {
		return 0, err
	}
	accountID, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	password, err := r.credentials.Seal(accountID, credential.CredentialPassword, []byte(seed.DisplayPassword))
	if err != nil {
		return 0, err
	}
	totp, err := r.credentials.Seal(accountID, credential.CredentialTOTP, []byte(seed.DisplayTOTPSecret))
	if err != nil {
		return 0, err
	}
	var sourceKeyID any
	var sourceCiphertext any
	if strings.TrimSpace(seed.SourceURL) != "" {
		source, err := r.credentials.Seal(accountID, credential.CredentialSourceURL, []byte(seed.SourceURL))
		if err != nil {
			return 0, err
		}
		sourceKeyID = source.KeyID
		sourceCiphertext = source.Ciphertext
	}
	if _, err := tx.ExecContext(ctx, `UPDATE chatgpt_accounts
		SET display_password_secret=?, display_password_key_id=?, display_2fa_secret=?, display_2fa_key_id=?,
		    source_url_key_id=?, source_url_secret=?, updated_at=?
		WHERE id=?`,
		password.Ciphertext, password.KeyID, totp.Ciphertext, totp.KeyID,
		sourceKeyID, sourceCiphertext, formatTime(now), accountID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return accountID, nil
}

func (r *Repository) UpsertSyncedAccount(ctx context.Context, seed SyncedAccount) (models.Account, bool, error) {
	now := r.now().UTC()
	terminal := seed.MonitorStatus == "dead_normal" || seed.MonitorStatus == "dead_banned"
	if strings.TrimSpace(seed.MonitorAccountID) == "" || strings.TrimSpace(seed.DisplayUsername) == "" || seed.AccountExpiry.IsZero() || (seed.AccountExpiry.Before(now) && !terminal) {
		return models.Account{}, false, ErrAccountExpiryTooLong
	}
	capacity := seed.MaxConcurrentUsers
	if capacity == 0 {
		defaultCapacity, err := r.DefaultAccountCapacity(ctx)
		if err != nil {
			return models.Account{}, false, err
		}
		capacity = defaultCapacity
	}
	if !validAccountCapacity(capacity) {
		return models.Account{}, false, ErrInvalidSetting
	}
	var existingID int64
	var archivedAt sql.NullString
	err := r.db.QueryRowContext(ctx, `SELECT id,archived_at FROM chatgpt_accounts WHERE monitor_account_id=?`, seed.MonitorAccountID).Scan(&existingID, &archivedAt)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return models.Account{}, false, err
	}
	if existingID > 0 {
		if archivedAt.Valid {
			return models.Account{}, false, ErrAccountArchived
		}
		result, err := r.db.ExecContext(ctx, `UPDATE chatgpt_accounts
			SET display_username=?, account_expiry=?, monitor_status=?,
			    status=CASE
			        WHEN ?='dead_banned' THEN 'banned'
			        WHEN ?='dead_normal' THEN 'expired'
			        ELSE status
			    END,
			    updated_at=?
			WHERE id=?`,
			strings.TrimSpace(seed.DisplayUsername), formatTime(seed.AccountExpiry.UTC()),
			defaultString(seed.MonitorStatus, "unknown"), defaultString(seed.MonitorStatus, "unknown"), defaultString(seed.MonitorStatus, "unknown"),
			formatTime(now), existingID)
		if err != nil {
			return models.Account{}, false, err
		}
		affected, _ := result.RowsAffected()
		if affected != 1 {
			return models.Account{}, false, sql.ErrNoRows
		}
		account, err := r.Account(ctx, existingID)
		return account, false, err
	}
	if r.credentials == nil {
		return models.Account{}, false, credential.ErrInvalidKeyring
	}
	status := "pending_credentials"
	if seed.MonitorStatus == "dead_banned" {
		status = "banned"
	} else if seed.MonitorStatus == "dead_normal" {
		status = "expired"
	}
	result, err := r.db.ExecContext(ctx, `INSERT INTO chatgpt_accounts
		(display_username,display_password_secret,display_password_key_id,display_2fa_secret,display_2fa_key_id,account_expiry,max_concurrent_users,monitor_account_id,monitor_status,status,created_at,updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		strings.TrimSpace(seed.DisplayUsername), []byte{}, r.credentials.ActiveKeyID(), []byte{}, r.credentials.ActiveKeyID(),
		formatTime(seed.AccountExpiry.UTC()), capacity, seed.MonitorAccountID, defaultString(seed.MonitorStatus, "unknown"),
		status, formatTime(now), formatTime(now))
	if err != nil {
		return models.Account{}, false, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return models.Account{}, false, err
	}
	account, err := r.Account(ctx, id)
	return account, true, err
}

func (r *Repository) CreateCard(ctx context.Context, seed CardSeed) (int64, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	now := r.now().UTC()
	codeHash := seed.CodeHash
	if len(codeHash) == 0 {
		sum := sha256.Sum256([]byte(fmt.Sprintf("%s-%d", seed.CodeSuffix, now.UnixNano())))
		codeHash = sum[:]
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO cards(code_hash,code_suffix,duration_days,status,created_at,updated_at)
		VALUES (?,?,?,'unused',?,?)`, codeHash, seed.CodeSuffix, seed.DurationDays, formatTime(now), formatTime(now))
	if err != nil {
		return 0, err
	}
	cardID, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	if seed.CodePlaintext != "" {
		if r.credentials == nil {
			return 0, credential.ErrInvalidKeyring
		}
		sealed, err := r.credentials.SealWithAAD(credential.CardAAD(cardID), []byte(seed.CodePlaintext))
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE cards SET encrypted_code_key_id=?, encrypted_code=?, updated_at=? WHERE id=?`,
			sealed.KeyID, sealed.Ciphertext, formatTime(now), cardID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return cardID, nil
}

func (r *Repository) ListCards(ctx context.Context, filter CardFilter) ([]models.Card, error) {
	query := `SELECT id,code_hash,code_suffix,duration_days,status,redeemed_at,expires_at,revoked_at,created_at,updated_at,encrypted_code IS NOT NULL FROM cards`
	var args []any
	var where []string
	if filter.Status != "" {
		where = append(where, "status=?")
		args = append(args, filter.Status)
	}
	if filter.DurationDays != 0 {
		where = append(where, "duration_days=?")
		args = append(args, filter.DurationDays)
	}
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY id ASC"
	rows, err := r.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cards []models.Card
	for rows.Next() {
		card, err := scanCard(rows)
		if err != nil {
			return nil, err
		}
		cards = append(cards, card)
	}
	return cards, rows.Err()
}

func (r *Repository) CardByID(ctx context.Context, cardID int64) (models.Card, error) {
	return r.cardByQuery(ctx, "id=?", cardID)
}

func (r *Repository) CardByHash(ctx context.Context, codeHash []byte) (models.Card, error) {
	return r.cardByQuery(ctx, "code_hash=?", codeHash)
}

func (r *Repository) RevealCardCode(ctx context.Context, cardID int64) (RevealedCardCode, error) {
	card, err := r.CardByID(ctx, cardID)
	if err != nil {
		return RevealedCardCode{}, err
	}
	var keyID sql.NullString
	var ciphertext []byte
	if err := r.db.QueryRowContext(ctx, `SELECT encrypted_code_key_id,encrypted_code FROM cards WHERE id=?`, cardID).Scan(&keyID, &ciphertext); err != nil {
		return RevealedCardCode{}, err
	}
	if !keyID.Valid || len(ciphertext) == 0 {
		return RevealedCardCode{Card: card, Available: false}, nil
	}
	if r.credentials == nil {
		return RevealedCardCode{}, credential.ErrInvalidKeyring
	}
	plaintext, err := r.credentials.OpenWithAAD(credential.CardAAD(cardID), keyID.String, ciphertext)
	if err != nil {
		return RevealedCardCode{}, err
	}
	return RevealedCardCode{Card: card, Code: string(plaintext), Available: true}, nil
}

func (r *Repository) RevokeCard(ctx context.Context, cardID int64) (models.Card, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return models.Card{}, err
	}
	defer tx.Rollback()
	now := r.now().UTC()
	var status string
	if err := tx.QueryRowContext(ctx, "SELECT status FROM cards WHERE id=?", cardID).Scan(&status); err != nil {
		return models.Card{}, err
	}
	if status == "revoked" || status == "expired" {
		return models.Card{}, ErrCardStateConflict
	}
	if _, err := tx.ExecContext(ctx, `UPDATE allocations
		SET allocation_state='revoked', active=0, updated_at=?
		WHERE card_id=? AND active=1 AND allocation_state IN ('primary','grace')`, formatTime(now), cardID); err != nil {
		return models.Card{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE chatgpt_accounts
		SET current_allocations=max(0,current_allocations-1),
		    status=CASE WHEN status='full' THEN 'available' ELSE status END,
		    updated_at=?
		WHERE id IN (SELECT account_id FROM allocations WHERE card_id=? AND allocation_state='revoked')`,
		formatTime(now), cardID); err != nil {
		return models.Card{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE cards SET status='revoked', revoked_at=?, updated_at=? WHERE id=?`, formatTime(now), formatTime(now), cardID); err != nil {
		return models.Card{}, err
	}
	if err := tx.Commit(); err != nil {
		return models.Card{}, err
	}
	return r.CardByID(ctx, cardID)
}

func (r *Repository) ExtendCard(ctx context.Context, cardID int64, days int) (models.Card, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return models.Card{}, err
	}
	defer tx.Rollback()
	now := r.now().UTC()
	var status string
	var redeemedRaw, expiresRaw sql.NullString
	if err := tx.QueryRowContext(ctx, "SELECT status,redeemed_at,expires_at FROM cards WHERE id=?", cardID).Scan(&status, &redeemedRaw, &expiresRaw); err != nil {
		return models.Card{}, err
	}
	if status != "redeemed" || !redeemedRaw.Valid || !expiresRaw.Valid {
		return models.Card{}, ErrCardStateConflict
	}
	redeemedAt, err := parseTime(redeemedRaw.String)
	if err != nil {
		return models.Card{}, err
	}
	expiresAt, err := parseTime(expiresRaw.String)
	if err != nil {
		return models.Card{}, err
	}
	if !expiresAt.After(now) {
		return models.Card{}, ErrCardStateConflict
	}
	extended := expiresAt.Add(time.Duration(days) * 24 * time.Hour)
	if extended.After(redeemedAt.Add(90 * 24 * time.Hour)) {
		return models.Card{}, ErrCardDurationLimit
	}
	if _, err := tx.ExecContext(ctx, `UPDATE cards SET expires_at=?, updated_at=? WHERE id=?`, formatTime(extended), formatTime(now), cardID); err != nil {
		return models.Card{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE allocations
		SET valid_until=?, updated_at=?
		WHERE card_id=? AND active=1 AND allocation_state IN ('primary','grace')`, formatTime(extended), formatTime(now), cardID); err != nil {
		return models.Card{}, err
	}
	if err := tx.Commit(); err != nil {
		return models.Card{}, err
	}
	return r.CardByID(ctx, cardID)
}

func (r *Repository) ExpireDueCards(ctx context.Context, now time.Time) (int64, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stamp := now.UTC()
	rows, err := tx.QueryContext(ctx, `SELECT a.account_id,count(*)
		FROM allocations a JOIN cards c ON c.id=a.card_id
		WHERE c.status='redeemed' AND c.expires_at IS NOT NULL AND datetime(c.expires_at) <= datetime(?)
		  AND a.active=1 AND a.allocation_state IN ('primary','grace')
		GROUP BY a.account_id`, formatTime(stamp))
	if err != nil {
		return 0, err
	}
	releases := make(map[int64]int)
	for rows.Next() {
		var accountID int64
		var count int
		if err := rows.Scan(&accountID, &count); err != nil {
			rows.Close()
			return 0, err
		}
		releases[accountID] = count
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for accountID, count := range releases {
		if _, err := tx.ExecContext(ctx, `UPDATE chatgpt_accounts
			SET current_allocations=max(0,current_allocations-?),
			    status=CASE WHEN status='full' THEN 'available' ELSE status END,
			    updated_at=?
			WHERE id=?`, count, formatTime(stamp), accountID); err != nil {
			return 0, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE allocations
		SET allocation_state='expired', active=0, updated_at=?
		WHERE card_id IN (
			SELECT id FROM cards WHERE status='redeemed' AND expires_at IS NOT NULL AND datetime(expires_at) <= datetime(?)
		) AND active=1 AND allocation_state IN ('primary','grace')`, formatTime(stamp), formatTime(stamp)); err != nil {
		return 0, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE cards
		SET status='expired', updated_at=?
		WHERE status='redeemed' AND expires_at IS NOT NULL AND datetime(expires_at) <= datetime(?)`, formatTime(stamp), formatTime(stamp))
	if err != nil {
		return 0, err
	}
	count, _ := result.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

func (r *Repository) Audit(ctx context.Context, action, targetType string, targetID int64, metadata map[string]any) error {
	encoded := "{}"
	if len(metadata) > 0 {
		var parts []string
		for key, value := range metadata {
			parts = append(parts, fmt.Sprintf("%q:%q", key, fmt.Sprint(value)))
		}
		encoded = "{" + strings.Join(parts, ",") + "}"
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO audit_events(actor_type,action,target_type,target_id,metadata_json,created_at)
		VALUES ('admin',?,?,?,?,?)`, action, targetType, nullableInt(targetID), encoded, formatTime(r.now().UTC()))
	return err
}

func (r *Repository) RedeemCard(ctx context.Context, cardID int64) (models.Allocation, error) {
	result, err := r.redeemCard(ctx, "id=?", cardID, true)
	return result.Allocation, err
}

func (r *Repository) RedeemCode(ctx context.Context, codeHash []byte, monitorAvailable bool) (RedeemResult, error) {
	result, err := r.redeemCard(ctx, "code_hash=?", codeHash, monitorAvailable)
	if errors.Is(err, ErrCardStateConflict) {
		return r.existingRedeemByCodeHash(ctx, codeHash)
	}
	return result, err
}

func (r *Repository) ProcessReplacements(ctx context.Context, now time.Time) (ReplacementRun, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return ReplacementRun{}, err
	}
	defer tx.Rollback()
	stamp := now.UTC()
	run := ReplacementRun{}
	expiredGrace, err := expireGraceAllocations(ctx, tx, stamp)
	if err != nil {
		return ReplacementRun{}, err
	}
	run.GraceExpired = expiredGrace
	rows, err := tx.QueryContext(ctx, `SELECT
			a.id,a.card_id,a.account_id,a.valid_until,c.expires_at,acct.account_expiry,acct.monitor_status,acct.status
		FROM allocations a
		JOIN cards c ON c.id=a.card_id
		JOIN chatgpt_accounts acct ON acct.id=a.account_id
		WHERE c.status='redeemed'
		  AND a.active=1
		  AND a.allocation_state='primary'
		  AND datetime(c.expires_at) > datetime(?)
		  AND (
		    acct.monitor_status='dead_banned'
		    OR acct.status='banned'
		    OR datetime(acct.account_expiry) <= datetime(?, '+24 hours')
		  )
		ORDER BY CASE WHEN acct.monitor_status='dead_banned' THEN 0 ELSE 1 END, datetime(acct.account_expiry) ASC, a.id ASC`,
		formatTime(stamp), formatTime(stamp))
	if err != nil {
		return ReplacementRun{}, err
	}
	var due []replacementDueAllocation
	for rows.Next() {
		var item replacementDueAllocation
		var allocationValidRaw, cardExpiresRaw, accountExpiryRaw, monitorStatus, accountStatus string
		if err := rows.Scan(&item.allocationID, &item.cardID, &item.oldAccountID, &allocationValidRaw, &cardExpiresRaw, &accountExpiryRaw, &monitorStatus, &accountStatus); err != nil {
			rows.Close()
			return ReplacementRun{}, err
		}
		cardExpiresAt, err := parseTime(cardExpiresRaw)
		if err != nil {
			rows.Close()
			return ReplacementRun{}, err
		}
		item.cardExpiresAt = cardExpiresAt
		if monitorStatus == "dead_banned" || accountStatus == "banned" {
			item.reason = "banned"
		} else {
			item.reason = "account_expiring"
		}
		due = append(due, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return ReplacementRun{}, err
	}
	rows.Close()
	for _, item := range due {
		replaced, err := replaceOneAllocation(ctx, tx, stamp, item)
		if errors.Is(err, ErrNoAccountCapacity) {
			run.Failed++
			if auditErr := auditWithTx(ctx, tx, r.now, "replacement.failed", "card", item.cardID, map[string]any{"reason": item.reason, "old_account_id": item.oldAccountID}); auditErr != nil {
				return ReplacementRun{}, auditErr
			}
			continue
		}
		if err != nil {
			return ReplacementRun{}, err
		}
		run.Replaced = append(run.Replaced, replaced)
	}
	if err := tx.Commit(); err != nil {
		return ReplacementRun{}, err
	}
	return run, nil
}

func (r *Repository) InventoryMetrics(ctx context.Context, now time.Time) (InventoryMetrics, error) {
	stamp := now.UTC()
	var metrics InventoryMetrics
	if err := r.db.QueryRowContext(ctx, `SELECT
			coalesce(sum(max_concurrent_users),0),
			coalesce(sum(current_allocations),0),
			count(*),
			coalesce(avg(max_concurrent_users),0)
		FROM chatgpt_accounts
		WHERE datetime(account_expiry) > datetime(?)
		  AND archived_at IS NULL
		  AND status IN ('available','unknown_monitor','full')
		  AND monitor_status != 'dead_banned'`, formatTime(stamp)).
		Scan(&metrics.Capacity, &metrics.Used, &metrics.EligibleAccounts, &metrics.AverageAccountCapacity); err != nil {
		return InventoryMetrics{}, err
	}
	metrics.AvailableCapacity = metrics.Capacity - metrics.Used
	if metrics.AvailableCapacity < 0 {
		metrics.AvailableCapacity = 0
	}
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM cards WHERE status='unused'`).Scan(&metrics.UnusedCards); err != nil {
		return InventoryMetrics{}, err
	}
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM cards
		WHERE status='redeemed' AND redeemed_at IS NOT NULL
		  AND datetime(redeemed_at) >= datetime(?, '-7 days')
		  AND datetime(redeemed_at) <= datetime(?)`, formatTime(stamp), formatTime(stamp)).Scan(&metrics.RedeemedLast7Days); err != nil {
		return InventoryMetrics{}, err
	}
	metrics.DailyRedemptionRate = float64(metrics.RedeemedLast7Days) / 7.0
	if metrics.DailyRedemptionRate > 0 {
		days := float64(metrics.AvailableCapacity) / metrics.DailyRedemptionRate
		metrics.DaysToExhaust = &days
		targetCapacity := intCeil(metrics.DailyRedemptionRate*16) - metrics.AvailableCapacity
		if targetCapacity > 0 {
			accountCapacity := metrics.AverageAccountCapacity
			if accountCapacity < 1 {
				accountCapacity = 1
			}
			metrics.RecommendedAccountAdd = intCeil(float64(targetCapacity) / accountCapacity)
		}
	}
	metrics.WarningLevel, metrics.WarningLabel, metrics.ExhaustionWindow = inventoryWarning(metrics.AvailableCapacity, metrics.DaysToExhaust)
	return metrics, nil
}

func (r *Repository) redeemCard(ctx context.Context, where string, arg any, monitorAvailable bool) (RedeemResult, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return RedeemResult{}, err
	}
	defer tx.Rollback()
	now := r.now().UTC()
	var durationDays int
	var status string
	var cardID int64
	if err := tx.QueryRowContext(ctx, "SELECT id,duration_days,status FROM cards WHERE "+where, arg).Scan(&cardID, &durationDays, &status); err != nil {
		return RedeemResult{}, err
	}
	if status != "unused" {
		return RedeemResult{}, ErrCardStateConflict
	}
	cardExpiresAt := now.Add(time.Duration(durationDays) * 24 * time.Hour)
	accountID, err := selectCandidateAccount(ctx, tx, now, cardExpiresAt, monitorAvailable)
	if err != nil {
		return RedeemResult{}, err
	}
	result, err := tx.ExecContext(ctx, `UPDATE chatgpt_accounts
		SET current_allocations=current_allocations+1,last_allocated_at=?,updated_at=?,
		    status=CASE WHEN current_allocations+1 >= max_concurrent_users THEN 'full' ELSE status END
		WHERE id=? AND current_allocations < max_concurrent_users AND datetime(account_expiry) > datetime(?)`,
		formatTime(now), formatTime(now), accountID, formatTime(now))
	if err != nil {
		return RedeemResult{}, err
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return RedeemResult{}, ErrNoAccountCapacity
	}
	insert, err := tx.ExecContext(ctx, `INSERT INTO allocations
		(card_id,account_id,allocated_at,valid_until,allocation_state,active,created_at,updated_at)
		VALUES (?,?,?,?,'primary',1,?,?)`,
		cardID, accountID, formatTime(now), formatTime(cardExpiresAt), formatTime(now), formatTime(now))
	if err != nil {
		return RedeemResult{}, err
	}
	allocationID, err := insert.LastInsertId()
	if err != nil {
		return RedeemResult{}, err
	}
	updated, err := tx.ExecContext(ctx, `UPDATE cards SET status='redeemed',redeemed_at=?,expires_at=?,updated_at=? WHERE id=? AND status='unused'`,
		formatTime(now), formatTime(cardExpiresAt), formatTime(now), cardID)
	if err != nil {
		return RedeemResult{}, err
	} else if affected, _ := updated.RowsAffected(); affected != 1 {
		return RedeemResult{}, ErrCardStateConflict
	}
	if err := tx.Commit(); err != nil {
		return RedeemResult{}, err
	}
	card, err := r.CardByID(ctx, cardID)
	if err != nil {
		return RedeemResult{}, err
	}
	account, err := r.Account(ctx, accountID)
	if err != nil {
		return RedeemResult{}, err
	}
	return RedeemResult{
		Allocation: models.Allocation{ID: allocationID, CardID: cardID, AccountID: accountID, AllocatedAt: now, ValidUntil: cardExpiresAt, AllocationState: "primary", Active: true},
		Card:       card,
		Account:    account,
	}, nil
}

func (r *Repository) existingRedeemByCodeHash(ctx context.Context, codeHash []byte) (RedeemResult, error) {
	views, err := r.UserAllocationsByCodeHash(ctx, codeHash, r.now().UTC())
	if err != nil {
		return RedeemResult{}, err
	}
	view := views[0]
	return RedeemResult{
		Allocation: view.Allocation,
		Card:       view.Card,
		Account:    view.Account,
	}, nil
}

func (r *Repository) cardByQuery(ctx context.Context, where string, arg any) (models.Card, error) {
	row := r.db.QueryRowContext(ctx, `SELECT id,code_hash,code_suffix,duration_days,status,redeemed_at,expires_at,revoked_at,created_at,updated_at,encrypted_code IS NOT NULL FROM cards WHERE `+where, arg)
	return scanCard(row)
}

func (r *Repository) ActiveAllocationCount(ctx context.Context, accountID int64) (int, error) {
	var count int
	err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM allocations WHERE account_id=? AND active=1 AND allocation_state IN ('primary','grace')`, accountID).Scan(&count)
	return count, err
}

func (r *Repository) ListActiveAllocations(ctx context.Context) ([]AdminAllocationView, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT
			a.id,a.card_id,a.account_id,a.allocated_at,a.valid_until,a.grace_until,a.allocation_state,a.active,a.superseded_by_allocation_id,
			c.code_suffix,c.duration_days,
			ac.display_username,ac.account_expiry
		FROM allocations a
		JOIN cards c ON c.id=a.card_id
		JOIN chatgpt_accounts ac ON ac.id=a.account_id
		WHERE a.active=1 AND a.allocation_state IN ('primary','grace')
		ORDER BY datetime(a.allocated_at) DESC, a.id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	views := make([]AdminAllocationView, 0)
	for rows.Next() {
		var view AdminAllocationView
		var allocatedAt, validUntil, graceUntil, accountExpiry sql.NullString
		var active int
		var superseded sql.NullInt64
		if err := rows.Scan(
			&view.Allocation.ID, &view.Allocation.CardID, &view.Allocation.AccountID,
			&allocatedAt, &validUntil, &graceUntil, &view.Allocation.AllocationState,
			&active, &superseded, &view.Card.CodeSuffix, &view.Card.DurationDays,
			&view.Account.DisplayUsername, &accountExpiry,
		); err != nil {
			return nil, err
		}
		view.Allocation.Active = active == 1
		view.Allocation.AllocatedAt, err = parseTime(allocatedAt.String)
		if err != nil {
			return nil, err
		}
		view.Allocation.ValidUntil, err = parseTime(validUntil.String)
		if err != nil {
			return nil, err
		}
		if graceUntil.Valid {
			value, err := parseTime(graceUntil.String)
			if err != nil {
				return nil, err
			}
			view.Allocation.GraceUntil = &value
		}
		if superseded.Valid {
			value := superseded.Int64
			view.Allocation.SupersededByAllocationID = &value
		}
		view.Card.ID = view.Allocation.CardID
		view.Account.ID = view.Allocation.AccountID
		view.Account.AccountExpiry, err = parseTime(accountExpiry.String)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	return views, rows.Err()
}

func (r *Repository) Account(ctx context.Context, accountID int64) (models.Account, error) {
	row := r.db.QueryRowContext(ctx, `SELECT id,display_username,account_expiry,max_concurrent_users,current_allocations,monitor_account_id,monitor_status,status,last_allocated_at,source_url_key_id,source_url_secret
		FROM chatgpt_accounts WHERE id=? AND archived_at IS NULL`, accountID)
	return r.scanAccount(row)
}

func (r *Repository) ListAccounts(ctx context.Context) ([]models.Account, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id,display_username,account_expiry,max_concurrent_users,current_allocations,monitor_account_id,monitor_status,status,last_allocated_at,source_url_key_id,source_url_secret
		FROM chatgpt_accounts WHERE archived_at IS NULL ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var accounts []models.Account
	for rows.Next() {
		account, err := r.scanAccount(rows)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, account)
	}
	return accounts, rows.Err()
}

func (r *Repository) UpdateAccount(ctx context.Context, accountID int64, update AccountUpdate) (models.Account, error) {
	now := r.now().UTC()
	if err := validateAccountExpiry(now, update.AccountExpiry); err != nil && update.Status != "pending_credentials" {
		return models.Account{}, err
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return models.Account{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE chatgpt_accounts
		SET display_username=?, account_expiry=?, max_concurrent_users=?, monitor_account_id=?, monitor_status=?,
		    status=CASE
		        WHEN ?='available' AND current_allocations >= ? THEN 'full'
		        ELSE ?
		    END, updated_at=?
		WHERE id=? AND archived_at IS NULL`,
		update.DisplayUsername, formatTime(update.AccountExpiry.UTC()), update.MaxConcurrentUsers, nullable(update.MonitorAccountID),
		defaultString(update.MonitorStatus, "unknown_monitor"), defaultString(update.Status, "available"), update.MaxConcurrentUsers, defaultString(update.Status, "available"), formatTime(now), accountID)
	if err != nil {
		return models.Account{}, err
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return models.Account{}, sql.ErrNoRows
	}
	if update.DisplayPassword != "" || update.DisplayTOTPSecret != "" || (update.SourceURL != nil && strings.TrimSpace(*update.SourceURL) != "") {
		if r.credentials == nil {
			return models.Account{}, credential.ErrInvalidKeyring
		}
	}
	if update.DisplayPassword != "" {
		password, err := r.credentials.Seal(accountID, credential.CredentialPassword, []byte(update.DisplayPassword))
		if err != nil {
			return models.Account{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE chatgpt_accounts
			SET display_password_secret=?, display_password_key_id=?, updated_at=?
			WHERE id=?`, password.Ciphertext, password.KeyID, formatTime(now), accountID); err != nil {
			return models.Account{}, err
		}
	}
	if update.DisplayTOTPSecret != "" {
		totp, err := r.credentials.Seal(accountID, credential.CredentialTOTP, []byte(update.DisplayTOTPSecret))
		if err != nil {
			return models.Account{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE chatgpt_accounts
			SET display_2fa_secret=?, display_2fa_key_id=?, updated_at=?
			WHERE id=?`, totp.Ciphertext, totp.KeyID, formatTime(now), accountID); err != nil {
			return models.Account{}, err
		}
	}
	if update.SourceURL != nil {
		if strings.TrimSpace(*update.SourceURL) == "" {
			if _, err := tx.ExecContext(ctx, `UPDATE chatgpt_accounts
				SET source_url_secret=NULL, source_url_key_id=NULL, updated_at=?
				WHERE id=?`, formatTime(now), accountID); err != nil {
				return models.Account{}, err
			}
		} else {
			source, err := r.credentials.Seal(accountID, credential.CredentialSourceURL, []byte(*update.SourceURL))
			if err != nil {
				return models.Account{}, err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE chatgpt_accounts
				SET source_url_secret=?, source_url_key_id=?, updated_at=?
				WHERE id=?`, source.Ciphertext, source.KeyID, formatTime(now), accountID); err != nil {
				return models.Account{}, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return models.Account{}, err
	}
	return r.Account(ctx, accountID)
}

func (r *Repository) AccountCapacitySettings(ctx context.Context) (AccountCapacitySettings, error) {
	capacity, err := r.DefaultAccountCapacity(ctx)
	if err != nil {
		return AccountCapacitySettings{}, err
	}
	return AccountCapacitySettings{DefaultAccountCapacity: capacity}, nil
}

func (r *Repository) DefaultAccountCapacity(ctx context.Context) (int, error) {
	var raw []byte
	err := r.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=? AND is_secret=0`, defaultAccountCapacityKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultAccountCapacity, nil
	}
	if err != nil {
		return 0, err
	}
	value, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || !validAccountCapacity(value) {
		return 0, ErrInvalidSetting
	}
	return value, nil
}

func (r *Repository) defaultAccountCapacityTx(ctx context.Context, tx *sql.Tx) (int, error) {
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=? AND is_secret=0`, defaultAccountCapacityKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return DefaultAccountCapacity, nil
	}
	if err != nil {
		return 0, err
	}
	value, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || !validAccountCapacity(value) {
		return 0, ErrInvalidSetting
	}
	return value, nil
}

func (r *Repository) SetDefaultAccountCapacity(ctx context.Context, capacity int) (AccountCapacitySettings, error) {
	if !validAccountCapacity(capacity) {
		return AccountCapacitySettings{}, ErrInvalidSetting
	}
	now := r.now().UTC()
	if _, err := r.db.ExecContext(ctx, `INSERT INTO settings(key,value,is_secret,key_id,updated_at)
		VALUES (?,CAST(? AS BLOB),0,NULL,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value,is_secret=0,key_id=NULL,updated_at=excluded.updated_at`,
		defaultAccountCapacityKey, strconv.Itoa(capacity), formatTime(now)); err != nil {
		return AccountCapacitySettings{}, err
	}
	_ = r.Audit(ctx, "settings.default_account_capacity.update", defaultAccountCapacityTarget, 0, map[string]any{"default_account_capacity": capacity, "actor": defaultAccountCapacityActor})
	return AccountCapacitySettings{DefaultAccountCapacity: capacity}, nil
}

func (r *Repository) ApplyDefaultAccountCapacity(ctx context.Context) (ApplyDefaultCapacityResult, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return ApplyDefaultCapacityResult{}, err
	}
	defer tx.Rollback()
	capacity, err := r.defaultAccountCapacityTx(ctx, tx)
	if err != nil {
		return ApplyDefaultCapacityResult{}, err
	}
	now := r.now().UTC()
	result, err := tx.ExecContext(ctx, `UPDATE chatgpt_accounts
		SET max_concurrent_users=?,
		    status=CASE
		        WHEN current_allocations >= ? THEN 'full'
		        WHEN status='full' THEN 'available'
		        ELSE status
		    END,
		    updated_at=?
		WHERE archived_at IS NULL`,
		capacity, capacity, formatTime(now))
	if err != nil {
		return ApplyDefaultCapacityResult{}, err
	}
	updated, _ := result.RowsAffected()
	if err := tx.Commit(); err != nil {
		return ApplyDefaultCapacityResult{}, err
	}
	_ = r.Audit(ctx, "accounts.apply_default_capacity", "account_batch", 0, map[string]any{"default_account_capacity": capacity, "updated_accounts": updated})
	return ApplyDefaultCapacityResult{DefaultAccountCapacity: capacity, UpdatedAccounts: updated}, nil
}

func (r *Repository) UpdateAccountMonitorStatus(ctx context.Context, accountID int64, monitorAccountID, monitorStatus string) (models.Account, error) {
	now := r.now().UTC()
	result, err := r.db.ExecContext(ctx, `UPDATE chatgpt_accounts
		SET monitor_account_id=?, monitor_status=?,
		    status=CASE
		        WHEN ?='dead_banned' THEN 'banned'
		        WHEN ?='dead_normal' THEN 'expired'
		        ELSE status
		    END,
		    updated_at=?
		WHERE id=? AND archived_at IS NULL`,
		nullable(monitorAccountID), defaultString(monitorStatus, "unknown_monitor"),
		defaultString(monitorStatus, "unknown_monitor"), defaultString(monitorStatus, "unknown_monitor"), formatTime(now), accountID)
	if err != nil {
		return models.Account{}, err
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return models.Account{}, sql.ErrNoRows
	}
	return r.Account(ctx, accountID)
}

func (r *Repository) RetireAccount(ctx context.Context, accountID int64) (RetireAccountResult, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return RetireAccountResult{}, err
	}
	defer tx.Rollback()
	now := r.now().UTC()
	var exists int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM chatgpt_accounts WHERE id=? AND archived_at IS NULL`, accountID).Scan(&exists); err != nil {
		return RetireAccountResult{}, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT a.id,a.card_id,a.allocation_state,a.valid_until,c.status,c.expires_at
		FROM allocations a
		JOIN cards c ON c.id=a.card_id
		WHERE a.account_id=? AND a.active=1 AND a.allocation_state IN ('primary','grace')
		ORDER BY a.id ASC`, accountID)
	if err != nil {
		return RetireAccountResult{}, err
	}
	allocations := make([]retireAllocation, 0)
	for rows.Next() {
		var item retireAllocation
		var validUntilRaw string
		var cardExpiresRaw sql.NullString
		if err := rows.Scan(&item.id, &item.cardID, &item.state, &validUntilRaw, &item.cardStatus, &cardExpiresRaw); err != nil {
			rows.Close()
			return RetireAccountResult{}, err
		}
		item.validUntil, err = parseTime(validUntilRaw)
		if err != nil {
			rows.Close()
			return RetireAccountResult{}, err
		}
		if cardExpiresRaw.Valid {
			expiresAt, parseErr := parseTime(cardExpiresRaw.String)
			if parseErr != nil {
				rows.Close()
				return RetireAccountResult{}, parseErr
			}
			item.cardExpiresAt = &expiresAt
		}
		allocations = append(allocations, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return RetireAccountResult{}, err
	}
	rows.Close()

	result := RetireAccountResult{Archived: true}
	for _, item := range allocations {
		effectivePrimary := item.state == "primary" && item.cardStatus == "redeemed" && item.cardExpiresAt != nil && item.cardExpiresAt.After(now) && item.validUntil.After(now)
		if !effectivePrimary {
			terminalState := "expired"
			if item.state == "grace" {
				terminalState = "replaced"
			} else if item.cardStatus == "revoked" {
				terminalState = "revoked"
			}
			if _, err := tx.ExecContext(ctx, `UPDATE allocations
				SET allocation_state=?,active=0,replaced_at=?,replacement_reason='account_retired',updated_at=?
				WHERE id=? AND active=1`, terminalState, formatTime(now), formatTime(now), item.id); err != nil {
				return RetireAccountResult{}, err
			}
			result.ClosedAllocations++
			continue
		}

		newAccountID, err := selectCandidateAccountExcluding(ctx, tx, now, *item.cardExpiresAt, true, accountID)
		if errors.Is(err, ErrNoAccountCapacity) {
			return RetireAccountResult{}, ErrAccountReplacementUnavailable
		}
		if err != nil {
			return RetireAccountResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE allocations
			SET allocation_state='replaced',active=0,replaced_at=?,replacement_reason='account_retired',updated_at=?
			WHERE id=? AND active=1 AND allocation_state='primary'`, formatTime(now), formatTime(now), item.id); err != nil {
			return RetireAccountResult{}, err
		}
		if err := reserveAccountCapacity(ctx, tx, newAccountID, now); errors.Is(err, ErrNoAccountCapacity) {
			return RetireAccountResult{}, ErrAccountReplacementUnavailable
		} else if err != nil {
			return RetireAccountResult{}, err
		}
		insert, err := tx.ExecContext(ctx, `INSERT INTO allocations
			(card_id,account_id,allocated_at,valid_until,allocation_state,active,created_at,updated_at)
			VALUES (?,?,?,?,'primary',1,?,?)`, item.cardID, newAccountID, formatTime(now), formatTime(item.validUntil), formatTime(now), formatTime(now))
		if err != nil {
			return RetireAccountResult{}, err
		}
		newAllocationID, err := insert.LastInsertId()
		if err != nil {
			return RetireAccountResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE allocations SET superseded_by_allocation_id=?,updated_at=? WHERE id=?`, newAllocationID, formatTime(now), item.id); err != nil {
			return RetireAccountResult{}, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO replacement_history
			(card_id,old_account_id,new_account_id,reason,detected_at,replaced_at,grace_until,operator,created_at)
			VALUES (?,?,?,'account_retired',?,?,NULL,'admin',?)`, item.cardID, accountID, newAccountID, formatTime(now), formatTime(now), formatTime(now)); err != nil {
			return RetireAccountResult{}, err
		}
		result.ReplacedAllocations++
	}

	archive, err := tx.ExecContext(ctx, `UPDATE chatgpt_accounts
		SET archived_at=?,status='disabled',current_allocations=0,
			display_password_secret=x'',display_password_key_id='',
			display_2fa_secret=x'',display_2fa_key_id='',
			source_url_secret=NULL,source_url_key_id=NULL,updated_at=?
		WHERE id=? AND archived_at IS NULL`, formatTime(now), formatTime(now), accountID)
	if err != nil {
		return RetireAccountResult{}, err
	}
	if affected, _ := archive.RowsAffected(); affected != 1 {
		return RetireAccountResult{}, sql.ErrNoRows
	}
	if err := auditWithTx(ctx, tx, r.now, "account.retired", "account", accountID, map[string]any{
		"replaced_allocations": result.ReplacedAllocations,
		"closed_allocations":   result.ClosedAllocations,
	}); err != nil {
		return RetireAccountResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return RetireAccountResult{}, err
	}
	return result, nil
}

func (r *Repository) CreateMonitorSyncRun(ctx context.Context, total int) (int64, error) {
	now := r.now().UTC()
	result, err := r.db.ExecContext(ctx, `INSERT INTO monitor_sync_runs(state, started_at, accounts_total)
		VALUES ('running', ?, ?)`, formatTime(now), total)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func (r *Repository) FinishMonitorSyncRun(ctx context.Context, runID int64, state string, okCount, failedCount int, errorCode string) error {
	if state != "completed" && state != "failed" {
		return fmt.Errorf("invalid monitor sync state %q", state)
	}
	now := r.now().UTC()
	result, err := r.db.ExecContext(ctx, `UPDATE monitor_sync_runs
		SET state=?, finished_at=?, accounts_ok=?, accounts_failed=?, error_code=?
		WHERE id=?`,
		state, formatTime(now), okCount, failedCount, nullable(errorCode), runID)
	if err != nil {
		return err
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func (r *Repository) LatestMonitorSyncRun(ctx context.Context) (MonitorSyncRun, error) {
	var run MonitorSyncRun
	var started, finished sql.NullString
	var errorCode sql.NullString
	if err := r.db.QueryRowContext(ctx, `SELECT id,state,started_at,finished_at,accounts_total,accounts_ok,accounts_failed,error_code
		FROM monitor_sync_runs ORDER BY started_at DESC, id DESC LIMIT 1`).Scan(
		&run.ID, &run.State, &started, &finished, &run.AccountsTotal, &run.AccountsOK, &run.AccountsFailed, &errorCode,
	); err != nil {
		return MonitorSyncRun{}, err
	}
	parsedStarted, err := parseTime(started.String)
	if err != nil {
		return MonitorSyncRun{}, err
	}
	run.StartedAt = parsedStarted
	if finished.Valid {
		parsedFinished, err := parseTime(finished.String)
		if err != nil {
			return MonitorSyncRun{}, err
		}
		run.FinishedAt = &parsedFinished
	}
	if errorCode.Valid {
		run.ErrorCode = errorCode.String
	}
	return run, nil
}

func (r *Repository) EncryptedCredentials(ctx context.Context, accountID int64) (passwordKeyID string, passwordCiphertext []byte, totpKeyID string, totpCiphertext []byte, err error) {
	err = r.db.QueryRowContext(ctx, `SELECT display_password_key_id,display_password_secret,display_2fa_key_id,display_2fa_secret FROM chatgpt_accounts WHERE id=?`, accountID).
		Scan(&passwordKeyID, &passwordCiphertext, &totpKeyID, &totpCiphertext)
	return
}

func (r *Repository) Credentials(ctx context.Context, accountID int64) (AccountCredentials, error) {
	if r.credentials == nil {
		return AccountCredentials{}, credential.ErrInvalidKeyring
	}
	var passwordKeyID, totpKeyID string
	var passwordCiphertext, totpCiphertext []byte
	if err := r.db.QueryRowContext(ctx, `SELECT display_password_key_id,display_password_secret,display_2fa_key_id,display_2fa_secret
		FROM chatgpt_accounts WHERE id=? AND archived_at IS NULL`, accountID).
		Scan(&passwordKeyID, &passwordCiphertext, &totpKeyID, &totpCiphertext); err != nil {
		return AccountCredentials{}, err
	}
	if strings.TrimSpace(passwordKeyID) == "" || len(passwordCiphertext) == 0 || strings.TrimSpace(totpKeyID) == "" || len(totpCiphertext) == 0 {
		return AccountCredentials{}, ErrAccountCredentialsUnavailable
	}
	password, err := r.credentials.Open(accountID, credential.CredentialPassword, passwordKeyID, passwordCiphertext)
	if err != nil {
		return AccountCredentials{}, err
	}
	totpSecret, err := r.credentials.Open(accountID, credential.CredentialTOTP, totpKeyID, totpCiphertext)
	if err != nil {
		return AccountCredentials{}, err
	}
	return AccountCredentials{Password: string(password), TOTPSecret: string(totpSecret)}, nil
}

func (r *Repository) ReencryptCredentials(ctx context.Context) (int64, error) {
	if r.credentials == nil {
		return 0, credential.ErrInvalidKeyring
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,display_password_key_id,display_password_secret,display_2fa_key_id,display_2fa_secret,source_url_key_id,source_url_secret FROM chatgpt_accounts WHERE archived_at IS NULL ORDER BY id ASC`)
	if err != nil {
		return 0, err
	}
	type row struct {
		id                 int64
		passwordKeyID      string
		passwordCiphertext []byte
		totpKeyID          string
		totpCiphertext     []byte
		sourceKeyID        sql.NullString
		sourceCiphertext   []byte
	}
	var accounts []row
	for rows.Next() {
		var item row
		if err := rows.Scan(&item.id, &item.passwordKeyID, &item.passwordCiphertext, &item.totpKeyID, &item.totpCiphertext, &item.sourceKeyID, &item.sourceCiphertext); err != nil {
			rows.Close()
			return 0, err
		}
		accounts = append(accounts, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	now := r.now().UTC()
	for _, item := range accounts {
		password, err := r.credentials.Open(item.id, credential.CredentialPassword, item.passwordKeyID, item.passwordCiphertext)
		if err != nil {
			return 0, err
		}
		totp, err := r.credentials.Open(item.id, credential.CredentialTOTP, item.totpKeyID, item.totpCiphertext)
		if err != nil {
			return 0, err
		}
		resealedPassword, err := r.credentials.Seal(item.id, credential.CredentialPassword, password)
		if err != nil {
			return 0, err
		}
		resealedTOTP, err := r.credentials.Seal(item.id, credential.CredentialTOTP, totp)
		if err != nil {
			return 0, err
		}
		var sourceKeyID any
		var sourceCiphertext any
		if item.sourceKeyID.Valid || len(item.sourceCiphertext) > 0 {
			if !item.sourceKeyID.Valid || len(item.sourceCiphertext) == 0 {
				return 0, credential.ErrDecryptCredential
			}
			source, err := r.credentials.Open(item.id, credential.CredentialSourceURL, item.sourceKeyID.String, item.sourceCiphertext)
			if err != nil {
				return 0, err
			}
			resealedSource, err := r.credentials.Seal(item.id, credential.CredentialSourceURL, source)
			if err != nil {
				return 0, err
			}
			sourceKeyID = resealedSource.KeyID
			sourceCiphertext = resealedSource.Ciphertext
		}
		if _, err := tx.ExecContext(ctx, `UPDATE chatgpt_accounts
			SET display_password_secret=?, display_password_key_id=?, display_2fa_secret=?, display_2fa_key_id=?,
			    source_url_key_id=?, source_url_secret=?, updated_at=?
			WHERE id=?`,
			resealedPassword.Ciphertext, resealedPassword.KeyID, resealedTOTP.Ciphertext, resealedTOTP.KeyID,
			sourceKeyID, sourceCiphertext, formatTime(now), item.id); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int64(len(accounts)), nil
}

func (r *Repository) UserAllocationByCodeHash(ctx context.Context, codeHash []byte, now time.Time) (UserAllocationView, error) {
	views, err := r.UserAllocationsByCodeHash(ctx, codeHash, now)
	if err != nil {
		return UserAllocationView{}, err
	}
	if len(views) == 0 {
		return UserAllocationView{}, sql.ErrNoRows
	}
	return views[0], nil
}

func (r *Repository) UserAllocationsByCodeHash(ctx context.Context, codeHash []byte, now time.Time) ([]UserAllocationView, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT
			c.id,c.code_hash,c.code_suffix,c.duration_days,c.status,c.redeemed_at,c.expires_at,c.revoked_at,c.created_at,c.updated_at,
			a.id,a.card_id,a.account_id,a.allocated_at,a.valid_until,a.grace_until,a.allocation_state,a.active,a.superseded_by_allocation_id
		FROM cards c JOIN allocations a ON a.card_id=c.id
		WHERE c.code_hash=? AND c.status='redeemed'
		  AND a.active=1 AND a.allocation_state IN ('primary','grace')
		  AND datetime(a.valid_until) > datetime(?)
		  AND (a.grace_until IS NULL OR datetime(a.grace_until) > datetime(?))
		ORDER BY CASE a.allocation_state WHEN 'primary' THEN 0 ELSE 1 END, a.id ASC`,
		codeHash, formatTime(now.UTC()), formatTime(now.UTC()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var views []UserAllocationView
	for rows.Next() {
		view, err := scanUserAllocationBase(rows)
		if err != nil {
			return nil, err
		}
		views = append(views, view)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(views) == 0 {
		return nil, sql.ErrNoRows
	}
	rows.Close()
	for index := range views {
		account, err := r.Account(ctx, views[index].Allocation.AccountID)
		if err != nil {
			return nil, err
		}
		credentials, err := r.Credentials(ctx, views[index].Allocation.AccountID)
		if err != nil {
			return nil, err
		}
		views[index].Account = account
		views[index].Credentials = credentials
	}
	return views, nil
}

func scanUserAllocationBase(scanner accountScanner) (UserAllocationView, error) {
	var card models.Card
	var allocation models.Allocation
	var accountID int64
	var redeemedAt, expiresAt, revokedAt, allocatedAt, validUntil, graceUntil sql.NullString
	var supersededBy sql.NullInt64
	var createdAt, updatedAt string
	if err := scanner.Scan(&card.ID, &card.CodeHash, &card.CodeSuffix, &card.DurationDays, &card.Status, &redeemedAt, &expiresAt, &revokedAt, &createdAt, &updatedAt,
		&allocation.ID, &allocation.CardID, &accountID, &allocatedAt, &validUntil, &graceUntil, &allocation.AllocationState, &allocation.Active, &supersededBy); err != nil {
		return UserAllocationView{}, err
	}
	if err := hydrateCardTimes(&card, redeemedAt, expiresAt, revokedAt, createdAt, updatedAt); err != nil {
		return UserAllocationView{}, err
	}
	allocation.AccountID = accountID
	if supersededBy.Valid {
		value := supersededBy.Int64
		allocation.SupersededByAllocationID = &value
	}
	parsedAllocatedAt, err := parseTime(allocatedAt.String)
	if err != nil {
		return UserAllocationView{}, err
	}
	parsedValidUntil, err := parseTime(validUntil.String)
	if err != nil {
		return UserAllocationView{}, err
	}
	allocation.AllocatedAt = parsedAllocatedAt
	allocation.ValidUntil = parsedValidUntil
	if graceUntil.Valid {
		parsed, err := parseTime(graceUntil.String)
		if err != nil {
			return UserAllocationView{}, err
		}
		allocation.GraceUntil = &parsed
	}
	return UserAllocationView{Allocation: allocation, Card: card}, nil
}

func (r *Repository) QueryFailures(ctx context.Context, subjectHash []byte, now time.Time) (int, error) {
	var count int
	err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM rate_limit_events
		WHERE scope='user_query_failure' AND subject_hash=? AND expires_at>?`, subjectHash, formatTime(now.UTC())).Scan(&count)
	return count, err
}

func (r *Repository) RecordQueryFailure(ctx context.Context, subjectHash []byte, now time.Time) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO rate_limit_events(scope,subject_hash,event_type,occurred_at,expires_at)
		VALUES ('user_query_failure',?,'failure',?,?)`, subjectHash, formatTime(now.UTC()), formatTime(now.UTC().Add(15*time.Minute)))
	return err
}

func (r *Repository) ResetQueryFailures(ctx context.Context, subjectHash []byte) error {
	_, err := r.db.ExecContext(ctx, "DELETE FROM rate_limit_events WHERE scope='user_query_failure' AND subject_hash=?", subjectHash)
	return err
}

func (r *Repository) CreateCaptcha(ctx context.Context, subjectHash []byte, answer string, now time.Time) (CaptchaChallenge, error) {
	left, right, err := randomDigits()
	if err != nil {
		return CaptchaChallenge{}, err
	}
	question := fmt.Sprintf("%d + %d", left, right)
	if answer == "" {
		answer = fmt.Sprintf("%d", left+right)
	}
	answerHash := captchaAnswerHash(subjectHash, answer)
	challengeHash := captchaChallengeHash(subjectHash, question, now)
	expiresAt := now.UTC().Add(2 * time.Minute)
	result, err := r.db.ExecContext(ctx, `INSERT INTO captcha_challenges(challenge_hash,answer_hash,expires_at,created_at)
		VALUES (?,?,?,?)`, challengeHash, answerHash, formatTime(expiresAt), formatTime(now.UTC()))
	if err != nil {
		return CaptchaChallenge{}, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return CaptchaChallenge{}, err
	}
	return CaptchaChallenge{ID: id, Question: question, ExpiresAt: expiresAt, Required: true}, nil
}

func (r *Repository) VerifyCaptcha(ctx context.Context, subjectHash []byte, challengeID int64, answer string, now time.Time) error {
	var answerHash []byte
	var expiresRaw string
	var verified sql.NullString
	if err := r.db.QueryRowContext(ctx, `SELECT answer_hash,expires_at,verified_at FROM captcha_challenges WHERE id=?`, challengeID).Scan(&answerHash, &expiresRaw, &verified); err != nil {
		return ErrCaptchaInvalid
	}
	expiresAt, err := parseTime(expiresRaw)
	if err != nil || !expiresAt.After(now.UTC()) || verified.Valid {
		return ErrCaptchaInvalid
	}
	expected := captchaAnswerHash(subjectHash, answer)
	if subtleCompare(answerHash, expected) != 1 {
		return ErrCaptchaInvalid
	}
	_, err = r.db.ExecContext(ctx, "UPDATE captcha_challenges SET verified_at=? WHERE id=? AND verified_at IS NULL", formatTime(now.UTC()), challengeID)
	return err
}

func selectCandidateAccount(ctx context.Context, tx *sql.Tx, now, cardExpiresAt time.Time, monitorAvailable bool) (int64, error) {
	return selectCandidateAccountExcluding(ctx, tx, now, cardExpiresAt, monitorAvailable, 0)
}

func selectCandidateAccountExcluding(ctx context.Context, tx *sql.Tx, now, cardExpiresAt time.Time, monitorAvailable bool, excludedAccountID int64) (int64, error) {
	row := tx.QueryRowContext(ctx, `SELECT id FROM chatgpt_accounts
		WHERE datetime(account_expiry) > datetime(?)
		  AND archived_at IS NULL
		  AND status='available'
		  AND current_allocations < max_concurrent_users
		  AND monitor_status != 'dead_banned'
		  AND (? = 0 OR id != ?)
		ORDER BY
		  CASE WHEN ? = 0 THEN 1 ELSE CASE monitor_status WHEN 'alive' THEN 0 WHEN 'unknown' THEN 1 WHEN 'unknown_monitor' THEN 1 WHEN 'dead_normal' THEN 2 ELSE 3 END END ASC,
		  max(0, (julianday(account_expiry) - julianday(?))) ASC,
		  abs((julianday(account_expiry) - julianday(?)) * 86400) ASC,
		  current_allocations ASC,
		  last_allocated_at IS NOT NULL ASC,
		  last_allocated_at ASC,
		  id ASC
		LIMIT 1`, formatTime(now), excludedAccountID, excludedAccountID, boolInt(monitorAvailable), formatTime(cardExpiresAt), formatTime(cardExpiresAt))
	var accountID int64
	if err := row.Scan(&accountID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNoAccountCapacity
		}
		return 0, err
	}
	return accountID, nil
}

func expireGraceAllocations(ctx context.Context, tx *sql.Tx, now time.Time) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT account_id,count(*)
		FROM allocations
		WHERE active=1 AND allocation_state='grace' AND datetime(grace_until) <= datetime(?)
		GROUP BY account_id`, formatTime(now))
	if err != nil {
		return 0, err
	}
	releases := map[int64]int{}
	total := 0
	for rows.Next() {
		var accountID int64
		var count int
		if err := rows.Scan(&accountID, &count); err != nil {
			rows.Close()
			return 0, err
		}
		releases[accountID] = count
		total += count
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	rows.Close()
	for accountID, count := range releases {
		if err := releaseAccountCapacity(ctx, tx, accountID, count, now); err != nil {
			return 0, err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE allocations
		SET allocation_state='replaced', active=0, replaced_at=?, replacement_reason='grace_expired', updated_at=?
		WHERE active=1 AND allocation_state='grace' AND datetime(grace_until) <= datetime(?)`,
		formatTime(now), formatTime(now), formatTime(now)); err != nil {
		return 0, err
	}
	return total, nil
}

func replaceOneAllocation(ctx context.Context, tx *sql.Tx, now time.Time, item replacementDueAllocation) (ReplacementResult, error) {
	newAccountID, err := selectCandidateAccountExcluding(ctx, tx, now, item.cardExpiresAt, true, item.oldAccountID)
	if err != nil {
		return ReplacementResult{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE allocations
		SET allocation_state='replaced', active=0, replaced_at=?, replacement_reason=?, updated_at=?
		WHERE id=? AND active=1 AND allocation_state='primary'`,
		formatTime(now), item.reason, formatTime(now), item.allocationID); err != nil {
		return ReplacementResult{}, err
	}
	if err := reserveAccountCapacity(ctx, tx, newAccountID, now); err != nil {
		return ReplacementResult{}, err
	}
	insert, err := tx.ExecContext(ctx, `INSERT INTO allocations
		(card_id,account_id,allocated_at,valid_until,allocation_state,active,created_at,updated_at)
		VALUES (?,?,?,?,'primary',1,?,?)`,
		item.cardID, newAccountID, formatTime(now), formatTime(item.cardExpiresAt), formatTime(now), formatTime(now))
	if err != nil {
		return ReplacementResult{}, err
	}
	newAllocationID, err := insert.LastInsertId()
	if err != nil {
		return ReplacementResult{}, err
	}
	result := ReplacementResult{
		CardID:          item.cardID,
		OldAllocationID: item.allocationID,
		NewAllocationID: newAllocationID,
		OldAccountID:    item.oldAccountID,
		NewAccountID:    newAccountID,
		Reason:          item.reason,
	}
	if item.reason == "banned" {
		if err := releaseAccountCapacity(ctx, tx, item.oldAccountID, 1, now); err != nil {
			return ReplacementResult{}, err
		}
	} else {
		graceUntil := now.Add(24 * time.Hour)
		result.GraceUntil = &graceUntil
		if _, err := tx.ExecContext(ctx, `UPDATE allocations
			SET allocation_state='grace', active=1, replaced_at=NULL, grace_until=?, replacement_reason=?, superseded_by_allocation_id=?, updated_at=?
			WHERE id=? AND active=0 AND allocation_state='replaced'`,
			formatTime(graceUntil), item.reason, newAllocationID, formatTime(now), item.allocationID); err != nil {
			return ReplacementResult{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO replacement_history
		(card_id,old_account_id,new_account_id,reason,detected_at,replaced_at,grace_until,operator,created_at)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		item.cardID, item.oldAccountID, newAccountID, item.reason, formatTime(now), formatTime(now), nullableTime(result.GraceUntil), "system", formatTime(now)); err != nil {
		return ReplacementResult{}, err
	}
	if err := auditWithTx(ctx, tx, nil, "replacement.completed", "card", item.cardID, map[string]any{"reason": item.reason, "old_account_id": item.oldAccountID, "new_account_id": newAccountID}); err != nil {
		return ReplacementResult{}, err
	}
	return result, nil
}

func reserveAccountCapacity(ctx context.Context, tx *sql.Tx, accountID int64, now time.Time) error {
	result, err := tx.ExecContext(ctx, `UPDATE chatgpt_accounts
		SET current_allocations=current_allocations+1,last_allocated_at=?,updated_at=?,
		    status=CASE WHEN current_allocations+1 >= max_concurrent_users THEN 'full' ELSE status END
		WHERE id=? AND archived_at IS NULL AND current_allocations < max_concurrent_users AND datetime(account_expiry) > datetime(?)`,
		formatTime(now), formatTime(now), accountID, formatTime(now))
	if err != nil {
		return err
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return ErrNoAccountCapacity
	}
	return nil
}

func releaseAccountCapacity(ctx context.Context, tx *sql.Tx, accountID int64, count int, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE chatgpt_accounts
		SET current_allocations=max(0,current_allocations-?),
		    status=CASE WHEN status='full' THEN 'available' ELSE status END,
		    updated_at=?
		WHERE id=?`, count, formatTime(now), accountID)
	return err
}

func auditWithTx(ctx context.Context, tx *sql.Tx, nowFunc func() time.Time, action, targetType string, targetID int64, metadata map[string]any) error {
	encoded := "{}"
	if len(metadata) > 0 {
		var parts []string
		for key, value := range metadata {
			parts = append(parts, fmt.Sprintf("%q:%q", key, fmt.Sprint(value)))
		}
		encoded = "{" + strings.Join(parts, ",") + "}"
	}
	stamp := time.Now().UTC()
	if nowFunc != nil {
		stamp = nowFunc().UTC()
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO audit_events(actor_type,action,target_type,target_id,metadata_json,created_at)
		VALUES ('system',?,?,?,?,?)`, action, targetType, nullableInt(targetID), encoded, formatTime(stamp))
	return err
}

type accountScanner interface {
	Scan(dest ...any) error
}

type cardScanner interface {
	Scan(dest ...any) error
}

func scanCard(scanner cardScanner) (models.Card, error) {
	var card models.Card
	var redeemed, expires, revoked, created, updated sql.NullString
	if err := scanner.Scan(&card.ID, &card.CodeHash, &card.CodeSuffix, &card.DurationDays, &card.Status, &redeemed, &expires, &revoked, &created, &updated, &card.PlaintextAvailable); err != nil {
		return models.Card{}, err
	}
	if err := hydrateCardTimes(&card, redeemed, expires, revoked, created.String, updated.String); err != nil {
		return models.Card{}, err
	}
	return card, nil
}

func hydrateCardTimes(card *models.Card, redeemed, expires, revoked sql.NullString, created, updated string) error {
	if redeemed.Valid {
		parsed, err := parseTime(redeemed.String)
		if err != nil {
			return err
		}
		card.RedeemedAt = &parsed
	}
	if expires.Valid {
		parsed, err := parseTime(expires.String)
		if err != nil {
			return err
		}
		card.ExpiresAt = &parsed
	}
	if revoked.Valid {
		parsed, err := parseTime(revoked.String)
		if err != nil {
			return err
		}
		card.RevokedAt = &parsed
	}
	parsedCreated, err := parseTime(created)
	if err != nil {
		return err
	}
	parsedUpdated, err := parseTime(updated)
	if err != nil {
		return err
	}
	card.CreatedAt = parsedCreated
	card.UpdatedAt = parsedUpdated
	return nil
}

func (r *Repository) scanAccount(scanner accountScanner) (models.Account, error) {
	var account models.Account
	var expiry, lastAllocated sql.NullString
	var monitorAccount sql.NullString
	var sourceKeyID sql.NullString
	var sourceCiphertext []byte
	if err := scanner.Scan(&account.ID, &account.DisplayUsername, &expiry, &account.MaxConcurrentUsers, &account.CurrentAllocations, &monitorAccount, &account.MonitorStatus, &account.Status, &lastAllocated, &sourceKeyID, &sourceCiphertext); err != nil {
		return models.Account{}, err
	}
	parsed, err := parseTime(expiry.String)
	if err != nil {
		return models.Account{}, err
	}
	account.AccountExpiry = parsed
	if monitorAccount.Valid {
		account.MonitorAccountID = monitorAccount.String
	}
	if lastAllocated.Valid {
		allocated, err := parseTime(lastAllocated.String)
		if err != nil {
			return models.Account{}, err
		}
		account.LastAllocatedAt = &allocated
	}
	if sourceKeyID.Valid || len(sourceCiphertext) > 0 {
		if !sourceKeyID.Valid || len(sourceCiphertext) == 0 || r.credentials == nil {
			return models.Account{}, credential.ErrDecryptCredential
		}
		sourceURL, err := r.credentials.Open(account.ID, credential.CredentialSourceURL, sourceKeyID.String, sourceCiphertext)
		if err != nil {
			return models.Account{}, err
		}
		account.SourceURL = string(sourceURL)
	}
	return account, nil
}

func validateAccountExpiry(now, expiry time.Time) error {
	if expiry.IsZero() || expiry.Before(now) {
		return ErrAccountExpiryTooLong
	}
	return nil
}

func validAccountCapacity(value int) bool {
	return value >= 1 && value <= MaxAccountCapacity
}

func nullable(value string) any {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	return value
}

func nullableInt(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return formatTime(*value)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func intCeil(value float64) int {
	whole := int(value)
	if value > float64(whole) {
		return whole + 1
	}
	return whole
}

func inventoryWarning(available int, days *float64) (string, string, string) {
	if available == 0 {
		return "exhausted", "耗尽", "0"
	}
	if days == nil {
		return "safe", "安全", "∞"
	}
	switch {
	case *days > 15:
		return "safe", "安全", fmt.Sprintf("%.1f", *days)
	case *days >= 7:
		return "notice", "注意", fmt.Sprintf("%.1f", *days)
	default:
		return "urgent", "紧急", fmt.Sprintf("%.1f", *days)
	}
}

func randomDigits() (int, int, error) {
	var values [2]byte
	if _, err := io.ReadFull(rand.Reader, values[:]); err != nil {
		return 0, 0, err
	}
	return int(values[0]%9) + 1, int(values[1]%9) + 1, nil
}

func captchaAnswerHash(subjectHash []byte, answer string) []byte {
	sum := sha256.Sum256([]byte("captcha-answer-v1:" + string(subjectHash) + ":" + strings.TrimSpace(answer)))
	return sum[:]
}

func captchaChallengeHash(subjectHash []byte, question string, now time.Time) []byte {
	sum := sha256.Sum256([]byte(fmt.Sprintf("captcha-challenge-v1:%x:%s:%s", subjectHash, question, formatTime(now.UTC()))))
	return sum[:]
}

func subtleCompare(a, b []byte) int {
	if len(a) != len(b) {
		return 0
	}
	return subtle.ConstantTimeCompare(a, b)
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func parseTime(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, value)
}
