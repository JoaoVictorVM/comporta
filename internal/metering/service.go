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

// Admit freezes the quote, then asks the limit coordinator for a reservation
// committed together with the journal entry. A reload that overtakes the
// admission restarts it on the current snapshot, quote included, so a stale
// ceiling is never applied after a newer one.
func (s *Service) Admit(ctx context.Context, snapshot *config.Snapshot, e *proxy.Envelope, name string, p config.Provider) (proxy.Admission, *proxy.Rejection) {
	for attempt := 0; ; attempt++ {
		admission, rejection, stale := s.admit(ctx, snapshot, e, name, p)
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

func (s *Service) admit(ctx context.Context, snapshot *config.Snapshot, e *proxy.Envelope, name string, p config.Provider) (proxy.Admission, *proxy.Rejection, bool) {
	reject := func(status int, code, msg string) (proxy.Admission, *proxy.Rejection, bool) {
		class := proxy.ClassInvalidRequest
		if status >= 500 {
			class = proxy.ClassInternal
		}
		return proxy.Admission{}, &proxy.Rejection{Status: status, Code: proxy.Code(code), Class: class, Message: msg}, false
	}
	q := pricing.Quote{}
	var err error
	if e.Operation == proxy.OperationGeneration {
		q, err = s.catalog.Resolve(snapshot, p, e.ModelEffective)
		if err != nil {
			return reject(400, "price_unknown", fmt.Sprintf("no price defined for model %q; add it under pricing in comporta.yaml", bounded(e.ModelEffective)))
		}
	}
	input, err := pricing.EstimateInput(e.BodySizeOriginal, e.BodySizeEffective, e.ImageCount)
	if err != nil {
		return reject(400, "pricing_overflow", pricing.ErrOverflow.Error())
	}
	reserved := config.Microusd(0)
	tokens := int64(0)
	if e.Operation == proxy.OperationGeneration {
		reserved, err = q.Estimate(e.BodySizeOriginal, e.BodySizeEffective, e.ImageCount, e.MaxTokens.Value)
		if err != nil {
			return reject(400, "pricing_overflow", pricing.ErrOverflow.Error())
		}
		if e.MaxTokens.Value < 0 || input > math.MaxInt64-e.MaxTokens.Value {
			return reject(400, "pricing_overflow", pricing.ErrOverflow.Error())
		}
		// The token reservation is the estimated input plus the capped output,
		// the same worst case the cost reservation is built from.
		tokens = input + e.MaxTokens.Value
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
	frozen := q.Frozen()
	frozen.EstimatedInput = input
	frozen.EstimatedOutput = e.MaxTokens.Value
	frozen.ReservedCost = reserved
	frozen.Generation = snapshot.Generation()
	frozen.InputFormulaCost, _ = pricing.Cost(pricing.Tokens{Input: input}, q.Price(input))
	r := storage.Record{RequestID: e.RequestID, AttemptIndex: 1, StartedAt: now, AppName: e.AppName, UserHash: e.Identity.Hash().Hex(), Anonymous: e.Identity.Anonymous(), Operation: string(e.Operation), Provider: name, ModelRequested: e.ModelRequested, ModelUsed: e.ModelEffective, Outcome: "ok", ReservedCost: reserved, Day: now.In(location).Format("2006-01-02"), Timezone: app.Timezone, Quote: frozen, RequestCount: 1}
	if s.limits == nil {
		if err = s.store.Admit(ctx, r); err != nil {
			s.warnStorage(err)
			<-s.slots
			return reject(503, "usage_storage_unavailable", storage.ErrUnavailable.Error())
		}
		var once sync.Once
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
		Operation: operation, EstimatedTokens: tokens, ReservedCost: reserved, Generation: snapshot.Generation()}
	handle, decision, err := s.limits.Reserve(policy, request, func(res storage.Reservation) (int64, error) {
		r.LimitsDegraded = res.Quote.Degraded
		return s.limitStore.AdmitReserved(ctx, r, res)
	})
	switch {
	case errors.Is(err, limits.ErrStale):
		<-s.slots
		return proxy.Admission{}, nil, true
	case errors.Is(err, limits.ErrUnavailable):
		s.block(r, now, 503, string(proxy.CodeLimitsUnavailable), string(proxy.ClassInternal), "")
		return reject(503, string(proxy.CodeLimitsUnavailable), limits.ErrUnavailable.Error())
	case errors.Is(err, limits.ErrIntegrity), errors.Is(err, limits.ErrConflict):
		<-s.slots
		s.logger.Error("limit state integrity failure; request rejected")
		return reject(503, "internal_error", "internal error")
	case err != nil:
		s.warnStorage(err)
		<-s.slots
		return reject(503, "usage_storage_unavailable", storage.ErrUnavailable.Error())
	case !decision.Allowed:
		return s.refuse(r, now, *decision.Primary)
	}
	var once sync.Once
	return proxy.Admission{
		Complete: func(result proxy.AttemptResult) { once.Do(func() { s.complete(r, q, handle, result) }) },
		Budget:   budgetHeaders(decision),
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
