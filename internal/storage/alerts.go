package storage

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/events"
)

// ErrAlertBacklogFull means the delivery outbox reached its ceiling. The input
// stays pending and is retried later; nothing is dropped.
var ErrAlertBacklogFull = errors.New("alert delivery backlog is full")

// ErrClaimLost means the claim token no longer owns the row: its lease expired
// and another worker took it, or it was already finished.
var ErrClaimLost = errors.New("alert claim lost")

// ErrAlertSourceMissing means the record an input points to no longer exists,
// so the input is finished without an occurrence.
var ErrAlertSourceMissing = errors.New("alert source record missing")

// OccurrenceID derives the identity of an occurrence from its deduplication
// key, so every replica and every retry computes the same row.
func OccurrenceID(dedupeKey string) string {
	sum := sha256.Sum256([]byte("alert-occurrence:" + dedupeKey))
	return hex.EncodeToString(sum[:16])
}

// DeliveryID derives the stable delivery identity of an occurrence on one
// channel. Receivers deduplicate on it.
func DeliveryID(occurrenceID, channel string) string {
	sum := sha256.Sum256([]byte("alert-delivery:" + occurrenceID + ":" + channel))
	return hex.EncodeToString(sum[:16])
}

// NewClaimToken draws the token that owns a claim until its lease ends.
func NewClaimToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// Alert limits shared by every backend.
const (
	MaxAlertBatch          = 100
	MaxAlertDeliveries     = 100_000
	MaxAlertPayloadBytes   = 16 << 10
	MaxAlertDedupeKeyBytes = 512
	MaxAlertAttempts       = 4
	AlertRetentionBatch    = 1000
)

// Alert delivery states.
const (
	DeliveryPending   = "pending"
	DeliveryClaimed   = "claimed"
	DeliveryDelivered = "delivered"
	DeliveryFailed    = "failed"
	DeliveryCanceled  = "canceled"
)

// AlertInputTypes are the event types that feed the alert inbox. The backends
// enqueue them in the same transaction that writes the event.
var AlertInputTypes = []events.Type{
	events.RequestCompleted, events.Blocked, events.Fallback, events.LoopCut,
	events.ConfigChanged, events.ConfigReloadFailed, events.LimitStateUnavailable,
}

// AlertInput is one claimed inbox entry with the event it points to. Event is
// nil when the event was removed before the input was processed.
type AlertInput struct {
	EventID    string
	EventType  events.Type
	EnqueuedAt time.Time
	ClaimToken string
	Event      *events.Event
}

// AlertOccurrence is one deduplicated alert. Payload is the canonical safe JSON
// projection, never a rendered message.
type AlertOccurrence struct {
	ID               string
	DedupeKey        string
	Trigger          string
	OccurredAt       time.Time
	App              string
	Scope            string
	UserHash         string
	Anonymous        *bool
	Dimension        string
	WindowStart      *time.Time
	WindowEnd        *time.Time
	ThresholdPercent int
	Payload          []byte
	CreatedAt        time.Time
}

// AlertChannelRef is the identity of a channel at expansion time. Secrets are
// never persisted: the delivery resolves them from the current snapshot.
type AlertChannelRef struct {
	Name string
	Type string
}

// AlertDelivery is one claimed delivery with the occurrence it carries.
type AlertDelivery struct {
	ID           string
	OccurrenceID string
	Channel      string
	ChannelType  string
	State        string
	Attempts     int
	ClaimToken   string
	CreatedAt    time.Time
	Occurrence   AlertOccurrence
}

// AlertResult is how one delivery attempt ended. Error is a closed category.
type AlertResult struct {
	// Outcome is delivered, retry, failed or canceled.
	Outcome string
	Status  int
	Error   string
	// RetryAt is when the next attempt becomes eligible; only with retry.
	RetryAt time.Time
	// Sent reports whether a call reached the network, which consumes an attempt.
	Sent bool
}

// Alert result outcomes.
const (
	AlertDelivered = "delivered"
	AlertRetry     = "retry"
	AlertFailed    = "failed"
	AlertCanceled  = "canceled"
)

// AlertBudget is the confirmed spend of one budget bucket: the checkpointed
// projection plus every settlement written after it. Reservations never count.
type AlertBudget struct {
	Scope       string
	Subject     string
	Anonymous   bool
	Dimension   string
	WindowStart int64
	WindowEnd   int64
	Spent       int64
}

// UsageBucket is the confirmed cost of one app in one UTC five minute bucket.
type UsageBucket struct {
	Start time.Time
	App   string
	Cost  int64
}

// AlertBacklog is the operational state of the outbox.
type AlertBacklog struct {
	PendingInputs     int64
	PendingDeliveries int64
	OldestDelivery    time.Duration
}

// AlertStore is the durable side of alerts. Every write that groups several rows
// is one transaction, and claims require the current token.
type AlertStore interface {
	ClaimAlertInputs(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]AlertInput, error)
	// CompleteAlertInput stores the occurrences, expands one delivery per
	// channel for each new occurrence and marks the input processed, atomically.
	CompleteAlertInput(ctx context.Context, eventID, token string, occurrences []AlertOccurrence, channels []AlertChannelRef, now time.Time) error
	ReleaseAlertInput(ctx context.Context, eventID, token string) error
	// RecordAlertOccurrences stores occurrences found by a scheduled evaluation,
	// with the same deduplication as inputs.
	RecordAlertOccurrences(ctx context.Context, occurrences []AlertOccurrence, channels []AlertChannelRef, now time.Time) error
	ClaimAlertDeliveries(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]AlertDelivery, error)
	FinishAlertDelivery(ctx context.Context, deliveryID, token string, result AlertResult, now time.Time) error
	ReleaseAlertDelivery(ctx context.Context, deliveryID, token string) error
	AlertRecord(ctx context.Context, requestID string, attempt int) (Record, error)
	AlertBudgets(ctx context.Context, app, scope, subject string, anonymous bool, at time.Time) ([]AlertBudget, error)
	CountAlertEvents(ctx context.Context, app string, kind events.Type, from, until time.Time) (int64, error)
	UsageBuckets(ctx context.Context, from, until time.Time) ([]UsageBucket, error)
	AlertBacklog(ctx context.Context, now time.Time) (AlertBacklog, error)
	RecentAlertOccurrences(ctx context.Context, limit int) ([]AlertOccurrence, error)
	RetainAlerts(ctx context.Context, now time.Time) error
}

// AlertTriggers is the closed set of triggers.
var AlertTriggers = map[string]bool{
	"budget_threshold": true, "hard_cap": true, "anomaly": true, "loop_cut": true,
	"fallback_burst": true, "config_reload_failed": true, "fail_open": true,
}

var alertCategory = regexp.MustCompile(`^[a-z_]{1,32}$`)

// ValidateOccurrence holds an occurrence to the shape every backend enforces.
func ValidateOccurrence(o AlertOccurrence) error {
	if !HexID.MatchString(o.ID) || o.DedupeKey == "" || len(o.DedupeKey) > MaxAlertDedupeKeyBytes || !AlertTriggers[o.Trigger] || o.OccurredAt.IsZero() {
		return ErrInvalid
	}
	switch o.Scope {
	case "global", "app", "user":
	default:
		return ErrInvalid
	}
	if o.App != "" && !RecordName.MatchString(o.App) {
		return ErrInvalid
	}
	if (o.UserHash == "") != (o.Anonymous == nil) || (o.UserHash != "" && !HexID.MatchString(o.UserHash)) {
		return ErrInvalid
	}
	if o.Dimension != "" && !ValidDimension(o.Dimension) {
		return ErrInvalid
	}
	if o.WindowStart != nil && o.WindowEnd != nil && !o.WindowEnd.After(*o.WindowStart) {
		return ErrInvalid
	}
	if o.ThresholdPercent < 0 || o.ThresholdPercent > 100 || len(o.Payload) == 0 || len(o.Payload) > MaxAlertPayloadBytes {
		return ErrInvalid
	}
	return nil
}

// ValidateAlertResult keeps the persisted outcome to closed categories.
func ValidateAlertResult(r AlertResult) error {
	switch r.Outcome {
	case AlertDelivered, AlertRetry, AlertFailed, AlertCanceled:
	default:
		return ErrInvalid
	}
	if (r.Status != 0 && (r.Status < 100 || r.Status > 599)) || (r.Error != "" && !alertCategory.MatchString(r.Error)) {
		return ErrInvalid
	}
	if r.Outcome == AlertRetry && r.RetryAt.IsZero() {
		return ErrInvalid
	}
	return nil
}

// ValidChannelType reports a known channel type.
func ValidChannelType(t string) bool {
	switch t {
	case "webhook", "discord", "slack", "ntfy":
		return true
	}
	return false
}

// NextDeliveryState is the shared lifecycle rule: it returns the new state and
// attempt count after an attempt ends.
func NextDeliveryState(attempts int, r AlertResult) (string, int) {
	if r.Sent {
		attempts++
	}
	switch r.Outcome {
	case AlertDelivered:
		return DeliveryDelivered, attempts
	case AlertCanceled:
		return DeliveryCanceled, attempts
	case AlertRetry:
		if attempts < MaxAlertAttempts {
			return DeliveryPending, attempts
		}
	}
	return DeliveryFailed, attempts
}

// AlertRetentionCutoffs are the retention rules of the alert tables.
func AlertRetentionCutoffs(now time.Time) (inputs, terminal, usage time.Time) {
	return now.AddDate(0, 0, -7), now.AddDate(0, 0, -30), now.AddDate(0, 0, -8)
}

// FoldAlertBudgets adds the settlements written after the checkpoint to the
// checkpointed buckets that contain at, for one subject.
func FoldAlertBudgets(base []AlertBudget, ledger []LedgerPayload, app, scope, subject string, anonymous bool, at int64) []AlertBudget {
	type key struct {
		dimension  string
		start, end int64
	}
	index := map[key]int{}
	out := append([]AlertBudget(nil), base...)
	for i, b := range out {
		index[key{b.Dimension, b.WindowStart, b.WindowEnd}] = i
	}
	for _, p := range ledger {
		if p.AppName != app {
			continue
		}
		for _, d := range p.Deltas {
			if d.Scope != scope || d.Subject != subject || d.Anonymous != anonymous || d.Spent == 0 || d.WindowStart > at || at >= d.WindowEnd {
				continue
			}
			k := key{d.Dimension, d.WindowStart, d.WindowEnd}
			i, ok := index[k]
			if !ok {
				out = append(out, AlertBudget{Scope: scope, Subject: subject, Anonymous: anonymous, Dimension: d.Dimension, WindowStart: d.WindowStart, WindowEnd: d.WindowEnd})
				i = len(out) - 1
				index[k] = i
			}
			out[i].Spent += d.Spent
		}
	}
	return out
}
