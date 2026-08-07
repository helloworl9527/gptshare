package monitorfacade

import (
	"context"
	"errors"
	"fmt"
	"time"
)

var ErrUnavailable = errors.New("monitor facade unavailable")

type FaultKind string

const (
	FaultUnavailable     FaultKind = "unavailable"
	FaultTimeout         FaultKind = "timeout"
	FaultNotFound        FaultKind = "not_found"
	FaultContractChanged FaultKind = "contract_changed"
)

type Fault struct {
	Kind FaultKind
}

func (f *Fault) Error() string {
	if f == nil {
		return "monitor facade fault"
	}
	return fmt.Sprintf("monitor facade fault: %s", f.Kind)
}

func (f *Fault) Unwrap() error { return ErrUnavailable }

func NewFault(kind FaultKind) error { return &Fault{Kind: kind} }

func FaultKindOf(err error) (FaultKind, bool) {
	var fault *Fault
	if !errors.As(err, &fault) || fault == nil {
		return "", false
	}
	return fault.Kind, true
}

type ImportRequest struct {
	Token     string
	TokenType string
	Label     string
}

type ImportResult struct {
	MonitorAccountID string
	MonitorStatus    string
	Email            string
	AccountExpiry    time.Time
	Plan             string
	SyncErrorCode    string
}

type StatusResult struct {
	MonitorAccountID string
	MonitorStatus    string
	Email            string
	AccountExpiry    time.Time
	Plan             string
	SyncErrorCode    string
}

type Client interface {
	ImportForAllocation(context.Context, ImportRequest) (ImportResult, error)
	ListAccounts(context.Context) ([]StatusResult, error)
	Status(context.Context, string) (ImportResult, error)
	BatchStatus(context.Context, []string) (map[string]StatusResult, error)
	Available(context.Context) bool
}
