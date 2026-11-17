package integration

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/storage/sqlite"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

const (
	okClaude    = `comporta_requests_total{app="bot",model="fake-claude",operation="generation",outcome="ok"}`
	costClaude  = `comporta_cost_microusd_total{app="bot",model="fake-claude",provider="anthropic"}`
	sentClaude  = `comporta_upstream_attempts_total{app="bot",model="fake-claude",provider="anthropic",result="ok"}`
	ttfbClaude  = `comporta_ttfb_seconds_count{app="bot",model="fake-claude"}`
	tokensInput = `comporta_tokens_total{app="bot",direction="input",model="fake-claude",provider="anthropic"}`
)

func TestSuccessStreamingAndCancellationTelemetry(t *testing.T) {
	reached := make(chan struct{}, 8)
	options := gatewayOptions{metered: true, edit: appendTelemetry(otelOn)}
	spans := observed(&options)
	gw := startConfiguredGateway(t, fakeprovider.Options{
		ByModel: map[string]fakeprovider.Scenario{"fake-gpt": fakeprovider.ScenarioSlowStream},
		Sleep: func(ctx context.Context, _ time.Duration) error {
			reached <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		},
	}, options)
	key := gw.createKey("bot").Key.Reveal()

	if response, body := gw.call("/v1/messages", key, testkit.AnthropicBody()); response.StatusCode != 200 {
		t.Fatalf("%d %s", response.StatusCode, body)
	}
	if response, body := gw.call("/v1/messages", key, testkit.AnthropicStreamBody()); response.StatusCode != 200 || !strings.Contains(body, "message_stop") {
		t.Fatalf("%d %s", response.StatusCode, body)
	}
	out := gw.waitMetric(t, okClaude, 2)
	if metric(out, sentClaude) != 2 || metric(out, ttfbClaude) != 2 {
		t.Fatalf("attempts or ttfb missing:\n%s", out)
	}
	records := gw.waitRecords(t, 2)
	var cost int64
	for _, r := range records {
		cost += int64(r.Cost)
	}
	if cost <= 0 || metric(out, costClaude) != float64(cost) {
		t.Fatalf("telemetry cost %v, metering cost %d", metric(out, costClaude), cost)
	}
	if metric(out, tokensInput) <= 0 {
		t.Fatalf("no input tokens:\n%s", out)
	}

	// A client that leaves mid-stream ends as canceled, even after its 200.
	ctx, cancel := context.WithCancel(context.Background())
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, gw.proxyURL("/v1/chat/completions"), strings.NewReader(testkit.OpenAIStreamBody()))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+key)
	var wg sync.WaitGroup
	wg.Go(func() {
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			buffer := make([]byte, 64)
			_, _ = response.Body.Read(buffer)
			_ = response.Body.Close()
		}
	})
	<-reached
	cancel()
	wg.Wait()
	out = gw.waitMetric(t, `comporta_requests_total{app="bot",model="fake-gpt",operation="generation",outcome="canceled"}`, 1)
	if metric(out, `comporta_upstream_attempts_total{app="bot",model="fake-gpt",provider="openai",result="canceled"}`) != 1 {
		t.Fatalf("canceled attempt not counted:\n%s", out)
	}

	all := gw.spans(t, spans)
	requireSafeSpans(t, all, testkit.Marker(), key, providerSecret)
	roots := names(all)["comporta.request"]
	if roots != 3 {
		t.Fatalf("%d root spans for 3 requests", roots)
	}
	canceled := names(traceWith(t, all, "canceled"))
	for _, stage := range []string{"authenticate", "validate", "identity", "route", "admit", "deliver", "settle", "upstream_attempt"} {
		if canceled[stage] != 1 {
			t.Fatalf("stage %s ran %d times: %v", stage, canceled[stage], canceled)
		}
	}
	if canceled["cache_lookup"] != 0 || canceled["fallback"] != 0 {
		t.Fatalf("stages that never ran have spans: %v", canceled)
	}
}

func TestFallbackChargesAllAttemptsOnce(t *testing.T) {
	options := gatewayOptions{edit: appendTelemetry(otelOn)}
	spans := observed(&options)
	gw := startFallbackGateway(t, "    fallback: [fake-haiku]\n", map[string]fakeprovider.Scenario{"fake-claude": fakeprovider.ScenarioServerError}, options)
	key := gw.createKey("bot").Key.Reveal()
	if response, body := gw.call("/v1/messages", key, testkit.AnthropicBody()); response.StatusCode != 200 {
		t.Fatalf("%d %s", response.StatusCode, body)
	}
	out := gw.waitMetric(t, `comporta_requests_total{app="bot",model="fake-haiku",operation="generation",outcome="fallback"}`, 1)
	if strings.Count(out, "\ncomporta_requests_total{") != 1 {
		t.Fatalf("the fallback counted more than one request:\n%s", out)
	}
	if metric(out, `comporta_upstream_attempts_total{app="bot",model="fake-claude",provider="anthropic",result="server_error"}`) != 1 ||
		metric(out, `comporta_upstream_attempts_total{app="bot",model="fake-haiku",provider="anthropic",result="ok"}`) != 1 ||
		metric(out, `comporta_fallbacks_total{app="bot",cause="500"}`) != 1 {
		t.Fatalf("attempts or transition:\n%s", out)
	}
	records := gw.waitRecords(t, 2)
	var cost int64
	for _, r := range records {
		cost += int64(r.Cost)
	}
	exposed := max(metric(out, costClaude), 0) + max(metric(out, `comporta_cost_microusd_total{app="bot",model="fake-haiku",provider="anthropic"}`), 0)
	if exposed != float64(cost) {
		t.Fatalf("telemetry cost %v, metering cost %d over %d attempts", exposed, cost, len(records))
	}

	trace := traceWith(t, gw.spans(t, spans), "fallback")
	got := names(trace)
	if got["comporta.request"] != 1 || got["chat fake-claude"] != 1 || got["upstream_attempt"] != 2 || got["fallback"] != 1 || got["settle"] != 2 {
		t.Fatalf("trace shape %v", got)
	}
	for _, s := range trace {
		if s.Name == "chat fake-claude" {
			for _, kv := range s.Attributes {
				if string(kv.Key) == "gen_ai.response.model" && kv.Value.AsString() != "fake-haiku" {
					t.Fatalf("response model %s", kv.Value.AsString())
				}
			}
		}
	}
}

func TestCachedResponseHasNoNewUsage(t *testing.T) {
	options := gatewayOptions{edit: appendTelemetry(otelOn)}
	spans := observed(&options)
	gw := startCacheGateway(t, cacheOn, fakeprovider.Options{}, options)
	key := gw.createKey("bot").Key.Reveal()
	body := zeroTemp(gptBody)
	gw.call("/v1/chat/completions", key, body)
	cost := `comporta_cost_microusd_total{app="bot",model="fake-gpt",provider="openai"}`
	out := gw.waitMetric(t, `comporta_requests_total{app="bot",model="fake-gpt",operation="generation",outcome="ok"}`, 1)
	before, tokens := metric(out, cost), metric(out, `comporta_tokens_total{app="bot",direction="output",model="fake-gpt",provider="openai"}`)
	if response, _ := gw.call("/v1/chat/completions", key, body); response.Header.Get("X-Comporta-Cache") != "hit" {
		t.Fatal("expected a hit")
	}
	out = gw.waitMetric(t, `comporta_requests_total{app="bot",model="fake-gpt",operation="generation",outcome="cached"}`, 1)
	if metric(out, `comporta_cache_hits_total{app="bot",model="fake-gpt"}`) != 1 {
		t.Fatalf("hit not counted:\n%s", out)
	}
	if metric(out, cost) != before || metric(out, `comporta_tokens_total{app="bot",direction="output",model="fake-gpt",provider="openai"}`) != tokens {
		t.Fatalf("the replay added consumption:\n%s", out)
	}
	if metric(out, `comporta_upstream_attempts_total{app="bot",model="fake-gpt",provider="openai",result="ok"}`) != 1 {
		t.Fatalf("the replay counted an upstream attempt:\n%s", out)
	}
	hit := names(traceWith(t, gw.spans(t, spans), "cached"))
	if hit["cache_lookup"] != 1 || hit["deliver"] != 1 || hit["settle"] != 1 || hit["upstream_attempt"] != 0 {
		t.Fatalf("hit trace %v", hit)
	}
}

func TestBlockedRequestWithoutUpstream(t *testing.T) {
	options := gatewayOptions{edit: appendTelemetry(otelOn)}
	spans := observed(&options)
	gw := startLimitedGateway(t, "    limits: {requests_per_minute: 1}\n", options)
	key := gw.createKey("bot").Key.Reveal()
	gw.call("/v1/messages", key, testkit.AnthropicBody())
	if response, _ := gw.call("/v1/messages", key, testkit.AnthropicBody()); response.StatusCode != 429 {
		t.Fatalf("status %d", response.StatusCode)
	}
	out := gw.waitMetric(t, `comporta_blocks_total{app="bot",scope="app",type="rate"}`, 1)
	if metric(out, `comporta_requests_total{app="bot",model="fake-claude",operation="generation",outcome="blocked"}`) != 1 || metric(out, sentClaude) != 1 {
		t.Fatalf("blocked request:\n%s", out)
	}
	// An unknown key is refused before any app is known.
	gw.call("/v1/messages", "ck_live_unknown", testkit.AnthropicBody())
	out = gw.waitMetric(t, `comporta_blocks_total{app="unauthenticated",scope="request",type="authentication"}`, 1)
	blocked := names(traceWhere(t, gw.spans(t, spans), "comporta.limit.type", "rate"))
	for _, absent := range []string{"cache_lookup", "deliver", "settle", "upstream_attempt"} {
		if blocked[absent] != 0 {
			t.Fatalf("a refused request has a %s span: %v", absent, blocked)
		}
	}
	if blocked["admit"] != 1 {
		t.Fatalf("admission span missing: %v", blocked)
	}
}

// settlementOutage fails the durable completion of the first sent attempt and
// counts every completion the coordinator asks for.
type settlementOutage struct {
	*sqlite.Store
	failed    atomic.Bool
	completed atomic.Int64
}

func (s *settlementOutage) CompleteSettled(ctx context.Context, r storage.Record, st storage.Settlement) (int64, error) {
	s.completed.Add(1)
	if r.Outcome == "ok" && s.failed.CompareAndSwap(false, true) {
		return 0, storage.ErrUnavailable
	}
	return s.Store.CompleteSettled(ctx, r, st)
}

func TestSettlementRetryDoesNotDuplicateMetrics(t *testing.T) {
	var outage *settlementOutage
	options := gatewayOptions{metered: true, wrap: func(s *sqlite.Store) storage.LimitStore {
		outage = &settlementOutage{Store: s}
		return outage
	}}
	observed(&options)
	gw := startLimitedGateway(t, "    budget: {usd_per_day: 10}\n", options)
	key := gw.createKey("bot").Key.Reveal()
	gw.call("/v1/messages", key, testkit.AnthropicBody())
	out := gw.waitMetric(t, `comporta_telemetry_failures_total{component="accounting_observer",reason="settlement_unconfirmed"}`, 1)
	// The coordinator retries the settlement on its own; the retry never reaches
	// telemetry again.
	deadline := time.Now().Add(10 * time.Second)
	for gw.limitView(nil, "apps.bot.budget.usd_per_day").Reserved != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the settlement was never retried")
		}
		time.Sleep(50 * time.Millisecond)
	}
	gw.call("/v1/messages", key, testkit.AnthropicBody())
	out = gw.waitMetric(t, okClaude, 2)
	records := gw.waitRecords(t, 2)
	if len(records) != 2 {
		t.Fatalf("records %d", len(records))
	}
	// Only the confirmed conclusion carries cost; nothing is counted twice.
	if metric(out, costClaude) != float64(records[0].Cost) && metric(out, costClaude) != float64(records[1].Cost) {
		t.Fatalf("cost %v for records %d and %d", metric(out, costClaude), records[0].Cost, records[1].Cost)
	}
	if metric(out, `comporta_telemetry_failures_total{component="accounting_observer",reason="settlement_unconfirmed"}`) != 1 {
		t.Fatalf("failure counted again:\n%s", out)
	}
}

func TestUnconfirmedSettlementIsDiagnosed(t *testing.T) {
	var outage *settlementOutage
	options := gatewayOptions{metered: true, wrap: func(s *sqlite.Store) storage.LimitStore {
		outage = &settlementOutage{Store: s}
		return outage
	}}
	observed(&options)
	gw := startLimitedGateway(t, "    budget: {usd_per_day: 10}\n", options)
	key := gw.createKey("bot").Key.Reveal()
	if response, _ := gw.call("/v1/messages", key, testkit.AnthropicBody()); response.StatusCode != 200 {
		t.Fatalf("status %d: the outage changed the answer", response.StatusCode)
	}
	out := gw.waitMetric(t, okClaude, 1)
	if metric(out, costClaude) != -1 || metric(out, tokensInput) != -1 {
		t.Fatalf("an unconfirmed settlement reported consumption:\n%s", out)
	}
	if metric(out, `comporta_telemetry_failures_total{component="accounting_observer",reason="settlement_unconfirmed"}`) != 1 {
		t.Fatalf("not diagnosed:\n%s", out)
	}
}
