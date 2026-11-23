package panel

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/useridentity"
)

// modelPattern is the shape of a model name accepted as a filter. It only
// bounds the input; the query is parameterized either way.
var modelPattern = regexp.MustCompile(`^[A-Za-z0-9._:/@-]{1,256}$`)

// RequestQuery is a requests filter as submitted. UserID is a raw identifier
// that needs App; UserHash is a pseudonym already shown by the panel.
type RequestQuery struct {
	App      string
	UserID   string
	UserHash string
	Model    string
	Outcome  string
	Cursor   string
}

func (f RequestFilters) canonical() string {
	return strings.Join([]string{"requests", f.App, f.UserHash, f.Model, f.Outcome}, "\x00")
}

// resolve validates a query and turns a raw identifier into its pseudonym.
func (s *Service) resolveRequests(q RequestQuery) (RequestFilters, bool) {
	f := RequestFilters{App: q.App, Model: q.Model, Outcome: q.Outcome}
	if f.App != "" && !storage.RecordName.MatchString(f.App) {
		return RequestFilters{}, false
	}
	if f.Model != "" && !modelPattern.MatchString(f.Model) {
		return RequestFilters{}, false
	}
	if f.Outcome != "" && !storage.ValidPanelOutcome(f.Outcome) {
		return RequestFilters{}, false
	}
	switch {
	case q.UserID != "" && q.UserHash != "":
		return RequestFilters{}, false
	case q.UserID != "":
		if f.App == "" || s.identity == nil || useridentity.ValidateID(q.UserID) != nil {
			return RequestFilters{}, false
		}
		hash, err := s.identity.LookupHash(f.App, q.UserID)
		if err != nil {
			return RequestFilters{}, false
		}
		f.UserHash = hash.Hex()
	case q.UserHash != "":
		if f.App == "" || !storage.HexID.MatchString(q.UserHash) {
			return RequestFilters{}, false
		}
		f.UserHash = q.UserHash
	}
	return f, true
}

// Requests lists the most recent attempts, newest first, in pages of fifty up
// to five hundred.
func (s *Service) Requests(ctx context.Context, q RequestQuery) (RequestList, error) {
	filters, ok := s.resolveRequests(q)
	if !ok {
		return RequestList{Invalid: true}, nil
	}
	out := RequestList{Filters: filters}
	digest := filters.canonical()
	shown := 0
	var before *storage.RecordCursor
	if q.Cursor != "" {
		c, ok := s.cursors.open(q.Cursor, "requests", digest)
		if !ok || c.Shown >= storage.PanelMaxRequests {
			return RequestList{Invalid: true}, nil
		}
		shown = c.Shown
		before = &storage.RecordCursor{StartedAt: c.time(), RequestID: c.ID, AttemptIndex: c.Attempt}
	}
	limit := min(storage.PanelPageSize, storage.PanelMaxRequests-shown)
	var page storage.PanelRequestPage
	err := s.query(ctx, ScreenRequests, func(ctx context.Context) error {
		var err error
		page, err = s.store.PanelRequests(ctx, storage.PanelRequestFilter{App: filters.App, UserHash: filters.UserHash, Model: filters.Model, Outcome: filters.Outcome, Before: before, Limit: limit})
		return err
	})
	if err != nil {
		return out, err
	}
	for _, r := range page.Records {
		out.Rows = append(out.Rows, requestRow(r))
	}
	out.Shown = shown + len(out.Rows)
	if page.Next != nil {
		if out.Shown >= storage.PanelMaxRequests {
			out.Capped = true
		} else {
			out.Next = s.cursors.seal(cursor{Kind: "requests", At: page.Next.StartedAt.UnixMicro(), ID: page.Next.RequestID, Attempt: page.Next.AttemptIndex, Shown: out.Shown}, digest)
		}
	}
	return out, nil
}

// Request returns the metadata of one attempt.
func (s *Service) Request(ctx context.Context, requestID string, attempt int) (RequestDetail, error) {
	if !storage.HexID.MatchString(requestID) || attempt < 0 || attempt > storage.MaxAttempts {
		return RequestDetail{}, ErrNotFound
	}
	var record storage.Record
	var found bool
	err := s.query(ctx, ScreenRequests, func(ctx context.Context) error {
		var err error
		record, found, err = s.store.PanelRequest(ctx, requestID, attempt)
		return err
	})
	if err != nil {
		return RequestDetail{}, err
	}
	if !found {
		return RequestDetail{}, ErrNotFound
	}
	return requestDetail(record), nil
}

func optionalCount(v *int64) string {
	if v == nil {
		return "unknown"
	}
	return strconv.FormatInt(*v, 10)
}

func millis(us int64) string {
	return strconv.FormatFloat(float64(us)/1000, 'f', 1, 64)
}

// requestRow is the allowlist of fields a list shows. Anything not copied here
// cannot reach a template.
func requestRow(r storage.Record) RequestRow {
	row := RequestRow{
		RequestID: r.RequestID, Attempt: r.AttemptIndex, StartedAt: r.StartedAt, App: r.AppName, User: ShortHash(r.UserHash), Anonymous: r.Anonymous,
		Provider: r.Provider, ModelRequested: r.ModelRequested, ModelUsed: r.ModelUsed, Outcome: r.Outcome, Policy: r.PolicyOutcome,
		Cost: r.Cost, CostKnown: !r.UsageMissing && !r.Recovered, InputTokens: optionalCount(r.Input), OutputTokens: optionalCount(r.Output),
		LatencyMS: millis(r.LatencyUS), TTFBMS: "unknown",
	}
	if r.DownstreamStatus != nil {
		row.Status = strconv.Itoa(*r.DownstreamStatus)
	}
	if r.ClientTTFB != nil {
		row.TTFBMS = millis(*r.ClientTTFB)
	}
	if r.UsageMissing {
		row.Flags = append(row.Flags, "usage missing")
	}
	if r.Recovered {
		row.Flags = append(row.Flags, "recovered")
	}
	if r.EstimateExceeded {
		row.Flags = append(row.Flags, "estimate exceeded")
	}
	if r.LimitsDegraded {
		row.Flags = append(row.Flags, "limits degraded")
	}
	if len(r.Detections) > 0 {
		row.Flags = append(row.Flags, "sensitive data")
	}
	if r.Anonymous {
		row.Flags = append(row.Flags, "anonymous")
	}
	return row
}

func requestDetail(r storage.Record) RequestDetail {
	d := RequestDetail{
		RequestRow: requestRow(r), FinishedAt: r.FinishedAt, Operation: r.Operation, ErrorClass: r.ErrorClass, ErrorCode: r.ErrorCode,
		TerminalCode: r.TerminalErrorCode, PolicyLimit: r.PolicyLimitName, PolicyScope: r.PolicyScope, PolicyReason: r.PolicyReason, PolicyRenewsAt: r.PolicyRenewsAt,
		FallbackFrom: r.FallbackFrom, FallbackCause: r.FallbackCause, ReservedCost: r.ReservedCost, CacheSaved: r.CacheSaved, DowngradeSaved: r.DowngradeSaved,
		EstimateMethod: r.Quote.EstimateMethod, EstimatedInput: r.Quote.EstimatedInput, EstimatedOutput: r.Quote.EstimatedOutput,
		UsageMissing: r.UsageMissing, Recovered: r.Recovered, EstimateExceeded: r.EstimateExceeded, LimitsDegraded: r.LimitsDegraded,
		CacheRead: optionalCount(r.CacheRead), CacheWrite: optionalCount(r.CacheWrite), Terminal: r.RequestCount == 1,
	}
	if r.TerminalStatus != nil {
		d.TerminalStatus = strconv.Itoa(*r.TerminalStatus)
	}
	for _, det := range r.Detections {
		d.Detections = append(d.Detections, Detection{Category: det.DataType.String(), Count: det.Count, Action: det.Action.String()})
	}
	return d
}

// Cost formats an amount at the edge, as an estimate in dollars.
func Cost(m config.Microusd) string { return "$" + m.String() }
