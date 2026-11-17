package integration

import (
	"bytes"
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/JoaoVictorVM/comporta/internal/telemetry"
)

// otelOn enables tracing; the exporter of the tests never reaches the URL.
const otelOn = "telemetry:\n  otel_enabled: true\n  otel_endpoint: http://127.0.0.1:4318\n"

// traces records the spans and endpoints of a test gateway's exporters.
type traces struct {
	*tracetest.InMemoryExporter
	mu        sync.Mutex
	endpoints []string
}

func (x *traces) exporterFor(endpoint string) (sdktrace.SpanExporter, error) {
	x.mu.Lock()
	x.endpoints = append(x.endpoints, endpoint)
	x.mu.Unlock()
	return x.InMemoryExporter, nil
}

func (x *traces) created() []string {
	x.mu.Lock()
	defer x.mu.Unlock()
	return append([]string(nil), x.endpoints...)
}

// observed adds telemetry to the options and returns where its spans go.
func observed(options *gatewayOptions) *traces {
	x := &traces{InMemoryExporter: tracetest.NewInMemoryExporter()}
	options.telemetry = &telemetry.Options{Exporter: x.exporterFor}
	return x
}

func appendTelemetry(settings string) func(string) string {
	return func(text string) string { return text + settings }
}

func (g *gateway) scrape(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	if err := g.telemetry.WriteMetrics(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

// metric returns the numeric value of one exposed series, or -1 when absent.
func metric(exposition, series string) float64 {
	for line := range strings.SplitSeq(exposition, "\n") {
		if value, ok := strings.CutPrefix(line, series+" "); ok {
			n, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return -1
			}
			return n
		}
	}
	return -1
}

// waitMetric polls until a series reaches want: a request's telemetry ends
// after the client already read the answer.
func (g *gateway) waitMetric(t *testing.T, series string, want float64) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		out := g.scrape(t)
		if metric(out, series) == want {
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s = %v, want %v\n%s", series, metric(out, series), want, out)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (g *gateway) spans(t *testing.T, x *traces) tracetest.SpanStubs {
	t.Helper()
	if err := g.telemetry.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	return x.GetSpans()
}

// traceWith groups the spans by trace and returns the one whose root has the
// given outcome, failing unless exactly one such trace exists.
func traceWith(t *testing.T, spans tracetest.SpanStubs, outcome string) tracetest.SpanStubs {
	t.Helper()
	return traceWhere(t, spans, "comporta.outcome", outcome)
}

// traceWhere returns the only trace whose root has the attribute value.
func traceWhere(t *testing.T, spans tracetest.SpanStubs, key, value string) tracetest.SpanStubs {
	t.Helper()
	byTrace := map[string]tracetest.SpanStubs{}
	var match []string
	for _, s := range spans {
		id := s.SpanContext.TraceID().String()
		byTrace[id] = append(byTrace[id], s)
		if s.Name == "comporta.request" {
			for _, kv := range s.Attributes {
				if string(kv.Key) == key && kv.Value.Emit() == value {
					match = append(match, id)
				}
			}
		}
	}
	if len(match) != 1 {
		t.Fatalf("%d traces with %s=%s in %d spans", len(match), key, value, len(spans))
	}
	return byTrace[match[0]]
}

func names(spans tracetest.SpanStubs) map[string]int {
	out := map[string]int{}
	for _, s := range spans {
		out[s.Name]++
	}
	return out
}

// requireSafeSpans checks every attribute key against the allowlist and every
// value against the given forbidden texts.
func requireSafeSpans(t *testing.T, spans tracetest.SpanStubs, forbidden ...string) {
	t.Helper()
	for _, s := range spans {
		if s.Parent.IsRemote() {
			t.Fatalf("span %s has a remote parent", s.Name)
		}
		if len(s.Events) > 0 {
			t.Fatalf("span %s has events", s.Name)
		}
		for _, kv := range s.Attributes {
			if !telemetry.AllowedAttributes[kv.Key] {
				t.Fatalf("span %s has attribute %s outside the allowlist", s.Name, kv.Key)
			}
			for _, bad := range forbidden {
				if bad != "" && strings.Contains(kv.Value.Emit(), bad) {
					t.Fatalf("span %s attribute %s carries %q", s.Name, kv.Key, bad)
				}
			}
		}
		for _, kv := range s.Resource.Attributes() {
			if key := string(kv.Key); key != "service.name" && key != "service.version" {
				t.Fatalf("resource attribute %s", key)
			}
		}
	}
}
