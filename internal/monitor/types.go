package monitor

import (
	"context"
	"time"

	"chatgpt-monitor/internal/chatgpt"
)

const (
	StateAlive      = "alive"
	StateDeadNormal = "dead_normal"
	StateDeadBanned = "dead_banned"

	CheckOK                      = "ok"
	CheckError                   = "error"
	CheckVerificationRequired    = "verification_required"
	CheckContractChanged         = "contract_changed"
	CheckReauthorizationRequired = "reauthorization_required"
)

type Client interface {
	ExchangeCredential(context.Context, chatgpt.CredentialKind, string) (chatgpt.TokenSet, error)
	FetchStatus(context.Context, string) (chatgpt.StatusResult, error)
}

type Cipher interface {
	ActiveKeyID() string
	Seal([]byte, []byte) ([]byte, error)
	Open([]byte, []byte) ([]byte, error)
}

type Config struct {
	Workers         int
	RequestTimeout  time.Duration
	MaxRetries      int
	MinInterval     time.Duration
	DefaultInterval time.Duration
	NearExpiryDays  int
	RefreshBefore   time.Duration
	ParserVersion   string
	ManualWait      time.Duration
}

func DefaultConfig() Config {
	return Config{
		Workers: 3, RequestTimeout: 15 * time.Second, MaxRetries: 2,
		MinInterval: 15 * time.Minute, DefaultInterval: time.Hour,
		NearExpiryDays: 3, RefreshBefore: 5 * time.Minute, ParserVersion: "status-v1", ManualWait: 20 * time.Second,
	}
}

type Run struct {
	ID              string         `json:"id"`
	State           string         `json:"state"`
	Trigger         string         `json:"trigger"`
	AccountID       *int64         `json:"account_id,omitempty"`
	StartedAt       time.Time      `json:"started_at"`
	FinishedAt      *time.Time     `json:"finished_at,omitempty"`
	AccountsTotal   int            `json:"accounts_total"`
	AccountsOK      int            `json:"accounts_ok"`
	AccountsFailed  int            `json:"accounts_failed"`
	AccountsSkipped int            `json:"accounts_skipped"`
	ErrorCounts     map[string]int `json:"error_counts"`
	ErrorCode       string         `json:"error_code,omitempty"`
}

type ConflictError struct{ RunID string }

func (e *ConflictError) Error() string { return "monitor: refresh already running" }

type NotFoundError struct{}

func (*NotFoundError) Error() string { return "monitor: record not found" }

type PausedError struct{}

func (*PausedError) Error() string { return "monitor: account polling is paused for evidence review" }

type ReauthorizationRequiredError struct{}

func (*ReauthorizationRequiredError) Error() string {
	return "monitor: account reauthorization is required"
}

type ReviewDecision string

const (
	ReviewConfirm ReviewDecision = "confirm"
	ReviewReject  ReviewDecision = "reject"
)

type ReviewRequest struct {
	Signature string
	Decision  ReviewDecision
	Reason    string
	Operator  string
}

type ReviewResult struct {
	Signature        string
	Decision         ReviewDecision
	AccountsAffected int
	AlertsCreated    int
	ReviewedAt       time.Time
}
