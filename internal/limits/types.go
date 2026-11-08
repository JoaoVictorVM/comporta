// Package limits decides whether an attempt fits the rate limits, concurrency
// limits and calendar budgets of its app and user, and keeps the reservations
// that make concurrent admissions safe. It never imports the proxy or SQLite:
// durability is reached through storage.LimitStore and caller commit functions.
package limits

import (
	"errors"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
)

var (
	// ErrUnavailable means limit state cannot be verified in fail_closed mode.
	ErrUnavailable = errors.New("limits cannot be verified; request rejected (fail_closed)")
	// ErrIntegrity is an arithmetic or ledger inconsistency. It never fails open.
	ErrIntegrity = errors.New("limit state integrity failure")
	// ErrStale means a newer configuration generation already reached the
	// coordinator; the admission must be redone on the current snapshot.
	ErrStale = errors.New("configuration generation is stale")
	// ErrConflict means the same idempotency key arrived with different data.
	ErrConflict = errors.New("incompatible reservation")
)

type Scope string

const (
	ScopeApp  Scope = "app"
	ScopeUser Scope = "user"
)

type Dimension string

const (
	RequestsPerMinute Dimension = "requests_per_minute"
	TokensPerMinute   Dimension = "tokens_per_minute"
	MaxConcurrent     Dimension = "max_concurrent"
	USDPerDay         Dimension = "usd_per_day"
	USDPerMonth       Dimension = "usd_per_month"
	TokensPerDay      Dimension = "tokens_per_day"
	TokensPerMonth    Dimension = "tokens_per_month"
)

// calendarDimensions is also the stable evaluation order inside a budget group.
var calendarDimensions = []Dimension{USDPerDay, USDPerMonth, TokensPerDay, TokensPerMonth}

func (d Dimension) calendar() bool {
	switch d {
	case USDPerDay, USDPerMonth, TokensPerDay, TokensPerMonth:
		return true
	}
	return false
}

func (d Dimension) monthly() bool { return d == USDPerMonth || d == TokensPerMonth }

func (d Dimension) money() bool { return d == USDPerDay || d == USDPerMonth }

// Unit of the values of a dimension.
func (d Dimension) Unit() Unit {
	switch d {
	case RequestsPerMinute:
		return UnitRequests
	case MaxConcurrent:
		return UnitConcurrent
	case USDPerDay, USDPerMonth:
		return UnitMicrousd
	}
	return UnitTokens
}

type Unit string

const (
	UnitRequests   Unit = "requests"
	UnitConcurrent Unit = "concurrent_requests"
	UnitTokens     Unit = "tokens"
	UnitMicrousd   Unit = "microusd"
)

type Kind string

const (
	KindRate        Kind = "rate"
	KindConcurrency Kind = "concurrency"
	KindBudget      Kind = "budget"
	KindHardCap     Kind = "hard_cap"
	KindReservation Kind = "reservation"
)

type Reason string

const (
	// ReasonSpent: the candidate would exceed even without other reservations.
	ReasonSpent Reason = "spent"
	// ReasonReservation: the candidate fits once in-flight reservations end.
	ReasonReservation Reason = "reservation"
)

type Operation string

const (
	OperationGeneration  Operation = "generation"
	OperationCountTokens Operation = "count_tokens"
)

// ReleaseReason says why a reservation ends without consumption.
type ReleaseReason string

const (
	ReleaseNotSent ReleaseReason = "not_sent"
	ReleaseCache   ReleaseReason = "cache"
)

// Subject is the key of a limit. App scope has an empty hash and is never
// anonymous; user scope uses the HMAC hash and an explicit anonymous flag, so
// the anonymous bucket of one app never meets another app's.
type Subject struct {
	App       string
	Hash      string
	Anonymous bool
}

func (s Subject) scope() Scope {
	if s.Hash == "" {
		return ScopeApp
	}
	return ScopeUser
}

func (s Subject) appSubject() Subject { return Subject{App: s.App} }

// Set is the ceilings of one scope. Zero disables a dimension.
type Set struct {
	Limits  config.Limits
	Budget  config.Budget
	HardCap config.Budget
}

func (s Set) rate(d Dimension) int64 {
	switch d {
	case RequestsPerMinute:
		return s.Limits.RequestsPerMinute
	case TokensPerMinute:
		return s.Limits.TokensPerMinute
	case MaxConcurrent:
		return s.Limits.MaxConcurrent
	}
	return 0
}

func ceiling(b config.Budget, d Dimension) int64 {
	switch d {
	case USDPerDay:
		return int64(b.USDPerDay)
	case USDPerMonth:
		return int64(b.USDPerMonth)
	case TokensPerDay:
		return b.TokensPerDay
	case TokensPerMonth:
		return b.TokensPerMonth
	}
	return 0
}

// Policy is the limit configuration of one app in one generation.
type Policy struct {
	App      string
	Timezone string
	FailMode config.FailMode
	OnLimit  config.OnLimit
	AppSet   Set
	UserSet  Set
}

// PolicyFor extracts the limits of an app from a snapshot.
func PolicyFor(snapshot *config.Snapshot, name string) (Policy, bool) {
	app, ok := snapshot.App(name)
	if !ok {
		return Policy{}, false
	}
	return Policy{
		App: name, Timezone: app.Timezone, FailMode: app.FailMode, OnLimit: app.OnLimit,
		AppSet:  Set{Limits: app.Limits, Budget: app.Budget, HardCap: app.HardCap},
		UserSet: Set{Limits: app.PerUser.Limits, Budget: app.PerUser.Budget, HardCap: app.PerUser.HardCap},
	}, true
}

func (p Policy) set(scope Scope) Set {
	if scope == ScopeApp {
		return p.AppSet
	}
	return p.UserSet
}

// LimitName is the stable configuration path of a ceiling.
func (p Policy) LimitName(scope Scope, kind Kind, d Dimension) string {
	group := "budget"
	switch {
	case kind == KindHardCap:
		group = "hard_cap"
	case !d.calendar():
		group = "limits"
	}
	prefix := "apps." + p.App + "."
	if scope == ScopeUser {
		prefix += "per_user."
	}
	return prefix + group + "." + string(d)
}

// Request is one attempt asking for admission. Identity fields come from the
// authenticated pipeline, never from request fields.
type Request struct {
	RequestID       string
	AttemptIndex    int
	Subject         Subject
	Operation       Operation
	EstimatedTokens int64
	ReservedCost    config.Microusd
	Generation      int64
}

// Violation describes one exceeded limit.
type Violation struct {
	Kind      Kind       `json:"kind"`
	Reason    Reason     `json:"reason"`
	Scope     Scope      `json:"scope"`
	LimitName string     `json:"limit_name"`
	Dimension Dimension  `json:"dimension"`
	Unit      Unit       `json:"unit"`
	Used      int64      `json:"used"`
	Reserved  int64      `json:"reserved"`
	Requested int64      `json:"requested"`
	Limit     int64      `json:"limit"`
	RenewsAt  *time.Time `json:"renews_at"`
	// RetryAfter is in whole seconds; zero when no honest value exists.
	RetryAfter int64 `json:"retry_after_seconds,omitempty"`
	// TooLarge marks a request larger than the whole limit.
	TooLarge bool   `json:"request_too_large_for_limit,omitempty"`
	Timezone string `json:"timezone,omitempty"`
	App      string `json:"-"`
	hardCap  bool
}

// Key is the short identifier used in Decision.Exceeded.
func (v Violation) Key() string {
	group := "limits"
	switch {
	case v.hardCap:
		group = "hard_cap"
	case v.Dimension.calendar():
		group = "budget"
	}
	return string(v.Scope) + "." + group + "." + string(v.Dimension)
}

// HardCap reports whether the violated ceiling is a hard cap.
func (v Violation) HardCap() bool { return v.hardCap }

// BudgetStatus is the most restrictive applicable budget right after admission.
type BudgetStatus struct {
	LimitName string
	Unit      Unit
	Remaining int64
	Limit     int64
	ResetAt   time.Time
}

// Decision is the outcome of an evaluation. Exceeded lists every violation for
// policy consumers; Primary follows the stable evaluation order.
type Decision struct {
	Allowed  bool
	Degraded bool
	Primary  *Violation
	Exceeded []Violation
	Budget   *BudgetStatus
}

// Usage is the consumption of a finished attempt. Tokens is the sum of the four
// disjoint usage dimensions; Complete says every counter was reported.
type Usage struct {
	Cost     config.Microusd
	Tokens   int64
	Complete bool
}

// LimitView is one ceiling as consumers see it.
type LimitView struct {
	LimitName string
	Scope     Scope
	Kind      Kind
	Dimension Dimension
	Unit      Unit
	Used      int64
	Reserved  int64
	Limit     int64
	RenewsAt  *time.Time
	Unknown   bool
}

// State is a consistent copy of the limits of an app and, optionally, a user.
type State struct {
	App             string
	Degraded        bool
	ActiveTimezone  string
	PendingTimezone string
	PendingAt       *time.Time
	Generation      int64
	Concurrent      int64
	UserConcurrent  int64
	Limits          []LimitView
}
