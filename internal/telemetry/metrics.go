package telemetry

import (
	"strings"
	"sync"

	"github.com/prometheus/client_golang/prometheus"
)

const (
	namespace = "comporta"
	// maxSeries bounds the ordinary label combinations of each family for the
	// life of the process, including names that a reload later removed.
	maxSeries = 1024
	overflow  = "overflow"
)

// durationBuckets are in seconds. TTFB stops at 60.
var (
	durationBuckets = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}
	ttfbBuckets     = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}
)

type kind int

const (
	kindCounter kind = iota
	kindHistogram
)

// series is one label combination. Counters accumulate integers, so money in
// microdollars stays exact until the exposition converts it.
type series struct {
	values  []string
	total   int64
	buckets []uint64
	sum     float64
	count   uint64
}

// family is a metric with a bounded set of label combinations. Once the bound
// is reached, new combinations are added to one reserved series whose labels
// are all "overflow", which keeps every total while memory stays fixed.
type family struct {
	name       string
	desc       *prometheus.Desc
	kind       kind
	labels     []string
	buckets    []float64
	onOverflow func(name string)

	mu       sync.Mutex
	series   map[string]*series
	ordered  []*series
	overflow *series
}

func newFamily(name, help string, k kind, buckets []float64, labels ...string) *family {
	return &family{
		name:    namespace + "_" + name,
		desc:    prometheus.NewDesc(namespace+"_"+name, help, labels, nil),
		kind:    k,
		labels:  labels,
		buckets: buckets,
		series:  map[string]*series{},
	}
}

func (f *family) entry(values []string) *series {
	key := strings.Join(values, "\xff")
	if s, ok := f.series[key]; ok {
		return s
	}
	if len(f.series) >= maxSeries {
		if f.overflow == nil {
			reserved := make([]string, len(f.labels))
			for i := range reserved {
				reserved[i] = overflow
			}
			f.overflow = f.newSeries(reserved)
		}
		if f.onOverflow != nil {
			f.onOverflow(f.name)
		}
		return f.overflow
	}
	s := f.newSeries(append([]string(nil), values...))
	f.series[key] = s
	f.ordered = append(f.ordered, s)
	return s
}

func (f *family) newSeries(values []string) *series {
	s := &series{values: values}
	if f.kind == kindHistogram {
		s.buckets = make([]uint64, len(f.buckets))
	}
	return s
}

// add increments a counter. Negative values are ignored: counters only grow.
func (f *family) add(value int64, labels ...string) {
	if value < 0 || len(labels) != len(f.labels) {
		return
	}
	f.mu.Lock()
	s := f.entry(labels)
	if s.total > maxInt64-value {
		s.total = maxInt64
	} else {
		s.total += value
	}
	f.mu.Unlock()
}

// touch creates a series at zero, for closed sets that dashboards expect to
// exist before the first event.
func (f *family) touch(labels ...string) { f.add(0, labels...) }

// observe records a duration in seconds. Negative values are never recorded.
func (f *family) observe(seconds float64, labels ...string) {
	if seconds < 0 || len(labels) != len(f.labels) {
		return
	}
	f.mu.Lock()
	s := f.entry(labels)
	for i, upper := range f.buckets {
		if seconds <= upper {
			s.buckets[i]++
		}
	}
	s.sum += seconds
	s.count++
	f.mu.Unlock()
}

const maxInt64 = int64(^uint64(0) >> 1)

func (f *family) describe(ch chan<- *prometheus.Desc) { ch <- f.desc }

func (f *family) collect(ch chan<- prometheus.Metric) {
	f.mu.Lock()
	all := f.ordered
	if f.overflow != nil {
		all = append(all[:len(all):len(all)], f.overflow)
	}
	metrics := make([]prometheus.Metric, 0, len(all))
	for _, s := range all {
		var metric prometheus.Metric
		var err error
		if f.kind == kindCounter {
			metric, err = prometheus.NewConstMetric(f.desc, prometheus.CounterValue, float64(s.total), s.values...)
		} else {
			cumulative := make(map[float64]uint64, len(f.buckets))
			for i, upper := range f.buckets {
				cumulative[upper] = s.buckets[i]
			}
			metric, err = prometheus.NewConstHistogram(f.desc, s.count, s.sum, cumulative, s.values...)
		}
		if err == nil {
			metrics = append(metrics, metric)
		}
	}
	f.mu.Unlock()
	for _, m := range metrics {
		ch <- m
	}
}

// value returns the counter total of one combination, for tests.
func (f *family) value(labels ...string) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.series[strings.Join(labels, "\xff")]; ok {
		return s.total
	}
	if f.overflow != nil && strings.Join(f.overflow.values, "\xff") == strings.Join(labels, "\xff") {
		return f.overflow.total
	}
	return 0
}

// histogramCount returns how many observations one combination holds.
func (f *family) histogramCount(labels ...string) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.series[strings.Join(labels, "\xff")]; ok {
		return s.count
	}
	return 0
}

// size is the number of ordinary combinations.
func (f *family) size() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.series)
}

// families holds every metric the gateway exposes.
type families struct {
	requests         *family
	requestDuration  *family
	ttfb             *family
	upstreamAttempts *family
	upstreamDuration *family
	cost             *family
	tokens           *family
	usageMissing     *family
	blocks           *family
	cacheHits        *family
	fallbacks        *family
	events           *family
	eventsLost       *family
	failures         *family
	droppedSpans     *family
	labelOverflow    *family
}

func newFamilies() *families {
	f := &families{
		requests:         newFamily("requests_total", "Logical proxy requests by terminal outcome; fallback attempts are not counted again.", kindCounter, nil, "app", "model", "operation", "outcome"),
		requestDuration:  newFamily("request_duration_seconds", "Time from entering the route handler until the request ended, streaming included.", kindHistogram, durationBuckets, "app", "operation"),
		ttfb:             newFamily("ttfb_seconds", "Time until the first content reached the client; absent when nothing was delivered.", kindHistogram, ttfbBuckets, "app", "model"),
		upstreamAttempts: newFamily("upstream_attempts_total", "Attempts actually sent to a provider.", kindCounter, nil, "app", "provider", "model", "result"),
		upstreamDuration: newFamily("upstream_duration_seconds", "Duration of each attempt sent to a provider.", kindHistogram, durationBuckets, "provider", "model"),
		cost:             newFamily("cost_microusd_total", "Estimated cost confirmed by the metering, in microdollars, summing every attempt.", kindCounter, nil, "app", "provider", "model"),
		tokens:           newFamily("tokens_total", "Tokens confirmed by the metering in disjoint directions.", kindCounter, nil, "app", "provider", "model", "direction"),
		usageMissing:     newFamily("usage_missing_total", "Attempts concluded without complete usage; their cost is the conservative floor.", kindCounter, nil, "app", "provider", "model"),
		blocks:           newFamily("blocks_total", "Terminal refusals by primary reason.", kindCounter, nil, "app", "type", "scope"),
		cacheHits:        newFamily("cache_hits_total", "Responses served from the response cache.", kindCounter, nil, "app", "model"),
		fallbacks:        newFamily("fallbacks_total", "Transitions to an admitted fallback model, even if it later failed.", kindCounter, nil, "app", "cause"),
		events:           newFamily("events_total", "Operational events received by this process; best effort diagnostics, not financial counters.", kindCounter, nil, "type"),
		eventsLost:       newFamily("event_notifications_lost_total", "Event notifications this process failed to receive.", kindCounter, nil),
		failures:         newFamily("telemetry_failures_total", "Failures of the telemetry itself.", kindCounter, nil, "component", "reason"),
		droppedSpans:     newFamily("telemetry_dropped_spans_total", "Spans discarded before reaching the collector.", kindCounter, nil, "reason"),
		labelOverflow:    newFamily("telemetry_label_overflow_total", "Observations aggregated into the overflow series of a family.", kindCounter, nil, "family"),
	}
	for _, x := range f.all() {
		if x != f.labelOverflow {
			x.onOverflow = func(name string) { f.labelOverflow.add(1, name) }
		}
	}
	f.eventsLost.touch()
	for reason := range dropReasons {
		f.droppedSpans.touch(reason)
	}
	return f
}

func (f *families) all() []*family {
	return []*family{f.requests, f.requestDuration, f.ttfb, f.upstreamAttempts, f.upstreamDuration, f.cost, f.tokens, f.usageMissing,
		f.blocks, f.cacheHits, f.fallbacks, f.events, f.eventsLost, f.failures, f.droppedSpans, f.labelOverflow}
}
