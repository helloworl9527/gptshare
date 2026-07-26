package notify

import (
	"context"
	"log/slog"
	"time"
)

// Event is deliberately transport-neutral so adding a channel never changes
// account detection or the monitoring transaction that produced the outbox row.
type Event struct {
	ID        int64
	AccountID int64
	EpochID   int64
	EventKey  string
	EventType string
	CreatedAt time.Time
}

type Notifier interface {
	Send(context.Context, Event) error
}

// ChannelAdapter is the reserved extension point for a future channel release.
// STEP-07 registers no network-backed implementations.
type ChannelAdapter interface {
	Name() string
	Send(context.Context, Event) error
}

type LogNotifier struct{ logger *slog.Logger }

func NewLogNotifier(logger *slog.Logger) *LogNotifier {
	if logger == nil {
		logger = slog.Default()
	}
	return &LogNotifier{logger: logger}
}

func (n *LogNotifier) Send(_ context.Context, event Event) error {
	n.logger.Info("alert recorded; notification channels are not connected",
		"event_id", event.ID,
		"event_key", event.EventKey,
		"event_type", event.EventType,
		"account_id", event.AccountID,
		"channel_state", "recorded_no_channel")
	return nil
}
