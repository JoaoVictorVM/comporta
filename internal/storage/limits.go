package storage

import (
	"context"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
)

// Reservation states and ledger transitions.
const (
	ReservationReserved = "reserved"
	ReservationSettled  = "settled"
	ReservationReleased = "released"

	TransitionAdmit   = "admit"
	TransitionSettle  = "settle"
	TransitionRelease = "release"
)

// LimitItem is one frozen reservation against a calendar budget window. Window
// bounds are UTC microseconds so the reference survives a restart unchanged.
type LimitItem struct {
	Scope       string `json:"scope"`
	Subject     string `json:"subject"`
	Anonymous   bool   `json:"anonymous"`
	Dimension   string `json:"dimension"`
	WindowStart int64  `json:"window_start"`
	WindowEnd   int64  `json:"window_end"`
	Timezone    string `json:"timezone"`
	Reserved    int64  `json:"reserved"`
	Settled     *int64 `json:"settled,omitempty"`
}

// LimitQuote is what the reservation was computed from. The generation is
// informative only and never identifies the reservation across restarts.
type LimitQuote struct {
	Operation       string          `json:"operation"`
	EstimatedTokens int64           `json:"estimated_tokens"`
	ReservedCost    config.Microusd `json:"reserved_cost_microusd"`
	Generation      int64           `json:"generation"`
	Degraded        bool            `json:"degraded,omitempty"`
}

// RateEvent is one admitted logical request in the sliding windows of its app
// and user.
type RateEvent struct {
	RequestID       string
	AppName         string
	Subject         string
	Anonymous       bool
	AdmittedAt      time.Time
	EstimatedTokens int64
	ActualTokens    *int64
	Active          bool
}

// Reservation is the durable handle of one attempt.
type Reservation struct {
	RequestID    string
	AttemptIndex int
	AppName      string
	Subject      string
	Anonymous    bool
	AdmittedAt   time.Time
	State        string
	Quote        LimitQuote
	Items        []LimitItem
	Rate         *RateEvent
}

// Settlement ends a reservation. Settled is aligned with the reservation items.
type Settlement struct {
	RequestID    string    `json:"request_id"`
	AttemptIndex int       `json:"attempt_index"`
	Transition   string    `json:"transition"`
	At           time.Time `json:"at"`
	Settled      []int64   `json:"settled"`
	RateTokens   *int64    `json:"rate_tokens,omitempty"`
}

// LimitDelta is a signed change to one budget bucket.
type LimitDelta struct {
	Scope       string `json:"scope"`
	Subject     string `json:"subject"`
	Anonymous   bool   `json:"anonymous"`
	Dimension   string `json:"dimension"`
	WindowStart int64  `json:"window_start"`
	WindowEnd   int64  `json:"window_end"`
	Spent       int64  `json:"spent"`
	Reserved    int64  `json:"reserved"`
}

// LedgerEntry is one committed transition after the last checkpoint.
type LedgerEntry struct {
	Seq          int64
	RequestID    string
	AttemptIndex int
	Transition   string
	OccurredAt   time.Time
	AppName      string
	Deltas       []LimitDelta
}

// BudgetBucket is the projection of one calendar window of one subject.
type BudgetBucket struct {
	AppName     string
	Scope       string
	Subject     string
	Anonymous   bool
	Dimension   string
	WindowStart int64
	WindowEnd   int64
	Spent       int64
	Reserved    int64
}

// ZoneState records a timezone change that waits for the active windows to end.
type ZoneState struct {
	Active     string `json:"active"`
	Pending    string `json:"pending,omitempty"`
	ActivateAt int64  `json:"activate_at,omitempty"`
}

// BucketRef names a bucket whose history could not be proven, which stays
// blocked until its window ends.
type BucketRef struct {
	AppName     string `json:"app_name"`
	Scope       string `json:"scope"`
	Subject     string `json:"subject"`
	Anonymous   bool   `json:"anonymous"`
	Dimension   string `json:"dimension"`
	WindowStart int64  `json:"window_start"`
	WindowEnd   int64  `json:"window_end"`
}

type CheckpointMetadata struct {
	Version int                  `json:"version"`
	Zones   map[string]ZoneState `json:"zones"`
	Unknown []BucketRef          `json:"unknown,omitempty"`
}

// LimitCheckpoint is the whole limit projection up to CoveredSeq.
type LimitCheckpoint struct {
	CoveredSeq int64
	CreatedAt  time.Time
	Watermark  time.Time
	Metadata   CheckpointMetadata
	// Buckets are the rows changed since the previous checkpoint; Removed are
	// expired windows without reservations.
	Buckets []BudgetBucket
	Removed []BudgetBucket
	// RateCutoff drops settled rate events admitted at or before it.
	RateCutoff time.Time
}

// OpenReservation is a reservation still reserved at startup, with the best
// available evidence of how its attempt ended.
type OpenReservation struct {
	Reservation Reservation
	Result      *Record
}

// MissingUsage counts retained records whose token usage is unknown, per
// daily aggregate row.
type MissingUsage struct {
	Day, Timezone, AppName, Scope, Subject string
	Anonymous                              bool
	Count                                  int64
	EstimatedTokens                        int64
}

// LimitState is everything recovery needs to rebuild the limit projection.
type LimitState struct {
	Checkpoint *LimitCheckpoint
	Ledger     []LedgerEntry
	Open       []OpenReservation
	RateEvents []RateEvent
	Daily      []Daily
	Missing    []MissingUsage
}

// LimitStore persists reservations together with the usage journal.
type LimitStore interface {
	AdmitReserved(context.Context, Record, Reservation) (int64, error)
	CompleteSettled(context.Context, Record, Settlement) (int64, error)
	SettleReservation(context.Context, Settlement) (int64, error)
	LoadLimits(context.Context, time.Time) (LimitState, error)
	SaveCheckpoint(context.Context, LimitCheckpoint) error
}
