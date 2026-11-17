package telemetry

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/JoaoVictorVM/comporta/internal/config"
)

const baseConfig = `timezone: UTC
data_dir: %s
providers:
  anthropic:
    api_format: anthropic
    base_url: http://127.0.0.1:9
    models: [fake-claude, fake-haiku]
apps:
  bot:
    allowed_models: [fake-claude, fake-haiku]
%s`

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

type env struct {
	t        *testing.T
	dir      string
	path     string
	manager  *config.Manager
	clock    *clock
	service  *Service
	exported *tracetest.InMemoryExporter
	mu       sync.Mutex
	exporter []string
}

func newEnv(t *testing.T, telemetry string, options ...func(*Options)) *env {
	t.Helper()
	e := &env{t: t, dir: t.TempDir(), clock: &clock{now: time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)}, exported: tracetest.NewInMemoryExporter()}
	e.path = filepath.Join(e.dir, "comporta.yaml")
	e.write(telemetry)
	manager, err := config.NewManager(config.ManagerOptions{Path: e.path})
	if err != nil {
		t.Fatal(err)
	}
	e.manager = manager
	opts := Options{Source: manager, Now: e.clock.Now, Exporter: func(endpoint string) (sdktrace.SpanExporter, error) {
		e.mu.Lock()
		e.exporter = append(e.exporter, endpoint)
		e.mu.Unlock()
		return e.exported, nil
	}}
	for _, apply := range options {
		apply(&opts)
	}
	e.service = New(opts)
	t.Cleanup(e.service.Close)
	return e
}

func (e *env) write(telemetry string) {
	e.t.Helper()
	data := fmt.Sprintf(baseConfig, filepath.ToSlash(filepath.Join(e.dir, "data")), telemetry)
	if err := os.WriteFile(e.path, []byte(data), 0o600); err != nil {
		e.t.Fatal(err)
	}
}

// reload writes a new telemetry block and publishes it.
func (e *env) reload(telemetry string) *config.Snapshot {
	e.t.Helper()
	e.write(telemetry)
	before := e.manager.Capture().Generation()
	e.manager.Reload(context.Background())
	snapshot := e.manager.Capture()
	if snapshot.Generation() == before {
		e.t.Fatalf("reload was not applied: %+v", e.manager.Status())
	}
	return snapshot
}

func (e *env) begin(operation Operation) *Op {
	e.t.Helper()
	op := e.service.Begin(e.manager.Capture(), operation)
	if op == nil {
		e.t.Fatal("no operation")
	}
	return op
}

func (e *env) scrape() string {
	e.t.Helper()
	var buf bytes.Buffer
	if err := e.service.WriteMetrics(&buf); err != nil {
		e.t.Fatal(err)
	}
	return buf.String()
}

// sample returns the value of one exposed line, or "" when absent.
func sample(exposition, series string) string {
	for line := range strings.SplitSeq(exposition, "\n") {
		if value, ok := strings.CutPrefix(line, series+" "); ok {
			return value
		}
	}
	return ""
}

func (e *env) flush() {
	e.t.Helper()
	e.service.mu.Lock()
	providers := append([]*traceProvider(nil), e.service.draining...)
	if e.service.current != nil {
		providers = append(providers, e.service.current)
	}
	e.service.mu.Unlock()
	for _, p := range providers {
		if err := p.tp.ForceFlush(context.Background()); err != nil {
			e.t.Fatal(err)
		}
	}
}

func (e *env) spans() tracetest.SpanStubs {
	e.flush()
	return e.exported.GetSpans()
}
