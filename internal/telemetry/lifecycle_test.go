package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// stuckExporter holds every export until its context ends, like a collector
// that accepts the connection and never answers.
type stuckExporter struct {
	calls atomic.Int64
	fail  bool
}

func (x *stuckExporter) ExportSpans(ctx context.Context, _ []sdktrace.ReadOnlySpan) error {
	x.calls.Add(1)
	if x.fail {
		return errors.New("connection refused to http://secret-collector/v1/traces")
	}
	<-ctx.Done()
	return ctx.Err()
}

func (x *stuckExporter) Shutdown(context.Context) error { return nil }

func withExporter(x sdktrace.SpanExporter, queue QueueOptions) func(*Options) {
	return func(o *Options) {
		o.Exporter = func(string) (sdktrace.SpanExporter, error) { return x, nil }
		o.Queue = queue
	}
}

func simpleRequest(e *env) time.Duration {
	start := time.Now()
	op := e.begin(OperationGeneration)
	op.Authenticated("bot")
	op.Stage(StageValidate).End()
	op.End(RequestEnd{Outcome: OutcomeOK, Status: Some(200)})
	return time.Since(start)
}

func dropped(out, reason string) int {
	n, _ := strconv.Atoi(sample(out, `comporta_telemetry_dropped_spans_total{reason="`+reason+`"}`))
	return n
}

func TestExporterFailureDoesNotBlock(t *testing.T) {
	logs := &lockedBuilder{}
	exporter := &stuckExporter{fail: true}
	e := newEnv(t, otelOn, withExporter(exporter, QueueOptions{Batch: 2, ExportTimeout: 50 * time.Millisecond}), func(o *Options) {
		o.Logger = slog.New(slog.NewTextHandler(logs, nil))
	})
	for range 20 {
		if took := simpleRequest(e); took > 100*time.Millisecond {
			t.Fatalf("a request waited %v on the exporter", took)
		}
	}
	if err := e.service.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := e.scrape()
	if dropped(out, DropExportFailed) == 0 || sample(out, `comporta_telemetry_failures_total{component="tracing",reason="export_failed"}`) == "" {
		t.Fatalf("export failure not visible:\n%s", out)
	}
	// Logged at most once a minute, with fixed fields only.
	if strings.Count(logs.String(), "telemetry degraded") != 1 || strings.Contains(logs.String(), "secret-collector") || strings.Contains(logs.String(), "refused") {
		t.Fatalf("logs:\n%s", logs.String())
	}
}

func TestQueueOverflowIsReported(t *testing.T) {
	shutdownLimit = 100 * time.Millisecond
	t.Cleanup(func() { shutdownLimit = 5 * time.Second })
	exporter := &stuckExporter{}
	e := newEnv(t, otelOn, withExporter(exporter, QueueOptions{Size: 4, Batch: 1, FlushInterval: time.Hour, ExportTimeout: time.Hour}))
	simpleRequest(e)
	// The first span is taken by the export that never ends; the queue holds
	// the rest.
	for deadline := time.Now().Add(5 * time.Second); exporter.calls.Load() == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the export never started")
		}
	}
	for range 19 {
		if took := simpleRequest(e); took > 100*time.Millisecond {
			t.Fatalf("a full queue held a request for %v", took)
		}
	}
	out := e.scrape()
	// Two spans per request; one is being exported and four wait in the queue.
	if got := dropped(out, DropQueueFull); got != 40-5 {
		t.Fatalf("queue_full = %d\n%s", got, out)
	}
}

func TestReloadBoundsProviders(t *testing.T) {
	e := newEnv(t, otelOn)
	endpoint := func(n int) string {
		return "telemetry:\n  otel_enabled: true\n  otel_endpoint: http://127.0.0.1:" + strconv.Itoa(4000+n) + "\n"
	}
	oldest := e.begin(OperationGeneration)
	e.reload(endpoint(1))
	middle := e.service.Begin(e.manager.Capture(), OperationGeneration)
	if e.service.Providers() != 2 {
		t.Fatalf("providers after one reload: %d", e.service.Providers())
	}
	// Reloads keep coming while both requests are still open.
	for n := 2; n <= 6; n++ {
		e.service.Reconcile(e.reload(endpoint(n)))
		if p := e.service.Providers(); p > maxProviders {
			t.Fatalf("%d providers after reload %d", p, n)
		}
	}
	if len(e.exporter) != 7 {
		t.Fatalf("exporters %v", e.exporter)
	}
	// The oldest session was retired and goes on without producing spans; the
	// one still draining keeps its provider.
	if oldest.Stage(StageValidate) != nil {
		t.Fatal("a retired session still opens spans")
	}
	if middle.Stage(StageValidate) == nil {
		t.Fatal("the draining session lost its provider")
	}
	oldest.End(RequestEnd{Outcome: OutcomeOK})
	middle.End(RequestEnd{Outcome: OutcomeOK})
	out := e.scrape()
	if dropped(out, DropReloadRetired) == 0 || sample(out, `comporta_telemetry_failures_total{component="reload",reason="provider_retired"}`) == "" {
		t.Fatalf("retirement not reported:\n%s", out)
	}
	// Disabling keeps no provider once the last session ends.
	e.reload("telemetry:\n  otel_enabled: false\n")
	current := e.service.Begin(e.manager.Capture(), OperationGeneration)
	if current.trace != nil {
		t.Fatal("tracing after disabling")
	}
	current.End(RequestEnd{Outcome: OutcomeOK})
	if p := e.service.Providers(); p != 0 {
		t.Fatalf("%d providers after disabling", p)
	}
}

func TestShutdownDeadline(t *testing.T) {
	shutdownLimit = 200 * time.Millisecond
	t.Cleanup(func() { shutdownLimit = 5 * time.Second })
	exporter := &stuckExporter{}
	e := newEnv(t, otelOn, withExporter(exporter, QueueOptions{Batch: 1, FlushInterval: time.Hour, ExportTimeout: time.Hour}))
	for range 5 {
		simpleRequest(e)
	}
	open := e.begin(OperationGeneration)
	start := time.Now()
	e.service.Close()
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("shutdown took %v", took)
	}
	e.service.Close()
	// A request still running when the process stops ends without spans.
	open.End(RequestEnd{Outcome: OutcomeOK})
	if e.service.Begin(e.manager.Capture(), OperationGeneration).trace != nil {
		t.Fatal("tracing after close")
	}
	out := e.scrape()
	if dropped(out, DropShutdownTimeout) == 0 {
		t.Fatalf("spans lost at shutdown are not counted:\n%s", out)
	}
}

type lockedBuilder struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuilder) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuilder) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
