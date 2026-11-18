package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"regexp"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/events"
)

// The rules below are shared by every backend, so a record accepted by one is
// accepted by the other and both produce the same events.

// HexID is a request identifier or a user pseudonym.
var HexID = regexp.MustCompile(`^[0-9a-f]{32}$`)

func ValidateRecord(r Record) error {
	if !HexID.MatchString(r.RequestID) || !HexID.MatchString(r.UserHash) || r.AppName == "" || r.AttemptIndex < 0 || r.Cost < 0 || r.ReservedCost < 0 || r.StartedAt.IsZero() || r.LatencyUS < 0 {
		return ErrInvalid
	}
	if r.Operation != "generation" && r.Operation != "count_tokens" {
		return ErrInvalid
	}
	switch r.Outcome {
	case "ok", "blocked", "cached", "fallback", "downgraded", "loop_cut":
	default:
		return ErrInvalid
	}
	if _, err := time.LoadLocation(r.Timezone); err != nil {
		return ErrInvalid
	}
	if _, err := time.Parse("2006-01-02", r.Day); err != nil {
		return ErrInvalid
	}
	if r.RequestCount < 0 || r.RequestCount > 1 {
		return ErrInvalid
	}
	if r.Outcome == "loop_cut" && r.AttemptIndex != 0 {
		return ErrInvalid
	}
	switch r.BlockReason {
	case "", "spent", "reservation":
	default:
		return ErrInvalid
	}
	if r.Detections != nil && (len(r.Detections) == 0 || r.Detections.Validate() != nil) {
		return ErrInvalid
	}
	if err := r.ValidatePolicy(); err != nil {
		return err
	}
	if err := r.ValidateFallback(); err != nil || !Category.MatchString(r.TerminalErrorCode) {
		return ErrInvalid
	}
	// A downgraded transport outcome only exists when the policy downgraded.
	if r.Outcome == "downgraded" && r.PolicyOutcome != "downgraded" {
		return ErrInvalid
	}
	// Attempt 0 is a request refused locally before any attempt: it is terminal,
	// costs nothing and never counts as a request in the generation totals. A
	// loop cut is only ever attempt 0.
	if r.AttemptIndex == 0 && ((r.Outcome != "blocked" && r.Outcome != "loop_cut") || r.RequestCount != 0 || r.Cost != 0 || r.ReservedCost != 0 || r.FinishedAt.IsZero()) {
		return ErrInvalid
	}
	if !r.FinishedAt.IsZero() && r.FinishedAt.Before(r.StartedAt) {
		return ErrInvalid
	}
	if !RecordName.MatchString(r.AppName) || len(r.ModelRequested) > 256 || len(r.ModelUsed) > 256 || !Category.MatchString(r.ErrorClass) || !Category.MatchString(r.ErrorCode) {
		return ErrInvalid
	}
	for _, status := range []*int{r.UpstreamStatus, r.DownstreamStatus} {
		if status != nil && (*status < 100 || *status > 599) {
			return ErrInvalid
		}
	}
	if r.Quote.Threshold < 0 || r.Quote.Write1h < 0 || r.Quote.HighWrite1h < 0 || r.Quote.EstimatedInput < 0 || r.Quote.EstimatedOutput < 0 || r.Quote.InputFormulaCost < 0 || r.Quote.ReservedCost < 0 {
		return ErrInvalid
	}
	for _, p := range []config.Price{r.Quote.Price, r.Quote.HighPrice} {
		if p.Input < 0 || p.Output < 0 || p.CacheWrite < 0 || p.CacheRead < 0 {
			return ErrInvalid
		}
	}
	switch r.Quote.Source {
	case "", "local", "override", "embedded":
	default:
		return ErrInvalid
	}
	for _, n := range []*int64{r.Input, r.Output, r.CacheWrite, r.CacheRead, r.RawInputTotal, r.CountedInput, r.CacheWrite5m, r.CacheWrite1h, r.UpstreamTTFB, r.ClientTTFB} {
		if n != nil && *n < 0 {
			return ErrInvalid
		}
	}
	return nil
}

var RecordName = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)
var Category = regexp.MustCompile(`^[a-z0-9_]{0,64}$`)

func DecodeRecord(data string, r *Record) error {
	d := json.NewDecoder(bytes.NewBufferString(data))
	d.DisallowUnknownFields()
	if err := d.Decode(r); err != nil {
		return ErrInvalid
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return ErrInvalid
	}
	return ValidateRecord(*r)
}

func SameAdmission(a, b Record) bool {
	qa, _ := json.Marshal(a.Quote)
	qb, _ := json.Marshal(b.Quote)
	return a.RequestID == b.RequestID && a.AttemptIndex == b.AttemptIndex && a.AppName == b.AppName && a.UserHash == b.UserHash && a.Anonymous == b.Anonymous && a.Operation == b.Operation && a.Provider == b.Provider && a.ModelRequested == b.ModelRequested && a.ModelUsed == b.ModelUsed && a.StartedAt.Equal(b.StartedAt) && a.Day == b.Day && a.Timezone == b.Timezone && a.ReservedCost == b.ReservedCost && string(qa) == string(qb) &&
		a.PolicyOutcome == b.PolicyOutcome && a.PolicyLimitName == b.PolicyLimitName && a.FallbackFrom == b.FallbackFrom && a.FallbackCause == b.FallbackCause
}

func BlockedEvent(r Record) events.Event {
	id := sha256.Sum256([]byte("blocked:" + r.RequestID))
	return events.Event{ID: hex.EncodeToString(id[:16]), Type: events.Blocked, At: r.FinishedAt, RequestID: r.RequestID, App: r.AppName,
		Payload: events.Payload{Outcome: r.Outcome, ErrorClass: r.ErrorClass, ErrorCode: r.ErrorCode, Reason: r.BlockReason}}
}

func EventFor(r Record) events.Event {
	if r.AttemptIndex == 0 {
		return BlockedEvent(r)
	}
	return CompletionEvent(r)
}

func CompletionEvent(r Record) events.Event {
	e := events.Completion(r.RequestID, r.AttemptIndex, r.FinishedAt, r.AppName, r.Outcome, r.Cost, r.UsageMissing)
	e.Payload.ErrorClass = r.ErrorClass
	e.Payload.ErrorCode = r.ErrorCode
	return e
}

func CachedEvent(r Record) events.Event {
	return events.CacheHit(r.RequestID, r.AttemptIndex, r.FinishedAt, r.AppName)
}

func Value(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

type LedgerPayload struct {
	AppName string       `json:"app_name"`
	Deltas  []LimitDelta `json:"deltas"`
}

func ValidDimension(d string) bool {
	switch d {
	case "usd_per_day", "usd_per_month", "tokens_per_day", "tokens_per_month":
		return true
	}
	return false
}

func ValidSubject(scope, subject string, anonymous bool) bool {
	switch scope {
	case "app":
		return subject == "" && !anonymous
	case "user":
		return HexID.MatchString(subject)
	}
	return false
}

func ValidReservation(r Reservation) error {
	if !HexID.MatchString(r.RequestID) || r.AttemptIndex < 1 || !RecordName.MatchString(r.AppName) || r.AdmittedAt.IsZero() || r.State != ReservationReserved {
		return ErrInvalid
	}
	if r.Subject != "" && !HexID.MatchString(r.Subject) {
		return ErrInvalid
	}
	q := r.Quote
	if (q.Operation != "generation" && q.Operation != "count_tokens") || q.EstimatedTokens < 0 || q.ReservedCost < 0 {
		return ErrInvalid
	}
	for _, item := range r.Items {
		if !ValidSubject(item.Scope, item.Subject, item.Anonymous) || !ValidDimension(item.Dimension) || item.WindowEnd <= item.WindowStart || item.Reserved < 0 || item.Settled != nil {
			return ErrInvalid
		}
		if _, err := time.LoadLocation(item.Timezone); err != nil {
			return ErrInvalid
		}
	}
	if e := r.Rate; e != nil {
		// Attempt 1 is the logical request; later attempts only carry tokens.
		attempt := e.AttemptIndex
		if attempt == 0 {
			attempt = 1
		}
		if e.RequestID != r.RequestID || e.AppName != r.AppName || e.Subject != r.Subject || e.Anonymous != r.Anonymous || e.EstimatedTokens < 0 || e.ActualTokens != nil || !e.Active ||
			attempt != r.AttemptIndex || attempt > MaxAttempts {
			return ErrInvalid
		}
	}
	return nil
}

func DecodeStrict(data string, target any) error {
	d := json.NewDecoder(bytes.NewBufferString(data))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return ErrInvalid
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return ErrInvalid
	}
	return nil
}

func PolicyEvent(r Record) (events.Event, bool) {
	var kind events.Type
	switch r.PolicyOutcome {
	case "warned":
		kind = events.Warned
	case "downgraded":
		kind = events.Downgraded
	default:
		return events.Event{}, false
	}
	return events.NewPolicyAdmission(kind, r.StartedAt, r.RequestID, r.AttemptIndex, r.AppName, r.PolicyLimitName, r.PolicyScope, r.PolicyReason, r.ModelRequested, r.ModelUsed), true
}

func ItemsJSON(items []LimitItem) string {
	if items == nil {
		items = []LimitItem{}
	}
	data, _ := json.Marshal(items)
	return string(data)
}

func DeltaFor(item LimitItem, spent, reserved int64) LimitDelta {
	return LimitDelta{Scope: item.Scope, Subject: item.Subject, Anonymous: item.Anonymous, Dimension: item.Dimension, WindowStart: item.WindowStart, WindowEnd: item.WindowEnd, Spent: spent, Reserved: reserved}
}
