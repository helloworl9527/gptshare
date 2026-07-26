package monitor

import (
	"context"
	"time"
)

// AlertEvent is the transport-neutral outbox contract produced by STEP-06.
// External channel adapters are deliberately not wired until STEP-07.
type AlertEvent struct {
	ID        int64
	AccountID int64
	EpochID   int64
	EventKey  string
	EventType string
	CreatedAt time.Time
}

type EventAdapter interface {
	Deliver(context.Context, AlertEvent) error
}
