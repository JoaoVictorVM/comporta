package events

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/JoaoVictorVM/comporta/internal/config"
	"regexp"
	"time"
)

type Type string

const (
	RequestCompleted      Type = "request_completed"
	Blocked               Type = "blocked"
	Cached                Type = "cached"
	Fallback              Type = "fallback"
	Downgraded            Type = "downgraded"
	LoopCut               Type = "loop_cut"
	ConfigChanged         Type = "config_changed"
	ConfigReloadFailed    Type = "config_reload_failed"
	Paused                Type = "paused"
	SensitiveDataDetected Type = "sensitive_data_detected"
	LimitStateUnavailable Type = "limit_state_unavailable"
	LimitStateRecovered   Type = "limit_state_recovered"
)

type Payload struct {
	ErrorClass   string          `json:"error_class,omitempty"`
	ErrorCode    string          `json:"error_code,omitempty"`
	Outcome      string          `json:"outcome,omitempty"`
	AttemptIndex int             `json:"attempt_index,omitempty"`
	Cost         config.Microusd `json:"cost_microusd,omitempty"`
	Count        int64           `json:"count,omitempty"`
	UsageMissing bool            `json:"usage_missing,omitempty"`
	// Paused is set only on pause events, where false (a resume) has to stay
	// distinguishable from an absent field.
	Paused     *bool               `json:"paused,omitempty"`
	Generation int64               `json:"generation,omitempty"`
	Result     config.ReloadResult `json:"result,omitempty"`
	Changes    []config.Change     `json:"changes,omitempty"`
	// Limit fields carry categories and configuration paths only: never a user
	// hash, so alerts built from them cannot single out an end user.
	Severity        string `json:"severity,omitempty"`
	Reason          string `json:"reason,omitempty"`
	LimitName       string `json:"limit_name,omitempty"`
	Scope           string `json:"scope,omitempty"`
	DegradedSeconds int64  `json:"degraded_seconds,omitempty"`
	// WindowMS and CooldownMS exist only on loop cuts, rounded up to the
	// millisecond.
	WindowMS   int64 `json:"window_ms,omitempty"`
	CooldownMS int64 `json:"cooldown_ms,omitempty"`
}

var eventID = regexp.MustCompile(`^[0-9a-f]{32}$`)
var appName = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)

func Completion(requestID string, index int, at time.Time, app, outcome string, cost config.Microusd, missing bool) Event {
	id := sha256.Sum256([]byte(fmt.Sprintf("completion:%s:%d", requestID, index)))
	return Event{ID: hex.EncodeToString(id[:16]), Type: RequestCompleted, At: at, RequestID: requestID, App: app, Payload: Payload{AttemptIndex: index, Outcome: outcome, Cost: cost, UsageMissing: missing}}
}

type Event struct {
	ID        string    `json:"event_id"`
	Type      Type      `json:"type"`
	At        time.Time `json:"occurred_at"`
	RequestID string    `json:"request_id,omitempty"`
	App       string    `json:"app,omitempty"`
	// UserHash and Anonymous name the pseudonymous subject of a loop cut and
	// nothing else. They are a pair: both present or both absent.
	UserHash  string  `json:"user_hash,omitempty"`
	Anonymous *bool   `json:"anonymous,omitempty"`
	Payload   Payload `json:"payload"`
}

// Loop cut reasons.
const (
	ReasonSimilarRequests = "similar_requests"
	ReasonRequestRate     = "request_rate"
)

// NewLoopCut builds the event of one loop episode. Durations are rounded up to the
// millisecond so a sub-millisecond configuration never reads as zero.
func NewLoopCut(id string, at time.Time, requestID, app, userHash string, anonymous bool, reason string, count int64, window, cooldown time.Duration) Event {
	return Event{
		ID: id, Type: LoopCut, At: at.UTC(), RequestID: requestID, App: app, UserHash: userHash, Anonymous: &anonymous,
		Payload: Payload{ErrorClass: "rate_limit", ErrorCode: "loop_detected", Outcome: "loop_cut", Reason: reason, Count: count,
			WindowMS: ceilMillis(window), CooldownMS: ceilMillis(cooldown)},
	}
}

func ceilMillis(d time.Duration) int64 {
	return int64((d + time.Millisecond - 1) / time.Millisecond)
}

// ValidateLoopCut holds a new loop cut to its full contract. Validate alone
// still accepts older loop cut events stored without a subject.
func (e Event) ValidateLoopCut() error {
	if err := e.Validate(); err != nil {
		return err
	}
	if e.Type != LoopCut || e.UserHash == "" || e.RequestID == "" || e.Payload.Reason == "" || e.Payload.Count < 1 || e.Payload.WindowMS < 1 || e.Payload.CooldownMS < 1 {
		return errors.New("invalid loop cut event")
	}
	return nil
}

func New(kind Type, at time.Time) Event {
	var id [16]byte
	_, _ = rand.Read(id[:])
	return Event{ID: hex.EncodeToString(id[:]), Type: kind, At: at.UTC()}
}
func (e Event) Validate() error {
	switch e.Type {
	case RequestCompleted, Blocked, Cached, Fallback, Downgraded, LoopCut, ConfigChanged, ConfigReloadFailed, Paused, SensitiveDataDetected, LimitStateUnavailable, LimitStateRecovered:
	default:
		return errors.New("invalid event type")
	}
	if err := e.validateLimitFields(); err != nil {
		return err
	}
	if err := e.validatePause(); err != nil {
		return err
	}
	if err := e.validateLoop(); err != nil {
		return err
	}
	if !eventID.MatchString(e.ID) || (e.RequestID != "" && !eventID.MatchString(e.RequestID)) || (e.App != "" && !appName.MatchString(e.App)) || e.At.IsZero() || e.Payload.Count < 0 || e.Payload.Cost < 0 || e.Payload.AttemptIndex < 0 {
		return errors.New("invalid event")
	}
	if e.Type != ConfigChanged && e.Type != ConfigReloadFailed {
		if len(e.Payload.Changes) > 0 || e.Payload.Generation != 0 || e.Payload.Result != "" {
			return errors.New("invalid event payload")
		}
	}
	if e.Payload.Outcome != "" {
		switch e.Payload.Outcome {
		case "ok", "blocked", "cached", "fallback", "downgraded", "loop_cut":
		default:
			return errors.New("invalid event outcome")
		}
	}
	switch e.Payload.ErrorClass {
	case "", "invalid_request", "authentication", "rate_limit", "overload", "upstream_5xx", "connection", "timeout", "protocol", "client_cancel", "internal":
	default:
		return errors.New("invalid event error class")
	}
	switch e.Payload.ErrorCode {
	case "", "provider_unreachable", "provider_timeout", "provider_redirect", "invalid_upstream_response", "provider_rate_limited", "provider_overloaded", "provider_error", "internal_error", "client_disconnected", "price_unknown", "pricing_overflow", "usage_storage_unavailable",
		"rate_limited", "concurrency_exceeded", "budget_exceeded", "budget_reserved", "hard_cap_exceeded", "limits_unavailable", "app_paused", "user_maintenance",
		"loop_detected", "loop_guard_unavailable":
	default:
		return errors.New("invalid event error code")
	}
	if e.Type == ConfigChanged || e.Type == ConfigReloadFailed {
		switch e.Payload.Result {
		case config.ResultInitial, config.ResultApplied, config.ResultUnchanged, config.ResultRejected:
		default:
			return errors.New("invalid reload event")
		}
	}
	return nil
}

type Publisher interface {
	Publish(context.Context, Event) error
}

type Cursor struct {
	At time.Time
	ID string
}
type Filter struct {
	App         string
	Types       []Type
	From, Until time.Time
	After       *Cursor
	Limit       int
}
type Page struct {
	Events []Event
	Next   *Cursor
}
type Reader interface {
	ReadEvents(context.Context, Filter) (Page, error)
}

var limitName = regexp.MustCompile(`^apps\.[a-z0-9-]{1,40}\.(per_user\.)?(limits|budget|hard_cap)\.[a-z_]{1,32}$`)

func (e Event) validateLimitFields() error {
	switch e.Payload.Severity {
	case "", "info", "warning":
	default:
		return errors.New("invalid event severity")
	}
	switch e.Payload.Reason {
	case "", "spent", "reservation", "checkpoint", "settlement", "integrity":
		if e.Type == LoopCut && e.Payload.Reason != "" {
			return errors.New("invalid event reason")
		}
	case ReasonSimilarRequests, ReasonRequestRate:
		if e.Type != LoopCut {
			return errors.New("invalid event reason")
		}
	default:
		return errors.New("invalid event reason")
	}
	switch e.Payload.Scope {
	case "", "app", "user", "global":
	default:
		return errors.New("invalid event scope")
	}
	if (e.Payload.LimitName != "" && !limitName.MatchString(e.Payload.LimitName)) || e.Payload.DegradedSeconds < 0 {
		return errors.New("invalid limit event payload")
	}
	return nil
}

// validatePause requires the explicit state and scope of a pause event, and keeps
// both out of every other type.
func (e Event) validatePause() error {
	if e.Type != Paused {
		if e.Payload.Paused != nil || e.Payload.Scope == "global" {
			return errors.New("invalid event payload")
		}
		return nil
	}
	switch {
	case e.Payload.Paused == nil:
		return errors.New("invalid pause event")
	case e.Payload.Scope == "global" && e.App == "":
	case e.Payload.Scope == "app" && e.App != "":
	default:
		return errors.New("invalid pause event")
	}
	return nil
}

// validateLoop keeps the subject and the loop metadata on loop cuts only.
func (e Event) validateLoop() error {
	hasSubject := e.UserHash != "" || e.Anonymous != nil
	hasLoop := e.Payload.WindowMS != 0 || e.Payload.CooldownMS != 0
	if e.Type != LoopCut {
		if hasSubject || hasLoop {
			return errors.New("invalid event payload")
		}
		return nil
	}
	if hasSubject && (e.Anonymous == nil || !eventID.MatchString(e.UserHash) || e.App == "") {
		return errors.New("invalid loop cut subject")
	}
	if e.Payload.WindowMS < 0 || e.Payload.CooldownMS < 0 {
		return errors.New("invalid loop cut payload")
	}
	return nil
}

func ValidType(kind Type) bool {
	switch kind {
	case RequestCompleted, Blocked, Cached, Fallback, Downgraded, LoopCut, ConfigChanged, ConfigReloadFailed, Paused, SensitiveDataDetected, LimitStateUnavailable, LimitStateRecovered:
		return true
	}
	return false
}

func (e Event) Clone() Event {
	if e.Anonymous != nil {
		anonymous := *e.Anonymous
		e.Anonymous = &anonymous
	}
	if e.Payload.Paused != nil {
		paused := *e.Payload.Paused
		e.Payload.Paused = &paused
	}
	e.Payload.Changes = append([]config.Change(nil), e.Payload.Changes...)
	for i := range e.Payload.Changes {
		c := &e.Payload.Changes[i]
		if c.Old != nil {
			x := *c.Old
			c.Old = &x
		}
		if c.New != nil {
			x := *c.New
			c.New = &x
		}
	}
	return e
}
