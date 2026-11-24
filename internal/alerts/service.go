package alerts

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// Lease and cadence of the worker.
const (
	ClaimLease      = 30 * time.Second
	DefaultPoll     = time.Second
	BudgetSweep     = time.Minute
	RetentionPeriod = time.Hour
	ShutdownGrace   = 10 * time.Second
	MaxConcurrent   = 8
)

// ConfigSource hands out the current snapshot.
type ConfigSource interface {
	Capture() *config.Snapshot
}

// Metrics receives the closed categories of the worker. Every method must be
// cheap and safe for concurrent use.
type Metrics interface {
	AlertOccurrences(trigger string, n int)
	AlertDelivery(channelType, outcome, category string)
	AlertBacklog(storage.AlertBacklog)
	AlertStoreFailure()
}

type noMetrics struct{}

func (noMetrics) AlertOccurrences(string, int)         {}
func (noMetrics) AlertDelivery(string, string, string) {}
func (noMetrics) AlertBacklog(storage.AlertBacklog)    {}
func (noMetrics) AlertStoreFailure()                   {}

type Options struct {
	Store  storage.AlertStore
	Source ConfigSource
	// Bus only wakes the worker early. Losing a notification delays an alert
	// until the next poll; it never loses it.
	Bus    *events.Bus
	Logger *slog.Logger
	// Now is the civil clock; tests inject one.
	Now func() time.Time
	// Client sends the deliveries. Without it a restricted client is built.
	Client *http.Client
	// Version goes into the User-Agent of every delivery.
	Version string
	Metrics Metrics
	// Poll is the cadence of claims; zero uses DefaultPoll.
	Poll time.Duration
	// After schedules retries and waits; tests replace it to avoid real sleeps.
	After func(time.Duration) <-chan time.Time
}

// Service is the supervised alert worker of one process. Several processes may
// share one store: claims and unique keys keep them from repeating work.
type Service struct {
	o         Options
	evaluator Evaluator
	client    *http.Client
	logger    *slog.Logger
	metrics   Metrics

	mu       sync.Mutex
	healthy  bool
	warnedAt time.Time
}

func New(o Options) *Service {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = func() time.Time { return time.Now().UTC() }
	}
	if o.Poll <= 0 {
		o.Poll = DefaultPoll
	}
	if o.After == nil {
		o.After = time.After
	}
	metrics := o.Metrics
	if metrics == nil {
		metrics = noMetrics{}
	}
	client := o.Client
	if client == nil {
		client = NewClient()
	}
	if o.Version == "" {
		o.Version = buildVersion()
	}
	return &Service{o: o, evaluator: Evaluator{Store: o.Store}, client: client, logger: o.Logger, metrics: metrics, healthy: true}
}

// Healthy reports whether the last store operation of the worker succeeded.
func (s *Service) Healthy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.healthy
}

// storeFailed marks the worker unhealthy and logs at most once a minute. The
// error itself is never logged: only its category.
func (s *Service) storeFailed(stage string, err error) {
	s.metrics.AlertStoreFailure()
	s.mu.Lock()
	s.healthy = false
	now := s.o.Now()
	log := now.Sub(s.warnedAt) >= time.Minute
	if log {
		s.warnedAt = now
	}
	s.mu.Unlock()
	if log {
		s.logger.Warn("alert store unavailable; alerts wait for it to recover", "stage", stage, "category", errorCategory(err))
	}
}

func (s *Service) storeRecovered() {
	s.mu.Lock()
	s.healthy = true
	s.mu.Unlock()
}

func errorCategory(err error) string {
	switch {
	case errors.Is(err, storage.ErrAlertBacklogFull):
		return "backlog_full"
	case errors.Is(err, storage.ErrFull):
		return "disk_full"
	case errors.Is(err, storage.ErrInvalid):
		return "invalid"
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return "canceled"
	}
	return "unavailable"
}

// ProcessInputs claims a batch of inputs and turns each into occurrences and
// deliveries. It returns how many inputs were finished.
func (s *Service) ProcessInputs(ctx context.Context) (int, error) {
	inputs, err := s.o.Store.ClaimAlertInputs(ctx, s.o.Now(), ClaimLease, storage.MaxAlertBatch)
	if err != nil {
		s.storeFailed("claim_inputs", err)
		return 0, err
	}
	done := 0
	for i, in := range inputs {
		err := s.processInput(ctx, in)
		if err == nil {
			done++
			continue
		}
		// Whatever was not finished goes back to pending at once instead of
		// waiting for its lease.
		for _, rest := range inputs[i:] {
			_ = s.o.Store.ReleaseAlertInput(context.WithoutCancel(ctx), rest.EventID, rest.ClaimToken)
		}
		if !errors.Is(err, storage.ErrClaimLost) {
			s.storeFailed("process_input", err)
		}
		return done, err
	}
	s.storeRecovered()
	return done, nil
}

// processInput captures the configuration before deriving and checks it again
// before committing, so no delivery is expanded for a set of channels that
// was replaced in between.
func (s *Service) processInput(ctx context.Context, in storage.AlertInput) error {
	for attempt := 0; ; attempt++ {
		snapshot := s.o.Source.Capture()
		occurrences, err := s.evaluator.Input(ctx, snapshot, in)
		if err != nil {
			return err
		}
		if s.o.Source.Capture().Generation() != snapshot.Generation() && attempt < 3 {
			continue
		}
		if err = s.o.Store.CompleteAlertInput(ctx, in.EventID, in.ClaimToken, occurrences, ChannelRefs(snapshot.Alerts()), s.o.Now()); err != nil {
			return err
		}
		s.count(occurrences)
		return nil
	}
}

func (s *Service) count(occurrences []storage.AlertOccurrence) {
	byTrigger := map[string]int{}
	for _, o := range occurrences {
		byTrigger[o.Trigger]++
	}
	for trigger, n := range byTrigger {
		s.metrics.AlertOccurrences(trigger, n)
	}
}

// record stores the occurrences of a scheduled evaluation.
func (s *Service) record(ctx context.Context, stage string, derive func(*config.Snapshot) ([]storage.AlertOccurrence, error)) error {
	for attempt := 0; ; attempt++ {
		snapshot := s.o.Source.Capture()
		occurrences, err := derive(snapshot)
		if err != nil {
			s.storeFailed(stage, err)
			return err
		}
		if s.o.Source.Capture().Generation() != snapshot.Generation() && attempt < 3 {
			continue
		}
		if err = s.o.Store.RecordAlertOccurrences(ctx, occurrences, ChannelRefs(snapshot.Alerts()), s.o.Now()); err != nil {
			s.storeFailed(stage, err)
			return err
		}
		s.count(occurrences)
		return nil
	}
}

// EvaluateBudgets sweeps the app scope budgets at the current instant.
func (s *Service) EvaluateBudgets(ctx context.Context) error {
	at := s.o.Now()
	return s.record(ctx, "budgets", func(snapshot *config.Snapshot) ([]storage.AlertOccurrence, error) {
		return s.evaluator.Budgets(ctx, snapshot, at)
	})
}

// EvaluateAnomalies runs the anomaly rule at an instant aligned to five minutes.
func (s *Service) EvaluateAnomalies(ctx context.Context, at time.Time) error {
	return s.record(ctx, "anomaly", func(snapshot *config.Snapshot) ([]storage.AlertOccurrence, error) {
		return s.evaluator.Anomalies(ctx, snapshot, at)
	})
}

// Run is the supervised worker: it keeps running until ctx ends, then lets
// deliveries in flight finish within ShutdownGrace and returns the rest of
// its claims to pending. Store failures make it unhealthy, never fatal.
func (s *Service) Run(ctx context.Context) error {
	wake := make(chan struct{}, 1)
	if s.o.Bus != nil {
		sub := s.o.Bus.Subscribe(ctx, storage.AlertInputTypes...)
		go func() {
			for range sub.Events {
				select {
				case wake <- struct{}{}:
				default:
				}
			}
		}()
	}
	pool := newPool(MaxConcurrent)
	defer pool.shutdown(s.o.After, ShutdownGrace)
	nextAnomaly := s.o.Now().Truncate(AnomalyStep).Add(AnomalyStep)
	var nextBudget, nextRetention time.Time
	for {
		now := s.o.Now()
		if !now.Before(nextBudget) {
			_ = s.EvaluateBudgets(ctx)
			nextBudget = now.Add(BudgetSweep)
		}
		if !now.Before(nextAnomaly) {
			_ = s.EvaluateAnomalies(ctx, nextAnomaly)
			nextAnomaly = now.Truncate(AnomalyStep).Add(AnomalyStep)
		}
		if !now.Before(nextRetention) {
			if err := s.o.Store.RetainAlerts(ctx, now); err != nil && ctx.Err() == nil {
				s.storeFailed("retention", err)
			}
			nextRetention = now.Add(RetentionPeriod)
		}
		for ctx.Err() == nil {
			n, err := s.ProcessInputs(ctx)
			if err != nil || n < storage.MaxAlertBatch {
				break
			}
		}
		s.deliverDue(ctx, pool)
		if backlog, err := s.o.Store.AlertBacklog(ctx, now); err == nil {
			s.metrics.AlertBacklog(backlog)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-wake:
		case <-pool.idle():
		case <-s.o.After(s.o.Poll):
		}
	}
}
