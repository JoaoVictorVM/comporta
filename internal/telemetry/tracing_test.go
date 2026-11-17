package telemetry

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	otlpcollector "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/protobuf/proto"
)

const otelOn = "telemetry:\n  otel_enabled: true\n  otel_endpoint: http://127.0.0.1:4318\n"

func attr(s tracetest.SpanStub, key string) (attribute.Value, bool) {
	for _, kv := range s.Attributes {
		if string(kv.Key) == key {
			return kv.Value, true
		}
	}
	return attribute.Value{}, false
}

func byName(spans tracetest.SpanStubs, name string) tracetest.SpanStubs {
	var out tracetest.SpanStubs
	for _, s := range spans {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

// fallbackRequest plays a request whose first attempt fails and whose second
// answers, with the metering's conclusion for each attempt.
func fallbackRequest(e *env) {
	op := e.begin(OperationGeneration)
	op.Stage(StageAuthenticate).End()
	op.Authenticated("bot")
	op.Stage(StageValidate).End()
	op.Route("fake-claude", "fake-claude", "anthropic", true)
	first := op.StartAttempt("req-1", 1, "anthropic", "fake-claude")
	settle := op.StageAttempt(StageSettle, 1)
	e.service.ObserveConsumption(Consumption{App: "bot", Provider: "anthropic", Model: "fake-claude", Operation: OperationGeneration, CostMicrousd: 300,
		SettlementConfirmed: true, RequestID: "req-1", AttemptIndex: 1})
	settle.End()
	first.Finish(AttemptEnd{Sent: true, Result: AttemptServerError, Status: Some(529), ErrorType: "provider_overloaded"})
	op.StageAttempt(StageFallback, 2).End()
	op.Fallback("529")
	op.Route("fake-claude", "fake-haiku", "anthropic", true)
	second := op.StartAttempt("req-1", 2, "anthropic", "fake-haiku")
	op.StageAttempt(StageDeliver, 2).End()
	settle = op.StageAttempt(StageSettle, 2)
	e.service.ObserveConsumption(Consumption{App: "bot", Provider: "anthropic", Model: "fake-haiku", Operation: OperationGeneration, CostMicrousd: 1200,
		InputTokens: Some[int64](100), OutputTokens: Some[int64](30), SettlementConfirmed: true, UsageComplete: true, RequestID: "req-1", AttemptIndex: 2})
	settle.End()
	second.Finish(AttemptEnd{Sent: true, Result: AttemptOK, Status: Some(200), InputTokens: Some[int64](100), OutputTokens: Some[int64](30), CacheReadTokens: Some[int64](20)})
	op.Delivered(10 * time.Millisecond)
	op.End(RequestEnd{Outcome: OutcomeFallback, Status: Some(200)})
}

func TestTraceStagesAndAttempts(t *testing.T) {
	e := newEnv(t, otelOn)
	fallbackRequest(e)
	spans := e.spans()
	roots := byName(spans, rootName)
	if len(roots) != 1 || roots[0].SpanKind != trace.SpanKindServer || roots[0].Parent.IsValid() {
		t.Fatalf("roots %+v", roots)
	}
	traceID := roots[0].SpanContext.TraceID()
	for _, s := range spans {
		if s.SpanContext.TraceID() != traceID {
			t.Fatalf("span %s is in another trace", s.Name)
		}
	}
	gen := byName(spans, "chat fake-claude")
	if len(gen) != 1 || gen[0].SpanKind != trace.SpanKindClient || gen[0].Parent.SpanID() != roots[0].SpanContext.SpanID() {
		t.Fatalf("generation span %+v", gen)
	}
	if v, _ := attr(gen[0], "gen_ai.response.model"); v.AsString() != "fake-haiku" {
		t.Fatalf("response model %v", v.AsString())
	}
	if v, _ := attr(gen[0], "comporta.cost_microusd"); v.AsInt64() != 1500 {
		t.Fatalf("logical cost %d", v.AsInt64())
	}
	if v, _ := attr(gen[0], "gen_ai.usage.input_tokens"); v.AsInt64() != 120 {
		t.Fatalf("input includes cache reads once: %d", v.AsInt64())
	}
	attempts := byName(spans, attemptOp)
	if len(attempts) != 2 {
		t.Fatalf("%d attempts", len(attempts))
	}
	for _, a := range attempts {
		if a.Parent.SpanID() != gen[0].SpanContext.SpanID() {
			t.Fatal("an attempt is outside the generation span")
		}
		index, _ := attr(a, "comporta.attempt")
		cost, _ := attr(a, "comporta.cost_microusd")
		model, _ := attr(a, "gen_ai.request.model")
		switch index.AsInt64() {
		case 1:
			if cost.AsInt64() != 300 || model.AsString() != "fake-claude" || a.Status.Code.String() != "Error" {
				t.Fatalf("first attempt %+v", a.Attributes)
			}
		case 2:
			if cost.AsInt64() != 1200 || model.AsString() != "fake-haiku" {
				t.Fatalf("second attempt %+v", a.Attributes)
			}
		default:
			t.Fatalf("attempt index %d", index.AsInt64())
		}
	}
	settles := byName(spans, "settle")
	if len(settles) != 2 || len(byName(spans, "fallback")) != 1 || len(byName(spans, "cache_lookup")) != 0 {
		t.Fatalf("stages %v", spans.Snapshots())
	}
	if v, ok := attr(settles[1], "comporta.attempt"); !ok || v.AsInt64() != 2 {
		t.Fatal("a repeated stage lost its attempt number")
	}
	if e.service.attempts[attemptKey{"req-1", 2}] != nil {
		t.Fatal("the correlation outlived the attempt")
	}
}

func TestTraceDisabled(t *testing.T) {
	e := newEnv(t, "")
	fallbackRequest(e)
	if len(e.exporter) != 0 || e.service.Providers() != 0 {
		t.Fatal("a provider exists while tracing is disabled")
	}
	if len(e.exported.GetSpans()) != 0 {
		t.Fatal("spans exported while tracing is disabled")
	}
	if len(e.service.attempts) != 0 {
		t.Fatal("attempts registered without tracing")
	}
	// Metrics are independent of tracing.
	if got := sample(e.scrape(), `comporta_requests_total{app="bot",model="fake-haiku",operation="generation",outcome="fallback"}`); got != "1" {
		t.Fatalf("requests = %q", got)
	}
}

func TestAttributeAllowlist(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "host.name=leak-host,user.name=leak-user")
	t.Setenv("OTEL_SERVICE_NAME", "leak-service")
	e := newEnv(t, otelOn)
	fallbackRequest(e)
	op := e.begin(OperationGeneration)
	op.Authenticated("canary-app-from-client")
	op.Route("canary-model", "canary-model", "canary-provider", false)
	op.StartAttempt("canary-request-id", 1, "canary-provider", "canary-model").Finish(AttemptEnd{Sent: true, Result: AttemptClientError, ErrorType: "canary error with spaces"})
	op.End(RequestEnd{Outcome: Outcome("canary-outcome"), ErrorType: "canary error with spaces", Block: Block{"canary", "canary"}})
	for _, s := range e.spans() {
		for _, kv := range s.Attributes {
			if !AllowedAttributes[kv.Key] {
				t.Fatalf("span %s has %s", s.Name, kv.Key)
			}
			if strings.Contains(kv.Value.Emit(), "canary") || strings.Contains(kv.Value.Emit(), "req-1") {
				t.Fatalf("span %s attribute %s = %s", s.Name, kv.Key, kv.Value.Emit())
			}
		}
		for _, kv := range s.Resource.Attributes() {
			if strings.Contains(kv.Value.Emit(), "leak") || (kv.Key != "service.name" && kv.Key != "service.version") {
				t.Fatalf("resource %s = %s", kv.Key, kv.Value.Emit())
			}
		}
		if len(s.Events) != 0 || len(s.Links) != 0 {
			t.Fatalf("span %s has events or links", s.Name)
		}
	}
}

// TestOTLPExportIsProtobuf sends real spans through the OTLP/HTTP exporter to
// a fake collector and decodes what arrived on the network.
func TestOTLPExportIsProtobuf(t *testing.T) {
	var mu sync.Mutex
	var received []*otlpcollector.ExportTraceServiceRequest
	var paths, types []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		request := &otlpcollector.ExportTraceServiceRequest{}
		if err := proto.Unmarshal(body, request); err != nil {
			http.Error(w, "bad", http.StatusBadRequest)
			return
		}
		mu.Lock()
		received = append(received, request)
		paths = append(paths, r.URL.Path)
		types = append(types, r.Header.Get("Content-Type"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(nil)
	}))
	defer server.Close()
	e := newEnv(t, "telemetry:\n  otel_enabled: true\n  otel_endpoint: "+server.URL+"/base\n", func(o *Options) { o.Exporter = nil })
	fallbackRequest(e)
	if err := e.service.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(received) == 0 || paths[0] != "/base/v1/traces" || types[0] != "application/x-protobuf" {
		t.Fatalf("collector got %d requests, %v %v", len(received), paths, types)
	}
	var spans int
	for _, request := range received {
		for _, rs := range request.ResourceSpans {
			for _, kv := range rs.Resource.Attributes {
				if kv.Key != "service.name" && kv.Key != "service.version" {
					t.Fatalf("resource attribute %s", kv.Key)
				}
			}
			for _, ss := range rs.ScopeSpans {
				for _, s := range ss.Spans {
					spans++
					for _, kv := range s.Attributes {
						if !AllowedAttributes[attribute.Key(kv.Key)] {
							t.Fatalf("span %s exported %s", s.Name, kv.Key)
						}
					}
				}
			}
		}
	}
	if spans < 8 {
		t.Fatalf("only %d spans arrived", spans)
	}
}

func TestTracesURL(t *testing.T) {
	for in, want := range map[string]string{
		"http://jaeger:4318":             "http://jaeger:4318/v1/traces",
		"http://jaeger:4318/":            "http://jaeger:4318/v1/traces",
		"https://otel.example/base":      "https://otel.example/base/v1/traces",
		"https://otel.example/v1/traces": "https://otel.example/v1/traces",
	} {
		if got, err := TracesURL(in); err != nil || got != want {
			t.Fatalf("%s -> %s %v", in, got, err)
		}
	}
}
