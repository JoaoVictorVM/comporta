package storage

import (
	"context"
	"encoding/json"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/events"
)

// Bounds of the panel queries. Each one is fixed: no screen can ask a backend
// for an unbounded amount of rows.
const (
	PanelQueryTimeout   = 5 * time.Second
	PanelPageSize       = 50
	PanelMaxRequests    = 500
	PanelTopUsers       = 10
	PanelProviderWindow = time.Hour
	PanelBucket         = 5 * time.Minute
	PanelBuckets        = 12
	PanelMaxModels      = 20
)

// PanelOutcomes is the closed set of request outcomes the panel filters on.
var PanelOutcomes = []string{"ok", "blocked", "cached", "fallback", "downgraded", "loop_cut"}

// ValidPanelOutcome reports whether an outcome belongs to PanelOutcomes.
func ValidPanelOutcome(outcome string) bool {
	for _, o := range PanelOutcomes {
		if o == outcome {
			return true
		}
	}
	return false
}

// PanelRequestFilter selects request attempts newest first. Every field is
// optional; a user hash needs its app, because hashes are scoped by app.
type PanelRequestFilter struct {
	App       string
	UserHash  string
	Anonymous bool
	Model     string
	Outcome   string
	// Before is the exclusive upper position of the page, in descending order.
	Before *RecordCursor
	Limit  int
}

// PanelRequestPage is one page of attempts in descending order.
type PanelRequestPage struct {
	Records []Record
	Next    *RecordCursor
}

// PanelEventFilter selects events newest first.
type PanelEventFilter struct {
	App    string
	Types  []events.Type
	Before *events.Cursor
	Limit  int
}

// PanelUserCost is the aggregated cost of one pseudonymous user of an app.
type PanelUserCost struct {
	Hash      string
	Anonymous bool
	Cost      config.Microusd
	Requests  int64
}

// PanelModelUse is the attempts and cost of one model of an app.
type PanelModelUse struct {
	Model    string
	Attempts int64
	Cost     config.Microusd
}

// PanelProviderStats aggregates the attempts sent to one provider in a window.
// Failures count only overload, timeout, connection and 5xx: a client error
// says nothing about the provider's health.
type PanelProviderStats struct {
	Provider      string
	Attempts      int64
	Successes     int64
	Failures      int64
	P50LatencyUS  int64
	P95LatencyUS  int64
	InputTokens   int64
	CacheReadToks int64
}

// PanelProviderBucket is one five-minute bucket of a provider series. A bucket
// without attempts is absent, never zero.
type PanelProviderBucket struct {
	Provider string
	Index    int
	Attempts int64
	Failures int64
}

// PanelWatermark advances whenever any replica writes a request or an event.
// It pairs the newest timestamp with the count of rows inside WatermarkSpan,
// so it is compared only for equality.
type PanelWatermark struct {
	RequestsLatest, RequestsRecent int64
	EventsLatest, EventsRecent     int64
}

// PanelStore is the read side of the admin panel. Every operation runs under
// its caller's deadline in one consistent read and returns aggregates or
// validated records only.
type PanelStore interface {
	UsageQueryStore
	PanelTopUsers(ctx context.Context, app, from, through string) ([]PanelUserCost, error)
	PanelModelMix(ctx context.Context, app string, from, until time.Time) ([]PanelModelUse, error)
	PanelRequests(ctx context.Context, filter PanelRequestFilter) (PanelRequestPage, error)
	PanelRequest(ctx context.Context, requestID string, attempt int) (Record, bool, error)
	PanelEvents(ctx context.Context, filter PanelEventFilter) (events.Page, error)
	PanelProviders(ctx context.Context, from, until time.Time) ([]PanelProviderStats, []PanelProviderBucket, error)
	PanelWatermark(ctx context.Context) (PanelWatermark, error)
}

// HealthFailureClasses are the error classes that lower a provider's health.
var HealthFailureClasses = []string{"overload", "timeout", "connection", "upstream_5xx"}

// ValidatePanelRequestFilter applies the rules shared by both backends.
func ValidatePanelRequestFilter(f PanelRequestFilter) (int, error) {
	limit := f.Limit
	if limit == 0 {
		limit = PanelPageSize
	}
	if limit < 1 || limit > PanelMaxRequests {
		return 0, ErrInvalid
	}
	if f.App != "" && !RecordName.MatchString(f.App) {
		return 0, ErrInvalid
	}
	if f.UserHash != "" && (f.App == "" || !HexID.MatchString(f.UserHash)) {
		return 0, ErrInvalid
	}
	if len(f.Model) > 256 || (f.Outcome != "" && !ValidPanelOutcome(f.Outcome)) {
		return 0, ErrInvalid
	}
	return limit, nil
}

// ValidatePanelEventFilter applies the rules shared by both backends.
func ValidatePanelEventFilter(f PanelEventFilter) (int, error) {
	limit := f.Limit
	if limit == 0 {
		limit = PanelPageSize
	}
	if limit < 1 || limit > 1000 || (f.App != "" && !RecordName.MatchString(f.App)) {
		return 0, ErrInvalid
	}
	for _, kind := range f.Types {
		if !events.ValidType(kind) {
			return 0, ErrInvalid
		}
	}
	return limit, nil
}

// NearestRank is the 1-based position of a percentile in n sorted values.
func NearestRank(n, percentile int64) int64 {
	if n <= 0 {
		return 0
	}
	return (n*percentile + 99) / 100
}

// EventRow is one events row as both backends read it.
type EventRow struct {
	ID, Type, RequestID, App, Hash, Payload string
	At                                      int64
	Anonymous                               *bool
}

// DecodeEventRow validates a stored event. Only loop cuts and sensitive data
// detections carry a subject in the event contract; any other row loses it.
func DecodeEventRow(row EventRow) (events.Event, error) {
	e := events.Event{ID: row.ID, Type: events.Type(row.Type), RequestID: row.RequestID, App: row.App, At: time.UnixMicro(row.At).UTC()}
	if (e.Type == events.LoopCut || e.Type == events.SensitiveDataDetected) && row.Hash != "" && row.Anonymous != nil {
		e.UserHash = row.Hash
		flag := *row.Anonymous
		e.Anonymous = &flag
	}
	if err := json.Unmarshal([]byte(row.Payload), &e.Payload); err != nil {
		return events.Event{}, ErrInvalid
	}
	if err := e.Validate(); err != nil {
		return events.Event{}, ErrInvalid
	}
	return e, nil
}

// WatermarkSpan is how far back the watermark counts recent rows, so a write
// that lands with an older timestamp than the newest row still moves it.
const WatermarkSpan = 10 * time.Minute
