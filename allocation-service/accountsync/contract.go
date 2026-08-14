package accountsync

import (
	"context"
	"errors"
	"strings"
	"time"
)

const (
	EventAccountCreated      = "account.created"
	EventAccountUpdated      = "account.updated"
	EventAccountBanned       = "account.dead_banned"
	EventAccountReauthorized = "account.reauthorized"
)

// Event is the credential-free contract used between the monitor and allocation domains.
type Event struct {
	EventID            string    `json:"event_id"`
	Version            int64     `json:"version"`
	Type               string    `json:"event_type"`
	OccurredAt         time.Time `json:"occurred_at"`
	ProviderAccountID  string    `json:"provider_account_id"`
	Email              string    `json:"email,omitempty"`
	Plan               string    `json:"plan"`
	SubscriptionExpiry time.Time `json:"subscription_expiry"`
	Status             string    `json:"status"`
}

type Result struct {
	Disposition string `json:"disposition"`
	Created     bool   `json:"created"`
	Updated     bool   `json:"updated"`
	Migrated    int    `json:"migrated"`
	Pending     int    `json:"pending"`
}

type Sink interface {
	ApplyMonitorAccountEvent(context.Context, Event) (Result, error)
}

func (e Event) Validate() error {
	if strings.TrimSpace(e.EventID) == "" || len(e.EventID) > 128 || e.Version < 1 || e.OccurredAt.IsZero() ||
		strings.TrimSpace(e.ProviderAccountID) == "" || len(e.ProviderAccountID) > 512 || e.SubscriptionExpiry.IsZero() {
		return errors.New("invalid monitor account event")
	}
	switch e.Type {
	case EventAccountCreated, EventAccountUpdated, EventAccountBanned, EventAccountReauthorized:
	default:
		return errors.New("unsupported monitor account event type")
	}
	switch e.Status {
	case "alive", "dead_normal", "dead_banned":
	default:
		return errors.New("unsupported monitor account status")
	}
	switch e.Plan {
	case "free", "plus", "team", "unknown":
	default:
		return errors.New("unsupported monitor account plan")
	}
	if e.Type == EventAccountBanned && e.Status != "dead_banned" {
		return errors.New("banned event has incompatible status")
	}
	return nil
}
