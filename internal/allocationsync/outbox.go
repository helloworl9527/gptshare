package allocationsync

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"allocation-service/accountsync"
)

const maxResponseBytes = 64 << 10

type PermanentError struct{ Code string }

func (e *PermanentError) Error() string { return e.Code }

func EnqueueAccountTx(ctx context.Context, tx *sql.Tx, accountID int64, eventType string, occurredAt time.Time) (accountsync.Event, error) {
	if tx == nil || accountID <= 0 {
		return accountsync.Event{}, errors.New("account outbox transaction is required")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE accounts SET sync_version=sync_version+1 WHERE id=? AND deleted_at IS NULL`, accountID); err != nil {
		return accountsync.Event{}, err
	}
	var event accountsync.Event
	var email sql.NullString
	var expiry string
	if err := tx.QueryRowContext(ctx, `SELECT sync_version,provider_account_id,email,plan,COALESCE(current_expiry,auth_expiry),status
		FROM accounts WHERE id=? AND deleted_at IS NULL`, accountID).Scan(
		&event.Version, &event.ProviderAccountID, &email, &event.Plan, &expiry, &event.Status,
	); err != nil {
		return accountsync.Event{}, err
	}
	eventID, err := newEventID()
	if err != nil {
		return accountsync.Event{}, err
	}
	event.EventID = eventID
	event.Type = eventType
	event.OccurredAt = occurredAt.UTC()
	event.Email = email.String
	parsedExpiry, err := time.Parse(time.RFC3339Nano, expiry)
	if err != nil {
		return accountsync.Event{}, err
	}
	event.SubscriptionExpiry = parsedExpiry.UTC()
	if event.Plan == accountsync.PlanFree {
		// free 账号没有订阅到期时间，上面的 COALESCE 会退到 auth_expiry（凭据到期），
		// 分配域会据此以为订阅还有效、继续往里分配顾客。降级即订阅当场终止，
		// 按观测时刻发出：分配域的到期判定、换号扫描和自动下线都会立刻生效。
		event.SubscriptionExpiry = event.OccurredAt
	}
	if err := event.Validate(); err != nil {
		return accountsync.Event{}, err
	}
	payload, err := json.Marshal(event)
	if err != nil {
		return accountsync.Event{}, err
	}
	stamp := event.OccurredAt.Format(time.RFC3339Nano)
	_, err = tx.ExecContext(ctx, `INSERT INTO allocation_account_outbox
		(event_id,account_id,account_version,event_type,payload_json,delivery_status,next_attempt_at,created_at,updated_at)
		VALUES (?,?,?,?,?,'pending',?,?,?)`, event.EventID, accountID, event.Version, event.Type, payload, stamp, stamp, stamp)
	return event, err
}

func newEventID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

type Consumer struct {
	db     *sql.DB
	sink   accountsync.Sink
	logger *slog.Logger
	now    func() time.Time
}

func NewConsumer(db *sql.DB, sink accountsync.Sink, logger *slog.Logger) (*Consumer, error) {
	if db == nil || sink == nil {
		return nil, errors.New("allocation outbox dependencies are required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Consumer{db: db, sink: sink, logger: logger, now: time.Now}, nil
}

func (c *Consumer) Run(ctx context.Context) error {
	stamp := c.now().UTC().Format(time.RFC3339Nano)
	if _, err := c.db.ExecContext(ctx, `UPDATE allocation_account_outbox SET delivery_status='retrying',next_attempt_at=?,updated_at=? WHERE delivery_status='processing'`, stamp, stamp); err != nil {
		return err
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if err := c.deliverOne(ctx); err != nil && !errors.Is(err, sql.ErrNoRows) && !errors.Is(err, context.Canceled) {
			c.logger.Error("allocation account event delivery failed", "error_code", "account_sync_delivery_failed")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c *Consumer) deliverOne(ctx context.Context) error {
	now := c.now().UTC()
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var eventID string
	var payload []byte
	var attempts int
	err = tx.QueryRowContext(ctx, `SELECT event_id,payload_json,attempts FROM allocation_account_outbox
		WHERE delivery_status IN ('pending','retrying') AND (next_attempt_at IS NULL OR next_attempt_at<=?)
		ORDER BY created_at,event_id LIMIT 1`, now.Format(time.RFC3339Nano)).Scan(&eventID, &payload, &attempts)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE allocation_account_outbox SET delivery_status='processing',attempts=attempts+1,updated_at=?
		WHERE event_id=? AND delivery_status IN ('pending','retrying')`, now.Format(time.RFC3339Nano), eventID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	var event accountsync.Event
	if err := json.Unmarshal(payload, &event); err != nil {
		return c.fail(ctx, eventID, "payload_invalid")
	}
	_, applyErr := c.sink.ApplyMonitorAccountEvent(ctx, event)
	if applyErr == nil {
		_, err = c.db.ExecContext(ctx, `UPDATE allocation_account_outbox SET delivery_status='delivered',next_attempt_at=NULL,last_error_code=NULL,delivered_at=?,updated_at=? WHERE event_id=?`,
			now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), eventID)
		return err
	}
	var permanent *PermanentError
	if errors.As(applyErr, &permanent) {
		return c.fail(ctx, eventID, permanent.Code)
	}
	delay := retryDelay(attempts)
	_, err = c.db.ExecContext(ctx, `UPDATE allocation_account_outbox SET delivery_status='retrying',next_attempt_at=?,last_error_code='delivery_unavailable',updated_at=? WHERE event_id=?`,
		now.Add(delay).Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), eventID)
	return err
}

func (c *Consumer) fail(ctx context.Context, eventID, code string) error {
	_, err := c.db.ExecContext(ctx, `UPDATE allocation_account_outbox SET delivery_status='failed',next_attempt_at=NULL,last_error_code=?,updated_at=? WHERE event_id=?`,
		code, c.now().UTC().Format(time.RFC3339Nano), eventID)
	return err
}

func retryDelay(attempts int) time.Duration {
	delays := []time.Duration{time.Second, 5 * time.Second, 30 * time.Second, 2 * time.Minute, 5 * time.Minute}
	if attempts < 0 {
		attempts = 0
	}
	if attempts >= len(delays) {
		return delays[len(delays)-1]
	}
	return delays[attempts]
}

type HTTPSink struct {
	url    string
	apiKey string
	client *http.Client
}

func NewHTTPSink(url, apiKey string, client *http.Client) (*HTTPSink, error) {
	if !validEndpointURL(url) || len(apiKey) < 32 || strings.TrimSpace(apiKey) != apiKey || strings.ContainsAny(apiKey, " \t\r\n") {
		return nil, errors.New("allocation event endpoint and API key are required")
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	return &HTTPSink{url: url, apiKey: apiKey, client: client}, nil
}

func validEndpointURL(value string) bool {
	endpoint, err := url.Parse(value)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.Fragment != "" || endpoint.Path != "/api/internal/v1/monitor-account-events" {
		return false
	}
	if endpoint.Scheme == "https" {
		return true
	}
	host := endpoint.Hostname()
	ip := net.ParseIP(host)
	return endpoint.Scheme == "http" && (strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback()))
}

func (s *HTTPSink) ApplyMonitorAccountEvent(ctx context.Context, event accountsync.Event) (accountsync.Result, error) {
	body, err := json.Marshal(event)
	if err != nil {
		return accountsync.Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return accountsync.Result{}, err
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", "application/json")
	response, err := s.client.Do(req)
	if err != nil {
		return accountsync.Result{}, err
	}
	defer response.Body.Close()
	data, readErr := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if readErr != nil || len(data) > maxResponseBytes {
		return accountsync.Result{}, errors.New("allocation event response invalid")
	}
	if response.StatusCode >= 500 || response.StatusCode == http.StatusTooManyRequests {
		return accountsync.Result{}, fmt.Errorf("allocation event endpoint unavailable: %d", response.StatusCode)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return accountsync.Result{}, &PermanentError{Code: "endpoint_rejected_event"}
	}
	var result accountsync.Result
	if err := json.Unmarshal(data, &result); err != nil {
		return accountsync.Result{}, &PermanentError{Code: "endpoint_protocol_invalid"}
	}
	if result.Disposition != "applied" && result.Disposition != "duplicate" && result.Disposition != "stale" {
		return accountsync.Result{}, &PermanentError{Code: "endpoint_protocol_invalid"}
	}
	return result, nil
}
