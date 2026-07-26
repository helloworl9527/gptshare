package notify

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"time"
)

const (
	maxAttempts     = 3
	claimTimeout    = 5 * time.Minute
	consumerBackoff = 2 * time.Second
)

type Consumer struct {
	db       *sql.DB
	notifier Notifier
	now      func() time.Time
	wg       sync.WaitGroup
}

func NewConsumer(db *sql.DB, notifier Notifier) (*Consumer, error) {
	if db == nil || notifier == nil {
		return nil, errors.New("notification consumer dependencies are required")
	}
	return &Consumer{db: db, notifier: notifier, now: time.Now}, nil
}

// Run executes the outbox loop in the caller's goroutine so a process-level
// supervisor can own panic recovery and restart policy.
func (c *Consumer) Run(ctx context.Context) error {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		_, _ = c.ProcessOnce(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (c *Consumer) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() { c.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ProcessOnce claims and delivers at most one event. The claim transaction is
// independent from account state transitions, so adapter failure cannot roll
// back or rewrite monitoring state.
func (c *Consumer) ProcessOnce(ctx context.Context) (bool, error) {
	now := c.now().UTC()
	_, _ = c.db.ExecContext(ctx, `UPDATE alert_events
		SET delivery_status='failed',next_attempt_at=?,last_error_code='consumer_interrupted',claimed_at=NULL,updated_at=?
		WHERE delivery_status='processing' AND claimed_at<?`,
		now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), now.Add(-claimTimeout).Format(time.RFC3339Nano))

	event, claimed, err := c.claim(ctx, now)
	if err != nil || !claimed {
		return claimed, err
	}
	if err := c.notifier.Send(ctx, event); err != nil {
		code := errorCode(err)
		next := any(nil)
		if eventAttempt, readErr := c.attempts(ctx, event.ID); readErr == nil && eventAttempt < maxAttempts {
			next = now.Add(consumerBackoff * time.Duration(eventAttempt)).Format(time.RFC3339Nano)
		}
		_, updateErr := c.db.ExecContext(ctx, `UPDATE alert_events
			SET delivery_status='failed',next_attempt_at=?,claimed_at=NULL,last_error_code=?,updated_at=?
			WHERE id=? AND delivery_status='processing'`, next, code, now.Format(time.RFC3339Nano), event.ID)
		if updateErr != nil {
			return true, updateErr
		}
		return true, nil
	}
	result, err := c.db.ExecContext(ctx, `UPDATE alert_events
		SET delivery_status='recorded_no_channel',next_attempt_at=NULL,claimed_at=NULL,last_error_code=NULL,updated_at=?
		WHERE id=? AND delivery_status='processing'`, now.Format(time.RFC3339Nano), event.ID)
	if err != nil {
		return true, err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return true, errors.New("notification claim was lost")
	}
	return true, nil
}

func (c *Consumer) claim(ctx context.Context, now time.Time) (Event, bool, error) {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return Event{}, false, err
	}
	defer tx.Rollback()
	var event Event
	var created string
	err = tx.QueryRowContext(ctx, `SELECT id,account_id,COALESCE(epoch_id,0),event_key,event_type,created_at
		FROM alert_events WHERE
		(delivery_status='pending' OR (delivery_status='failed' AND attempts<?))
		AND (next_attempt_at IS NULL OR next_attempt_at<=?)
		ORDER BY id LIMIT 1`, maxAttempts, now.Format(time.RFC3339Nano)).Scan(
		&event.ID, &event.AccountID, &event.EpochID, &event.EventKey, &event.EventType, &created)
	if err == sql.ErrNoRows {
		return Event{}, false, nil
	}
	if err != nil {
		return Event{}, false, err
	}
	event.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	result, err := tx.ExecContext(ctx, `UPDATE alert_events SET
		delivery_status='processing',attempts=attempts+1,claimed_at=?,updated_at=?
		WHERE id=? AND (delivery_status='pending' OR delivery_status='failed')`,
		now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), event.ID)
	if err != nil {
		return Event{}, false, err
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		return Event{}, false, nil
	}
	if err := tx.Commit(); err != nil {
		return Event{}, false, err
	}
	return event, true, nil
}

func (c *Consumer) attempts(ctx context.Context, id int64) (int, error) {
	var attempts int
	err := c.db.QueryRowContext(ctx, "SELECT attempts FROM alert_events WHERE id=?", id).Scan(&attempts)
	return attempts, err
}

func errorCode(err error) string {
	value := strings.ToLower(err.Error())
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(value, "timeout") {
		return "adapter_timeout"
	}
	if errors.Is(err, context.Canceled) {
		return "adapter_cancelled"
	}
	return "adapter_failed"
}
