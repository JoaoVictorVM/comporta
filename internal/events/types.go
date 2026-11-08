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
)

type Payload struct {
	ErrorClass   string              `json:"error_class,omitempty"`
	ErrorCode    string              `json:"error_code,omitempty"`
	Outcome      string              `json:"outcome,omitempty"`
	AttemptIndex int                 `json:"attempt_index,omitempty"`
	Cost         config.Microusd     `json:"cost_microusd,omitempty"`
	Count        int64               `json:"count,omitempty"`
	UsageMissing bool                `json:"usage_missing,omitempty"`
	Paused       bool                `json:"paused,omitempty"`
	Generation   int64               `json:"generation,omitempty"`
	Result       config.ReloadResult `json:"result,omitempty"`
	Changes      []config.Change     `json:"changes,omitempty"`
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
	Payload   Payload   `json:"payload"`
}

func New(kind Type, at time.Time) Event {
	var id [16]byte
	_, _ = rand.Read(id[:])
	return Event{ID: hex.EncodeToString(id[:]), Type: kind, At: at.UTC()}
}
func (e Event) Validate() error {
	switch e.Type {
	case RequestCompleted, Blocked, Cached, Fallback, Downgraded, LoopCut, ConfigChanged, ConfigReloadFailed, Paused, SensitiveDataDetected:
	default:
		return errors.New("invalid event type")
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
	case "", "provider_unreachable", "provider_timeout", "provider_redirect", "invalid_upstream_response", "provider_rate_limited", "provider_overloaded", "provider_error", "internal_error", "client_disconnected", "price_unknown", "pricing_overflow", "usage_storage_unavailable":
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

func ValidType(kind Type) bool {
	switch kind {
	case RequestCompleted, Blocked, Cached, Fallback, Downgraded, LoopCut, ConfigChanged, ConfigReloadFailed, Paused, SensitiveDataDetected:
		return true
	}
	return false
}

func (e Event) Clone() Event {
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
