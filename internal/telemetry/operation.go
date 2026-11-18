package telemetry

import (
	"sync"
	"time"

	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/JoaoVictorVM/comporta/internal/config"
)

// Op observes one logical proxy request from the route handler to its end.
// Every method is safe on a nil Op, which observes nothing. It captures the
// telemetry configuration of the snapshot it began with and keeps it until it
// ends, whatever a reload changes meanwhile.
type Op struct {
	s         *Service
	snapshot  *config.Snapshot
	metrics   bool
	operation Operation
	start     time.Time
	trace     *traceSession

	mu        sync.Mutex
	app       string
	requested string
	model     string
	provider  string
	stream    bool
	ttfb      Optional[time.Duration]
	cacheHit  bool
	cost      Optional[int64]
	input     Optional[int64]
	output    Optional[int64]
	lastEnd   time.Time
	ended     bool
}

// Begin starts observing a request that entered an allowed proxy route. It
// reconciles the telemetry configuration of the snapshot first.
func (s *Service) Begin(snapshot *config.Snapshot, operation Operation) *Op {
	if s == nil || snapshot == nil || !operations[operation] {
		return nil
	}
	s.Reconcile(snapshot)
	op := &Op{s: s, snapshot: snapshot, operation: operation, start: s.now(), app: unauthenticated, requested: unknown, model: unknown, provider: unknown}
	op.metrics = snapshot.Telemetry().MetricsEnabled
	op.trace = s.openSession(op)
	return op
}

// Authenticated records the app once its key was accepted.
func (o *Op) Authenticated(app string) {
	if o == nil {
		return
	}
	if _, ok := o.snapshot.App(app); !ok {
		return
	}
	o.mu.Lock()
	o.app = app
	o.mu.Unlock()
	o.trace.rootAttrs(attrApp(app))
}

// Route records the model asked for and the model and provider the request
// is sent to. Names absent from the configuration stay unknown.
func (o *Op) Route(requested, effective, provider string, stream bool) {
	if o == nil {
		return
	}
	o.mu.Lock()
	if _, ok := o.snapshot.ProviderForModel(requested); ok {
		o.requested = requested
	}
	if _, ok := o.snapshot.ProviderForModel(effective); ok {
		o.model = effective
	}
	if _, ok := o.snapshot.Provider(provider); ok {
		o.provider = provider
	}
	o.stream = stream
	o.mu.Unlock()
}

// Delivered records the first content that reached the client.
func (o *Op) Delivered(elapsed time.Duration) {
	if o == nil || elapsed < 0 {
		return
	}
	o.mu.Lock()
	if !o.ttfb.Known {
		o.ttfb = Some(elapsed)
	}
	o.mu.Unlock()
}

// CacheHit marks a response served from the cache.
func (o *Op) CacheHit() {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.cacheHit = true
	o.mu.Unlock()
}

// Fallback counts a transition to an admitted alternative, once per
// transition, even if that alternative later fails.
func (o *Op) Fallback(cause string) {
	if o == nil || !o.metrics || !causePattern.MatchString(cause) {
		return
	}
	o.mu.Lock()
	app := o.app
	o.mu.Unlock()
	o.s.f.fallbacks.add(1, app, cause)
}

// StageSpan is one executed pipeline step.
type StageSpan struct{ span trace.Span }

// Stage starts the span of a step that is about to run. Steps that never run
// never get a span.
func (o *Op) Stage(stage Stage) *StageSpan { return o.StageAttempt(stage, 0) }

// StageAttempt starts the span of a step repeated for a later attempt.
func (o *Op) StageAttempt(stage Stage, attempt int) *StageSpan {
	if o == nil || !stages[stage] {
		return nil
	}
	span := o.trace.stage(stage, attempt)
	if span == nil {
		return nil
	}
	return &StageSpan{span: span}
}

// End ends the step.
func (st *StageSpan) End() {
	if st != nil {
		st.span.End()
	}
}

// Attempt observes one upstream attempt of the request.
type Attempt struct {
	op       *Op
	key      attemptKey
	index    int
	provider string
	model    string
	started  time.Time
	span     trace.Span
	once     sync.Once

	mu   sync.Mutex
	cost Optional[int64]
}

type attemptKey struct {
	requestID string
	index     int
}

// StartAttempt opens the observation of an attempt about to be sent. The
// request ID only correlates it with the metering's conclusion and is never
// exported.
func (o *Op) StartAttempt(requestID string, index int, provider, model string) *Attempt {
	if o == nil {
		return nil
	}
	_, provider, model = labelsFor(o.snapshot, "", provider, model)
	a := &Attempt{op: o, key: attemptKey{requestID, index}, index: index, provider: provider, model: model, started: o.s.now()}
	a.span = o.trace.attempt(o, a)
	if a.span != nil && requestID != "" {
		o.s.mu.Lock()
		o.s.attempts[a.key] = a
		o.s.mu.Unlock()
	}
	return a
}

// correlate attaches the settled cost and tokens to the attempt span still
// open for that conclusion. Without tracing nothing is registered.
func (s *Service) correlate(c Consumption) {
	s.mu.Lock()
	a := s.attempts[attemptKey{c.RequestID, c.AttemptIndex}]
	s.mu.Unlock()
	if a == nil || !c.SettlementConfirmed || c.Operation != OperationGeneration {
		return
	}
	a.mu.Lock()
	a.cost = Some(max(c.CostMicrousd, 0))
	a.mu.Unlock()
	o := a.op
	o.mu.Lock()
	o.cost = Some(o.cost.Value + max(c.CostMicrousd, 0))
	o.mu.Unlock()
}

// Finish ends the attempt with what the proxy observed. Metrics count only
// attempts actually sent.
func (a *Attempt) Finish(end AttemptEnd) {
	if a == nil {
		return
	}
	a.once.Do(func() {
		o := a.op
		if a.span != nil {
			o.s.mu.Lock()
			delete(o.s.attempts, a.key)
			o.s.mu.Unlock()
		}
		ended := end.Ended
		if ended.IsZero() || ended.Before(a.started) {
			ended = o.s.now()
		}
		input := gaiInput(end)
		o.mu.Lock()
		if input.Known {
			o.input = Some(o.input.Value + input.Value)
		}
		if end.OutputTokens.Known {
			o.output = Some(o.output.Value + end.OutputTokens.Value)
		}
		if ended.After(o.lastEnd) {
			o.lastEnd = ended
		}
		app := o.app
		o.mu.Unlock()
		if end.Sent && o.metrics && attempts[end.Result] {
			o.s.f.upstreamAttempts.add(1, app, a.provider, a.model, string(end.Result))
			o.s.f.upstreamDuration.observe(ended.Sub(a.started).Seconds(), a.provider, a.model)
		}
		a.mu.Lock()
		cost := a.cost
		a.mu.Unlock()
		o.trace.finishAttempt(a, end, input, cost, ended)
	})
}

// gaiInput adds the input categories once each, as the GenAI attribute
// expects; unknown stays unknown.
func gaiInput(end AttemptEnd) Optional[int64] {
	var total Optional[int64]
	for _, v := range []Optional[int64]{end.InputTokens, end.CacheReadTokens, end.CacheWriteTokens} {
		if v.Known && v.Value >= 0 {
			total = Some(total.Value + v.Value)
		}
	}
	return total
}

// End concludes the request. It runs once, after the operation's own cleanup.
func (o *Op) End(end RequestEnd) {
	if o == nil {
		return
	}
	o.mu.Lock()
	if o.ended {
		o.mu.Unlock()
		return
	}
	o.ended = true
	app, model, cacheHit, ttfb := o.app, o.model, o.cacheHit, o.ttfb
	o.mu.Unlock()
	outcome := end.Outcome
	if !outcomes[outcome] {
		outcome = OutcomeError
	}
	if o.metrics {
		f := o.s.f
		duration := o.s.now().Sub(o.start)
		f.requests.add(1, app, model, string(o.operation), string(outcome))
		f.requestDuration.observe(duration.Seconds(), app, string(o.operation))
		if ttfb.Known {
			f.ttfb.observe(ttfb.Value.Seconds(), app, model)
		}
		if blockTypes[end.Block.Type] && blockScopes[end.Block.Scope] {
			f.blocks.add(1, app, string(end.Block.Type), string(end.Block.Scope))
		}
		if cacheHit && outcome == OutcomeCached {
			f.cacheHits.add(1, app, model)
		}
	}
	o.trace.end(o, end, outcome)
	o.s.closeSession(o.trace)
}

func spanStatus(span trace.Span, failed bool) {
	if failed {
		span.SetStatus(codes.Error, "")
	}
}
