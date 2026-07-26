package models

import "time"

type Account struct {
	ID                 int64
	DisplayUsername    string
	AccountExpiry      time.Time
	MaxConcurrentUsers int
	CurrentAllocations int
	MonitorAccountID   string
	MonitorStatus      string
	Status             string
	LastAllocatedAt    *time.Time
}

type Card struct {
	ID                 int64
	CodeHash           []byte
	CodeSuffix         string
	DurationDays       int
	Status             string
	RedeemedAt         *time.Time
	ExpiresAt          *time.Time
	RevokedAt          *time.Time
	CreatedAt          time.Time
	UpdatedAt          time.Time
	PlaintextAvailable bool
}

type Allocation struct {
	ID                       int64
	CardID                   int64
	AccountID                int64
	AllocatedAt              time.Time
	ValidUntil               time.Time
	GraceUntil               *time.Time
	ReplacedAt               *time.Time
	ReplacementReason        string
	AllocationState          string
	Active                   bool
	SupersededByAllocationID *int64
}
