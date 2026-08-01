package chatgpt

import (
	"fmt"
	"time"
)

type CredentialKind string

const (
	CredentialAccess  CredentialKind = "access"
	CredentialRefresh CredentialKind = "refresh"
	CredentialSession CredentialKind = "session"
	CredentialDevice  CredentialKind = "device"
	CredentialOAuth   CredentialKind = "oauth"
)

type Plan string

const (
	PlanFree    Plan = "free"
	PlanPlus    Plan = "plus"
	PlanTeam    Plan = "team"
	PlanUnknown Plan = "unknown"
)

type AccountState string

const (
	StateActive                   AccountState = "active"
	StateAccessExpiredRefreshable AccountState = "access_expired_refreshable"
	StateCredentialRevoked        AccountState = "credential_revoked"
	StateAccountDisabled          AccountState = "account_disabled"
	StatePermissionDenied         AccountState = "permission_or_scope_denied"
	StateRateLimited              AccountState = "rate_limited"
	StateUpstreamTransient        AccountState = "upstream_transient"
	StateContractChanged          AccountState = "contract_changed"
)

type ErrorKind string

const (
	ErrorCredentialRevoked ErrorKind = "credential_revoked"
	ErrorAccountDisabled   ErrorKind = "account_disabled"
	ErrorPermissionDenied  ErrorKind = "permission_or_scope_denied"
	ErrorRateLimited       ErrorKind = "rate_limited"
	ErrorUpstreamTransient ErrorKind = "upstream_transient"
	ErrorContractChanged   ErrorKind = "contract_changed"
	ErrorInput             ErrorKind = "invalid_input"
)

type EvidenceLevel string

const (
	EvidenceLiveVerified                EvidenceLevel = "live_verified"
	EvidenceContractVerifiedLivePending EvidenceLevel = "contract_verified_live_pending"
	EvidenceUnverified                  EvidenceLevel = "unverified"
)

type TypedError struct {
	Kind                  ErrorKind
	StatusCode            int
	EvidenceCode          string
	EvidenceLevel         EvidenceLevel
	Retryable             bool
	RetryAfter            time.Duration
	BannedCandidate       bool
	PreserveBusinessState bool
	Cause                 error
}

func (e *TypedError) Error() string {
	if e.EvidenceCode != "" {
		return fmt.Sprintf("chatgpt: %s (%s)", e.Kind, e.EvidenceCode)
	}
	return fmt.Sprintf("chatgpt: %s", e.Kind)
}

func (e *TypedError) Unwrap() error { return e.Cause }

// Error is retained as an alias for callers written against revision 5.
type Error = TypedError

type TokenSet struct {
	AccessToken     string
	RefreshToken    string
	IDToken         string
	AccessExpiresAt *time.Time
}

type StatusResult struct {
	ProviderAccountID  string
	Email              string
	RawPlan            string
	Plan               Plan
	SubscriptionExpiry *time.Time
	AccountState       AccountState
	EvidenceCode       string
	EvidenceLevel      EvidenceLevel
	ResponseHash       string
}

type DeviceAuthorization struct {
	DeviceAuthID string        `json:"device_auth_id"`
	UserCode     string        `json:"user_code"`
	VerifyURL    string        `json:"verify_url"`
	Interval     time.Duration `json:"interval"`
	ExpiresAt    time.Time     `json:"expires_at"`
}

type DevicePollState string

const (
	DevicePollPending    DevicePollState = "pending"
	DevicePollSlowDown   DevicePollState = "slow_down"
	DevicePollAuthorized DevicePollState = "authorized"
)

type DevicePollResult struct {
	State      DevicePollState
	Tokens     TokenSet
	RetryAfter time.Duration
}
