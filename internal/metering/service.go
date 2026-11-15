package metering

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/limits"
	"github.com/JoaoVictorVM/comporta/internal/pricing"
	"github.com/JoaoVictorVM/comporta/internal/proxy"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

type Store interface {
	storage.AdmissionStore
	storage.CompletionStore
	storage.MaintenanceStore
}

// Source supplies the configuration generation in force.
type Source interface {
	Capture() *config.Snapshot
}

type Service struct {
	limits      *limits.Manager
	limitStore  storage.LimitStore
	source      Source
	flushMu     sync.Mutex
	store       Store
	catalog     *pricing.Catalog
	logger      *slog.Logger
	mu          sync.Mutex
	pending     []storage.Record
	slots       chan struct{}
	wake        chan struct{}
	now         func() time.Time
	retention   func() int64
	unhealthy   bool
	lastWarning time.Time
	newTimer    func(time.Duration) timer
}

func New(store Store, catalog *pricing.Catalog, logger *slog.Logger, retention func() int64) *Service {
	return &Service{store: store, catalog: catalog, logger: logger, slots: make(chan struct{}, 50000), wake: make(chan struct{}, 1), now: time.Now, retention: retention, newTimer: newSystemTimer}
}

// UseLimits connects the limit coordinator. store writes reservations in the
// same transactions as the usage journal, so it must be the usage database.
func (s *Service) UseLimits(manager *limits.Manager, store storage.LimitStore, source Source) error {
	if manager == nil || store == nil {
		return errors.New("limits need a coordinator and a store")
	}
	s.limits, s.limitStore, s.source = manager, store, source
	return nil
}

var errReloaded = &proxy.Rejection{Status: 503, Code: proxy.CodeInternalError, Class: proxy.ClassInternal, Message: "configuration changed during admission; retry the request"}

// Admit admits the request as prepared, without a policy target. A reload that
// overtakes the admission restarts it on the current snapshot, quote included,
// so a stale ceiling is never applied after a newer one.
func (s *Service) Admit(ctx context.Context, snapshot *config.Snapshot, e *proxy.Envelope, name string, p config.Provider) (proxy.Admission, *proxy.Rejection) {
	for attempt := 0; ; attempt++ {
		admission, rejection, stale := s.admit(ctx, snapshot, e, []proxy.Candidate{original(e, name, p)})
		if !stale {
			return admission, rejection
		}
		if s.source == nil || attempt >= 3 {
			rejection := *errReloaded
			return proxy.Admission{}, &rejection
		}
		snapshot = s.source.Capture()
		current, ok := snapshot.ProviderForModel(e.ModelEffective)
		provider, found := snapshot.Provider(current)
		if _, app := snapshot.App(e.AppName); !ok || !found || !app || current != name || provider.APIFormat != p.APIFormat {
			rejection := *errReloaded
			return proxy.Admission{}, &rejection
		}
		p = provider
	}
}

// AdmitCandidates prices every candidate on the same snapshot and lets the
// limit coordinator choose and reserve one. A reload that overtakes the
// admission comes back as Stale: the bodies depend on the snapshot, so only
// the proxy can prepare them again.
func (s *Service) AdmitCandidates(ctx context.Context, snapshot *config.Snapshot, e *proxy.Envelope, candidates []proxy.Candidate) (proxy.Admission, *proxy.Rejection) {
	admission, rejection, stale := s.admit(ctx, snapshot, e, candidates)
	if stale {
		rejection := *errReloaded
		rejection.Stale = true
		return proxy.Admission{}, &rejection
	}
	return admission, rejection
}

func original(e *proxy.Envelope, name string, p config.Provider) proxy.Candidate {
	return proxy.Candidate{ID: proxy.CandidateOriginal, Model: e.ModelEffective, ProviderName: name, Provider: p, BodySizeEffective: e.BodySizeEffective}
}

// priced is a candidate with the quote frozen for its own body.
type priced struct {
	candidate proxy.Candidate
	quote     pricing.Quote
	frozen    storage.Quote
	tokens    int64
	reserved  config.Microusd
}

func (s *Service) price(snapshot *config.Snapshot, e *proxy.Envelope, c proxy.Candidate) (priced, *proxy.Rejection) {
	out := priced{candidate: c}
	reject := func(code, msg string) (priced, *proxy.Rejection) {
		return out, &proxy.Rejection{Status: 400, Code: proxy.Code(code), Class: proxy.ClassInvalidRequest, Message: msg}
	}
	var err error
	if e.Operation == proxy.OperationGeneration {
		out.quote, err = s.catalog.Resolve(snapshot, c.Provider, c.Model)
		if err != nil {
			return reject("price_unknown", fmt.Sprintf("no price defined for model %q; add it under pricing in comporta.yaml", bounded(c.Model)))
		}
	}
	input, err := pricing.EstimateInput(e.BodySizeOriginal, c.BodySizeEffective, e.ImageCount)
	if err != nil {
		return reject("pricing_overflow", pricing.ErrOverflow.Error())
	}
	if e.Operation == proxy.OperationGeneration {
		out.reserved, err = out.quote.Estimate(e.BodySizeOriginal, c.BodySizeEffective, e.ImageCount, e.MaxTokens.Value)
		if err != nil {
			return reject("pricing_overflow", pricing.ErrOverflow.Error())
		}
		if e.MaxTokens.Value < 0 || input > math.MaxInt64-e.MaxTokens.Value {
			return reject("pricing_overflow", pricing.ErrOverflow.Error())
		}
		// The token reservation is the estimated input plus the capped output,
		// the same worst case the cost reservation is built from.
		out.tokens = input + e.MaxTokens.Value
	}
	out.frozen = out.quote.Frozen()
	out.frozen.EstimatedInput = input
	out.frozen.EstimatedOutput = e.MaxTokens.Value
	out.frozen.ReservedCost = out.reserved
	out.frozen.Generation = snapshot.Generation()
	out.frozen.InputFormulaCost, _ = pricing.Cost(pricing.Tokens{Input: input}, out.quote.Price(input))
	return out, nil
}

// withPolicy fills the record of the chosen candidate. It is pure, so a repeat
// that finds the reservation already made builds the same record.
func withPolicy(r storage.Record, chosen priced, sel limits.Selection) storage.Record {
	r.Provider, r.ModelUsed = chosen.candidate.ProviderName, chosen.candidate.Model
	r.ReservedCost, r.Quote = chosen.reserved, chosen.frozen
	r.PolicyOutcome = string(sel.Outcome)
	if sel.Outcome == limits.PolicyDowngraded {
		r.Outcome = "downgraded"
	}
	if v := sel.Trigger; v != nil {
		r.PolicyLimitName, r.PolicyScope, r.PolicyReason = v.LimitName, string(v.Scope), string(v.Reason)
		if v.RenewsAt != nil {
			renew := v.RenewsAt.UTC()
			r.PolicyRenewsAt = &renew
		}
	}
	return r
}

func (s *Service) admit(ctx context.Context, snapshot *config.Snapshot, e *proxy.Envelope, candidates []proxy.Candidate) (proxy.Admission, *proxy.Rejection, bool) {
	reject := func(status int, code, msg string) (proxy.Admission, *proxy.Rejection, bool) {
		class := proxy.ClassInvalidRequest
		if status >= 500 {
			class = proxy.ClassInternal
		}
		return proxy.Admission{}, &proxy.Rejection{Status: status, Code: proxy.Code(code), Class: class, Message: msg}, false
	}
	if len(candidates) == 0 || len(candidates) > 2 || candidates[0].ID != proxy.CandidateOriginal || candidates[0].Unavailable != nil ||
		(len(candidates) == 2 && candidates[1].ID != proxy.CandidateDowngrade) {
		return reject(500, "internal_error", "internal error")
	}
	base, rejection := s.price(snapshot, e, candidates[0])
	if rejection != nil {
		return proxy.Admission{}, rejection, false
	}
	// A target that cannot be prepared or priced only matters if the policy
	// needs it; then the request is refused without being sent.
	var target *priced
	var targetRefusal *proxy.Rejection
	if len(candidates) == 2 {
		targetRefusal = candidates[1].Unavailable
		if targetRefusal == nil {
			quoted, refusal := s.price(snapshot, e, candidates[1])
			if refusal == nil {
				target = &quoted
			}
			targetRefusal = refusal
		}
	}
	s.mu.Lock()
	bad := s.unhealthy
	s.mu.Unlock()
	if bad {
		return reject(503, "usage_storage_unavailable", storage.ErrUnavailable.Error())
	}
	select {
	case s.slots <- struct{}{}:
	default:
		return reject(503, "usage_storage_unavailable", storage.ErrUnavailable.Error())
	}
	now := s.now().UTC()
	app, _ := snapshot.App(e.AppName)
	location, err := time.LoadLocation(app.Timezone)
	if err != nil {
		<-s.slots
		return reject(503, "usage_storage_unavailable", storage.ErrUnavailable.Error())
	}
	r := storage.Record{RequestID: e.RequestID, AttemptIndex: 1, StartedAt: now, AppName: e.AppName, UserHash: e.Identity.Hash().Hex(), Anonymous: e.Identity.Anonymous(), Operation: string(e.Operation), Provider: base.candidate.ProviderName, ModelRequested: e.ModelRequested, ModelUsed: base.candidate.Model, Outcome: "ok", ReservedCost: base.reserved, Day: now.In(location).Format("2006-01-02"), Timezone: app.Timezone, Quote: base.frozen, RequestCount: 1, Detections: e.Sensitive.Clone()}
	if s.limits == nil {
		if err = s.store.Admit(ctx, r); err != nil {
			s.warnStorage(err)
			<-s.slots
			return reject(503, "usage_storage_unavailable", storage.ErrUnavailable.Error())
		}
		var once sync.Once
		q := base.quote
		return proxy.Admission{Complete: func(result proxy.AttemptResult) { once.Do(func() { s.complete(r, q, nil, result) }) }}, nil, false
	}
	policy, ok := limits.PolicyFor(snapshot, e.AppName)
	if !ok {
		<-s.slots
		return reject(503, "internal_error", "internal error")
	}
	operation := limits.OperationGeneration
	if e.Operation == proxy.OperationCountTokens {
		operation = limits.OperationCountTokens
	}
	request := limits.Request{RequestID: e.RequestID, AttemptIndex: 1, Subject: limits.Subject{App: e.AppName, Hash: r.UserHash, Anonymous: r.Anonymous},
		Operation: operation, Generation: snapshot.Generation()}
	options := []limits.Candidate{{ID: limits.CandidateOriginal, Model: base.candidate.Model, EstimatedTokens: base.tokens, ReservedCost: base.reserved}}
	if len(candidates) == 2 {
		option := limits.Candidate{ID: limits.CandidateDowngrade, Model: candidates[1].Model, Unavailable: target == nil}
		if target != nil {
			option.EstimatedTokens, option.ReservedCost = target.tokens, target.reserved
		}
		options = append(options, option)
	}
	chosen := func(sel limits.Selection) priced {
		if sel.Candidate == limits.CandidateDowngrade && target != nil {
			return *target
		}
		return base
	}
	handle, decision, sel, err := s.limits.ReserveSelect(policy, request, options, func(res storage.Reservation, sel limits.Selection) (int64, error) {
		admitted := withPolicy(r, chosen(sel), sel)
		admitted.LimitsDegraded = res.Quote.Degraded
		return s.limitStore.AdmitReserved(ctx, admitted, res)
	})
	switch {
	case errors.Is(err, limits.ErrStale):
		<-s.slots
		return proxy.Admission{}, nil, true
	case errors.Is(err, limits.ErrUnavailable):
		s.block(r, now, 503, string(proxy.CodeLimitsUnavailable), string(proxy.ClassInternal), "")
		return reject(503, string(proxy.CodeLimitsUnavailable), limits.ErrUnavailable.Error())
	case errors.Is(err, limits.ErrTargetUnavailable):
		answer := targetRefusal
		if answer == nil {
			answer = &proxy.Rejection{Status: 503, Code: proxy.CodeInternalError, Class: proxy.ClassInternal, Message: "internal error"}
		}
		blocked := withPolicy(r, base, limits.Selection{Outcome: limits.PolicyBlocked, Trigger: decision.Primary})
		s.block(blocked, now, answer.Status, string(answer.Code), string(answer.Class), "")
		return proxy.Admission{}, answer, false
	case errors.Is(err, limits.ErrIntegrity), errors.Is(err, limits.ErrConflict):
		<-s.slots
		s.logger.Error("limit state integrity failure; request rejected")
		return reject(503, "internal_error", "internal error")
	case err != nil:
		s.warnStorage(err)
		<-s.slots
		return reject(503, "usage_storage_unavailable", storage.ErrUnavailable.Error())
	case !decision.Allowed:
		return s.refuse(withPolicy(r, base, limits.Selection{Outcome: limits.PolicyBlocked, Trigger: decision.Primary}), now, *decision.Primary)
	}
	admitted := withPolicy(r, chosen(sel), sel)
	admitted.LimitsDegraded = handle.Degraded()
	q := chosen(sel).quote
	var once sync.Once
	return proxy.Admission{
		Complete: func(result proxy.AttemptResult) { once.Do(func() { s.complete(admitted, q, handle, result) }) },
		Budget:   budgetHeaders(decision),
		Selected: proxy.CandidateID(sel.Candidate),
		Policy:   proxy.PolicyOutcome(sel.Outcome),
	}, nil, false
}

func limitCode(v limits.Violation) proxy.Code {
	switch v.Kind {
	case limits.KindConcurrency:
		return proxy.CodeConcurrencyExceeded
	case limits.KindHardCap:
		return proxy.CodeHardCapExceeded
	case limits.KindReservation:
		return proxy.CodeBudgetReserved
	case limits.KindBudget:
		return proxy.CodeBudgetExceeded
	}
	return proxy.CodeRateLimited
}

// refuse answers a limit violation with 429 and records it as a local block.
func (s *Service) refuse(r storage.Record, now time.Time, v limits.Violation) (proxy.Admission, *proxy.Rejection, bool) {
	code := limitCode(v)
	s.block(r, now, 429, string(code), string(proxy.ClassRateLimit), string(v.Reason))
	rejection := &proxy.Rejection{Status: 429, Code: code, Class: proxy.ClassRateLimit, Message: limits.Message(v, r.AppName, now),
		Limit: &proxy.LimitDetail{LimitName: v.LimitName, Scope: string(v.Scope), Reason: string(v.Reason), RenewsAt: v.RenewsAt}}
	if v.Kind == limits.KindRate {
		rejection.RetryAfter = v.RetryAfter
	}
	return proxy.Admission{}, rejection, false
}

// block queues the terminal record of a request refused before any attempt.
// The caller holds an admission slot, which the record keeps until its batch
// is flushed, like any other record.
func (s *Service) block(r storage.Record, now time.Time, status int, code, class, reason string) {
	r.AttemptIndex, r.Outcome, r.RequestCount = 0, "blocked", 0
	r.ReservedCost, r.Cost, r.Quote.ReservedCost = 0, 0, 0
	r.FinishedAt, r.ErrorClass, r.ErrorCode, r.BlockReason = now, class, code, reason
	r.DownstreamStatus = &status
	s.mu.Lock()
	s.pending = append(s.pending, r)
	s.mu.Unlock()
}

func budgetHeaders(d limits.Decision) *proxy.BudgetHeaders {
	b := d.Budget
	if b == nil || d.Degraded {
		return nil
	}
	h := &proxy.BudgetHeaders{Reset: b.ResetAt, Limit: b.LimitName, Unit: "tokens", Remaining: strconv.FormatInt(b.Remaining, 10)}
	if b.Unit == limits.UnitMicrousd {
		h.Unit, h.Remaining = "usd", config.Microusd(b.Remaining).String()
	}
	return h
}

func bounded(s string) string {
	if len(s) > 120 {
		return s[:120]
	}
	return s
}
func optional[T any](v proxy.Optional[T]) *T {
	if !v.Known {
		return nil
	}
	x := v.Value
	return &x
}

func (s *Service) complete(r storage.Record, q pricing.Quote, handle *limits.Handle, result proxy.AttemptResult) {
	r.FinishedAt = s.now().UTC()
	if r.FinishedAt.Before(r.StartedAt) {
		r.FinishedAt = r.StartedAt
	}
	r.LatencyUS = max(0, result.Latency.Microseconds())
	r.UpstreamStatus = optional(result.UpstreamStatus)
	r.DownstreamStatus = optional(result.DownstreamStatus)
	r.ErrorClass = string(result.ErrorClass)
	r.ErrorCode = string(result.ErrorCode)
	if result.UpstreamTTFB.Known {
		v := max(0, result.UpstreamTTFB.Value.Microseconds())
		r.UpstreamTTFB = &v
	}
	if result.ClientTTFB.Known {
		v := max(0, result.ClientTTFB.Value.Microseconds())
		r.ClientTTFB = &v
	}
	if result.NotSent {
		r.Cost = 0
		r.Outcome = "blocked"
	} else if result.Operation == proxy.OperationCountTokens {
		r.CountedInput = optional(result.CountedInputTokens)
	} else {
		u := result.Usage
		r.Input = optional(u.Input)
		r.Output = optional(u.Output)
		r.CacheWrite = optional(u.CacheWrite)
		r.CacheRead = optional(u.CacheRead)
		r.RawInputTotal = optional(u.RawInputTotal)
		r.CacheWrite5m = optional(u.CacheWrite5m)
		r.CacheWrite1h = optional(u.CacheWrite1h)
		tokens := pricing.Tokens{Input: u.Input.Value, Output: u.Output.Value, CacheWrite: u.CacheWrite.Value, CacheRead: u.CacheRead.Value}
		cost, err := q.Cost(tokens, r.CacheWrite5m, r.CacheWrite1h)
		if err != nil {
			s.mu.Lock()
			s.unhealthy = true
			s.mu.Unlock()
			s.logger.Error("usage accounting failed; admission paused")
			return
		}
		r.Cost = cost
		r.CacheWriteEstimated = q.CacheWriteEstimated(tokens.CacheWrite, r.CacheWrite5m, r.CacheWrite1h)
		r.UsageMissing = !result.UsageComplete
		if r.UsageMissing {
			r.Cost = max(r.Cost, r.ReservedCost)
		}
		r.EstimateExceeded = r.Cost > r.ReservedCost
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.finish(ctx, r, handle, result); err != nil {
		if errors.Is(err, storage.ErrInvalid) || errors.Is(err, storage.ErrConflict) {
			s.mu.Lock()
			s.unhealthy = true
			s.mu.Unlock()
		}
		s.warnStorage(err)
	}
	s.mu.Lock()
	s.pending = append(s.pending, r)
	full := len(s.pending) >= 1000
	s.mu.Unlock()
	if full {
		select {
		case s.wake <- struct{}{}:
		default:
		}
	}
}

// finish makes the result durable. With limits, the settlement or release of
// the reservation is written in the same transaction; on failure the
// reservation stays conservatively held and the limit worker retries it.
func (s *Service) finish(ctx context.Context, r storage.Record, handle *limits.Handle, result proxy.AttemptResult) error {
	if handle == nil {
		return s.store.Complete(ctx, r)
	}
	// The deadline covers the write only: waiting for the coordinator behind a
	// burst of admissions is not a storage failure.
	persist := func(st storage.Settlement) (int64, error) {
		write, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		return s.limitStore.CompleteSettled(write, r, st)
	}
	if result.NotSent {
		return s.limits.Release(handle, limits.ReleaseNotSent, persist)
	}
	usage := limits.Usage{Cost: r.Cost, Complete: !r.UsageMissing}
	for _, n := range []*int64{r.Input, r.Output, r.CacheWrite, r.CacheRead} {
		if n != nil {
			if usage.Tokens > math.MaxInt64-*n {
				usage.Tokens = math.MaxInt64
			} else {
				usage.Tokens += *n
			}
		}
	}
	return s.limits.Settle(handle, usage, persist)
}

func (s *Service) warnStorage(err error) {
	s.mu.Lock()
	now := s.now()
	log := s.lastWarning.IsZero() || now.Sub(s.lastWarning) >= 30*time.Second
	if log {
		s.lastWarning = now
	}
	s.mu.Unlock()
	if log {
		if errors.Is(err, storage.ErrFull) {
			s.logger.Error("data directory is full")
		} else {
			s.logger.Error("usage persistence unavailable; records retained")
		}
	}
}
