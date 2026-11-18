package telemetry

import (
	"errors"
	"io"
	"log/slog"
	"runtime/debug"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/expfmt"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/limits"
)

const (
	// ContentType is the exposition format of /metrics.
	ContentType = "text/plain; version=0.0.4; charset=utf-8"
	// budgetRefresh is how often the budget gauges are read from the limit
	// coordinator, outside of any scrape.
	budgetRefresh = 5 * time.Second
	// budgetMaxAge is how old a budget snapshot may be before its ratios are
	// withheld and the state is reported as unavailable.
	budgetMaxAge = 15 * time.Second
	// diagnosticInterval bounds how often one kind of telemetry failure is
	// logged.
	diagnosticInterval = time.Minute
	unknown            = "unknown"
	unauthenticated    = "unauthenticated"
)

// Source supplies the configuration in force.
type Source interface {
	Capture() *config.Snapshot
}

// LimitView is the subject-free view of the limit coordinator.
type LimitView interface {
	Aggregate([]limits.Policy) limits.Aggregate
}

// Options configures a Service.
type Options struct {
	Source Source
	// Limits feeds the budget gauges. Without it no budget series exists.
	Limits  LimitView
	Logger  *slog.Logger
	Now     func() time.Time
	Version string
	// Exporter replaces the OTLP/HTTP exporter, for tests.
	Exporter func(endpoint string) (sdktrace.SpanExporter, error)
	// Queue overrides the span queue settings, for tests. Zero values keep
	// the defaults.
	Queue QueueOptions
}

// Service is the telemetry of one process. It owns a private registry and its
// own tracer providers; nothing is registered globally.
type Service struct {
	source   Source
	limits   LimitView
	logger   *slog.Logger
	now      func() time.Time
	version  string
	exporter func(endpoint string) (sdktrace.SpanExporter, error)
	queue    QueueOptions

	f        *families
	registry *prometheus.Registry
	budgets  atomic.Pointer[budgetSnapshot]
	descs    budgetDescs

	metricsEnabled atomic.Bool

	mu         sync.Mutex
	reconciled int64
	current    *traceProvider
	draining   []*traceProvider
	closed     bool
	shutdowns  sync.WaitGroup
	attempts   map[attemptKey]*Attempt

	logMu  sync.Mutex
	logged map[string]time.Time
}

// New returns a Service. It does not start any worker; Run does.
func New(options Options) *Service {
	s := &Service{
		source: options.Source, limits: options.Limits, logger: options.Logger, now: options.Now,
		version: options.Version, exporter: options.Exporter, queue: options.Queue.withDefaults(),
		f: newFamilies(), registry: prometheus.NewRegistry(), descs: newBudgetDescs(),
		reconciled: -1, attempts: map[attemptKey]*Attempt{}, logged: map[string]time.Time{},
	}
	if s.logger == nil {
		s.logger = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.version == "" {
		s.version = buildVersion()
	}
	if s.exporter == nil {
		s.exporter = newOTLPExporter
	}
	quietOTel()
	s.metricsEnabled.Store(true)
	s.registry.MustRegister(collector{s})
	return s
}

// Register adds a collector to the scrape, for state this package does not
// own, such as the health of shared stores.
func (s *Service) Register(c prometheus.Collector) error { return s.registry.Register(c) }

// buildVersion is the module version stamped by the Go toolchain, or "dev"
// for a local build.
func buildVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

// MetricsEnabled reports whether /metrics is exposed by the configuration in
// force.
func (s *Service) MetricsEnabled() bool {
	if s == nil {
		return false
	}
	if s.source != nil {
		s.Reconcile(s.source.Capture())
	}
	return s.metricsEnabled.Load()
}

var errCollect = errors.New("metrics could not be collected")

// WriteMetrics writes the Prometheus text exposition. A collection error
// writes nothing and is counted; the caller answers with a generic failure.
func (s *Service) WriteMetrics(w io.Writer) error {
	families, err := s.registry.Gather()
	if err != nil {
		s.diagnose(ComponentMetrics, ReasonScrapeFailed)
		return errCollect
	}
	encoder := expfmt.NewEncoder(w, expfmt.NewFormat(expfmt.TypeTextPlain))
	for _, family := range families {
		if err := encoder.Encode(family); err != nil {
			return err
		}
	}
	return nil
}

// diagnose counts a telemetry failure and logs it with fixed fields at most
// once a minute per kind. Raw errors, URLs and headers are never logged.
func (s *Service) diagnose(component, reason string) {
	if s == nil || !components[component] || !failures[reason] {
		return
	}
	s.f.failures.add(1, component, reason)
	key := component + "/" + reason
	now := s.now()
	s.logMu.Lock()
	last, seen := s.logged[key]
	log := !seen || now.Sub(last) >= diagnosticInterval
	if log {
		s.logged[key] = now
	}
	s.logMu.Unlock()
	if log {
		s.logger.Warn("telemetry degraded", "category", component, "reason", reason)
	}
}

func (s *Service) dropSpans(reason string, n int) {
	if n > 0 && dropReasons[reason] {
		s.f.droppedSpans.add(int64(n), reason)
	}
}

// labelsFor keeps only names declared by the configuration; anything else is
// reported as unknown, so a label never carries text sent by a client.
func labelsFor(snapshot *config.Snapshot, app, provider, model string) (string, string, string) {
	if snapshot == nil {
		return unknown, unknown, unknown
	}
	if _, ok := snapshot.App(app); !ok {
		app = unknown
	}
	if _, ok := snapshot.Provider(provider); !ok {
		provider = unknown
	}
	if _, ok := snapshot.ProviderForModel(model); !ok {
		model = unknown
	}
	return app, provider, model
}

func (s *Service) snapshot() *config.Snapshot {
	if s.source == nil {
		return nil
	}
	return s.source.Capture()
}

// ObserveConsumption implements AccountingObserver.
func (s *Service) ObserveConsumption(c Consumption) {
	if s == nil {
		return
	}
	s.correlate(c)
	if !s.metricsEnabled.Load() {
		return
	}
	if !c.SettlementConfirmed {
		s.diagnose(ComponentAccounting, ReasonSettlementUnconfirmed)
		return
	}
	// Counting tokens is advisory and a cache replay consumed nothing: neither
	// adds cost or tokens.
	if c.Operation != OperationGeneration {
		return
	}
	app, provider, model := labelsFor(s.snapshot(), c.App, c.Provider, c.Model)
	s.f.cost.add(max(c.CostMicrousd, 0), app, provider, model)
	for _, t := range []struct {
		direction string
		value     Optional[int64]
	}{{"input", c.InputTokens}, {"output", c.OutputTokens}, {"cache_read", c.CacheReadTokens}, {"cache_write", c.CacheWriteTokens}} {
		if t.value.Known {
			s.f.tokens.add(t.value.Value, app, provider, model, t.direction)
		}
	}
	if !c.UsageComplete {
		s.f.usageMissing.add(1, app, provider, model)
	}
}

// AccountingFailure implements AccountingObserver.
func (s *Service) AccountingFailure(reason string) {
	if s == nil {
		return
	}
	s.diagnose(ComponentAccounting, reason)
}

// budgetSnapshot is an immutable copy of the budget gauges.
type budgetSnapshot struct {
	at   time.Time
	apps []budgetApp
}

type budgetApp struct {
	app         string
	unavailable bool
	users       int
	series      []budgetSeries
}

type budgetSeries struct {
	scope, dimension, kind string
	used, reserved         float64
}

// RefreshBudgets reads the limit coordinator once. A reload that lands while
// it reads discards the result; the next cycle tries again.
func (s *Service) RefreshBudgets() {
	if s.limits == nil || s.source == nil {
		return
	}
	snapshot := s.source.Capture()
	names := snapshot.AppNames()
	slices.Sort(names)
	policies := make([]limits.Policy, 0, len(names))
	for _, name := range names {
		if p, ok := limits.PolicyFor(snapshot, name); ok {
			policies = append(policies, p)
		}
	}
	aggregate := s.limits.Aggregate(policies)
	if s.source.Capture().Generation() != snapshot.Generation() {
		s.diagnose(ComponentMetrics, ReasonStaleSnapshot)
		return
	}
	out := &budgetSnapshot{at: s.now()}
	kept := 0
	omitted := 0
	for _, view := range aggregate.Apps {
		app := budgetApp{app: view.App, users: view.UsersObserved, unavailable: aggregate.Degraded}
		for _, e := range view.Entries {
			app.unavailable = app.unavailable || e.Unknown
		}
		for _, e := range view.Entries {
			if app.unavailable {
				break
			}
			if kept >= maxSeries {
				omitted++
				continue
			}
			kept++
			app.series = append(app.series, budgetSeries{scope: string(e.Scope), dimension: string(e.Dimension), kind: string(e.Kind), used: e.Used, reserved: e.Reserved})
		}
		out.apps = append(out.apps, app)
	}
	if omitted > 0 {
		s.f.labelOverflow.add(int64(omitted), namespace+"_budget_used_ratio")
	}
	s.budgets.Store(out)
}

type budgetDescs struct {
	used, reserved, users, unavailable *prometheus.Desc
}

func newBudgetDescs() budgetDescs {
	labels := []string{"app", "scope", "dimension", "kind"}
	return budgetDescs{
		used:        prometheus.NewDesc(namespace+"_budget_used_ratio", "Spent divided by the ceiling; user_max is the largest ratio of any active user.", labels, nil),
		reserved:    prometheus.NewDesc(namespace+"_budget_reserved_ratio", "Reserved divided by the ceiling, apart from what was spent.", labels, nil),
		users:       prometheus.NewDesc(namespace+"_budget_users_observed", "Users active in a window with a per-user ceiling.", []string{"app"}, nil),
		unavailable: prometheus.NewDesc(namespace+"_limit_state_unavailable", "1 when the budget state of the app is unknown or stale.", []string{"app"}, nil),
	}
}

// collector exposes the families and the budget snapshot. It never reads the
// limit coordinator or the database during a scrape.
type collector struct{ s *Service }

func (c collector) Describe(ch chan<- *prometheus.Desc) {
	for _, f := range c.s.f.all() {
		f.describe(ch)
	}
	ch <- c.s.descs.used
	ch <- c.s.descs.reserved
	ch <- c.s.descs.users
	ch <- c.s.descs.unavailable
}

func (c collector) Collect(ch chan<- prometheus.Metric) {
	for _, f := range c.s.f.all() {
		f.collect(ch)
	}
	c.collectBudgets(ch)
}

func (c collector) collectBudgets(ch chan<- prometheus.Metric) {
	s := c.s
	if s.limits == nil || s.source == nil {
		return
	}
	current := s.source.Capture()
	snapshot := s.budgets.Load()
	fresh := snapshot != nil && s.now().Sub(snapshot.at) <= budgetMaxAge
	byApp := map[string]budgetApp{}
	if fresh {
		for _, app := range snapshot.apps {
			byApp[app.app] = app
		}
	}
	for _, name := range current.AppNames() {
		app, ok := byApp[name]
		if !ok || app.unavailable {
			ch <- prometheus.MustNewConstMetric(s.descs.unavailable, prometheus.GaugeValue, 1, name)
			continue
		}
		ch <- prometheus.MustNewConstMetric(s.descs.unavailable, prometheus.GaugeValue, 0, name)
		ch <- prometheus.MustNewConstMetric(s.descs.users, prometheus.GaugeValue, float64(app.users), name)
		for _, x := range app.series {
			ch <- prometheus.MustNewConstMetric(s.descs.used, prometheus.GaugeValue, x.used, name, x.scope, x.dimension, x.kind)
			ch <- prometheus.MustNewConstMetric(s.descs.reserved, prometheus.GaugeValue, x.reserved, name, x.scope, x.dimension, x.kind)
		}
	}
}
