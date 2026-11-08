package metering

import (
	"context"
	"errors"
	"fmt"
	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/pricing"
	"github.com/JoaoVictorVM/comporta/internal/proxy"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"log/slog"
	"sync"
	"time"
)

type Store interface {
	storage.AdmissionStore
	storage.CompletionStore
	storage.MaintenanceStore
}
type Service struct {
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

func (s *Service) Admit(ctx context.Context, snapshot *config.Snapshot, e *proxy.Envelope, name string, p config.Provider) (func(proxy.AttemptResult), *proxy.Rejection) {
	reject := func(status int, code, msg string) (func(proxy.AttemptResult), *proxy.Rejection) {
		class := proxy.ClassInvalidRequest
		if status >= 500 {
			class = proxy.ClassInternal
		}
		return nil, &proxy.Rejection{Status: status, Code: proxy.Code(code), Class: class, Message: msg}
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
	if e.Operation == proxy.OperationGeneration {
		reserved, err = q.Estimate(e.BodySizeOriginal, e.BodySizeEffective, e.ImageCount, e.MaxTokens.Value)
		if err != nil {
			return reject(400, "pricing_overflow", pricing.ErrOverflow.Error())
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
	frozen := q.Frozen()
	frozen.EstimatedInput = input
	frozen.EstimatedOutput = e.MaxTokens.Value
	frozen.ReservedCost = reserved
	frozen.Generation = snapshot.Generation()
	frozen.InputFormulaCost, _ = pricing.Cost(pricing.Tokens{Input: input}, q.Price(input))
	r := storage.Record{RequestID: e.RequestID, AttemptIndex: 1, StartedAt: now, AppName: e.AppName, UserHash: e.Identity.Hash().Hex(), Anonymous: e.Identity.Anonymous(), Operation: string(e.Operation), Provider: name, ModelRequested: e.ModelRequested, ModelUsed: e.ModelEffective, Outcome: "ok", ReservedCost: reserved, Day: now.In(location).Format("2006-01-02"), Timezone: app.Timezone, Quote: frozen, RequestCount: 1}
	if err = s.store.Admit(ctx, r); err != nil {
		s.warnStorage(err)
		<-s.slots
		return reject(503, "usage_storage_unavailable", storage.ErrUnavailable.Error())
	}
	var once sync.Once
	return func(result proxy.AttemptResult) { once.Do(func() { s.complete(r, q, result) }) }, nil
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

func (s *Service) complete(r storage.Record, q pricing.Quote, result proxy.AttemptResult) {
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
	if err := s.store.Complete(ctx, r); err != nil {
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
