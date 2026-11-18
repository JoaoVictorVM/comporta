package telemetry

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRequestCountIsLogical(t *testing.T) {
	e := newEnv(t, "")
	op := e.begin(OperationGeneration)
	op.Authenticated("bot")
	op.Route("fake-claude", "fake-claude", "anthropic", false)
	first := op.StartAttempt("req-1", 1, "anthropic", "fake-claude")
	first.Finish(AttemptEnd{Sent: true, Result: AttemptServerError, Status: Some(503)})
	op.Fallback("503")
	op.Route("fake-claude", "fake-haiku", "anthropic", false)
	second := op.StartAttempt("req-1", 2, "anthropic", "fake-haiku")
	second.Finish(AttemptEnd{Sent: true, Result: AttemptOK, Status: Some(200)})
	op.End(RequestEnd{Outcome: OutcomeFallback, Status: Some(200)})
	op.End(RequestEnd{Outcome: OutcomeFallback, Status: Some(200)})

	out := e.scrape()
	if got := sample(out, `comporta_requests_total{app="bot",model="fake-haiku",operation="generation",outcome="fallback"}`); got != "1" {
		t.Fatalf("logical requests = %q\n%s", got, out)
	}
	if strings.Count(out, "comporta_requests_total{") != 1 {
		t.Fatalf("a fallback attempt became a request:\n%s", out)
	}
	if sample(out, `comporta_upstream_attempts_total{app="bot",model="fake-claude",provider="anthropic",result="server_error"}`) != "1" ||
		sample(out, `comporta_upstream_attempts_total{app="bot",model="fake-haiku",provider="anthropic",result="ok"}`) != "1" {
		t.Fatalf("attempts not counted apart:\n%s", out)
	}
	if sample(out, `comporta_fallbacks_total{app="bot",cause="503"}`) != "1" {
		t.Fatalf("fallback transition not counted:\n%s", out)
	}
}

func TestMetricLabelsBounded(t *testing.T) {
	e := newEnv(t, "")
	f := e.service.f.blocks
	for i := range maxSeries + 76 {
		f.add(1, "app-"+strconv.Itoa(i), "rate", "app")
	}
	if f.size() != maxSeries {
		t.Fatalf("family grew to %d series", f.size())
	}
	if got := f.value(overflow, overflow, overflow); got != 76 {
		t.Fatalf("overflow series = %d", got)
	}
	out := e.scrape()
	if got := sample(out, `comporta_telemetry_label_overflow_total{family="comporta_blocks_total"}`); got != "76" {
		t.Fatalf("overflow counter = %q", got)
	}
	// Totals are preserved: ordinary series plus overflow equals every add.
	var total int
	for line := range strings.SplitSeq(out, "\n") {
		if strings.HasPrefix(line, "comporta_blocks_total{") {
			n, _ := strconv.Atoi(line[strings.LastIndex(line, " ")+1:])
			total += n
		}
	}
	if total != maxSeries+76 {
		t.Fatalf("total across series = %d", total)
	}

	// Names the configuration does not declare never become labels.
	op := e.begin(OperationGeneration)
	op.Authenticated("client-chosen-app")
	op.Route("client-chosen-model", "client-chosen-model", "client-provider", false)
	op.End(RequestEnd{Outcome: OutcomeError, Status: Some(400)})
	out = e.scrape()
	if strings.Contains(out, "client-chosen") || strings.Contains(out, "client-provider") {
		t.Fatalf("unvalidated name exposed:\n%s", out)
	}
	if sample(out, `comporta_requests_total{app="unauthenticated",model="unknown",operation="generation",outcome="error"}`) != "1" {
		t.Fatalf("unvalidated request not counted under safe labels:\n%s", out)
	}
}

func TestMetricsTogglePreservesCounters(t *testing.T) {
	e := newEnv(t, "")
	done := func() {
		op := e.begin(OperationModels)
		op.Authenticated("bot")
		op.End(RequestEnd{Outcome: OutcomeOK, Status: Some(200)})
	}
	series := `comporta_requests_total{app="bot",model="unknown",operation="models",outcome="ok"}`
	done()
	e.reload("telemetry:\n  metrics_enabled: false\n")
	if e.service.MetricsEnabled() {
		t.Fatal("metrics still enabled")
	}
	done()
	e.service.ObserveConsumption(Consumption{App: "bot", Provider: "anthropic", Model: "fake-claude", Operation: OperationGeneration, CostMicrousd: 5, SettlementConfirmed: true, UsageComplete: true})
	e.reload("telemetry:\n  metrics_enabled: true\n")
	if !e.service.MetricsEnabled() {
		t.Fatal("metrics not enabled again")
	}
	out := e.scrape()
	// The value continues from before the gap, without any backfill.
	if got := sample(out, series); got != "1" {
		t.Fatalf("after re-enable = %q", got)
	}
	if strings.Contains(out, "comporta_cost_microusd_total{") {
		t.Fatal("consumption counted while metrics were disabled")
	}
	done()
	if got := sample(e.scrape(), series); got != "2" {
		t.Fatalf("counter did not continue: %q", got)
	}
}

func TestLatencyAndTTFBPresence(t *testing.T) {
	e := newEnv(t, "")
	refused := e.begin(OperationGeneration)
	refused.Authenticated("bot")
	e.clock.Advance(20 * time.Millisecond)
	refused.End(RequestEnd{Outcome: OutcomeBlocked, Status: Some(429), Block: Block{BlockRate, ScopeApp}})

	stream := e.begin(OperationGeneration)
	stream.Authenticated("bot")
	stream.Route("fake-claude", "fake-claude", "anthropic", true)
	e.clock.Advance(300 * time.Millisecond)
	stream.Delivered(300 * time.Millisecond)
	// The stream keeps going long after its first byte.
	e.clock.Advance(4 * time.Second)
	stream.End(RequestEnd{Outcome: OutcomeOK, Status: Some(200)})

	out := e.scrape()
	if got := sample(out, `comporta_ttfb_seconds_count{app="bot",model="fake-claude"}`); got != "1" {
		t.Fatalf("ttfb count = %q\n%s", got, out)
	}
	if strings.Contains(out, `comporta_ttfb_seconds_count{app="bot",model="unknown"}`) {
		t.Fatal("a refusal without delivery produced a TTFB")
	}
	if got := sample(out, `comporta_ttfb_seconds_bucket{app="bot",model="fake-claude",le="0.25"}`); got != "0" {
		t.Fatalf("ttfb 0.3s fell under 0.25: %q", got)
	}
	if got := sample(out, `comporta_request_duration_seconds_count{app="bot",operation="generation"}`); got != "2" {
		t.Fatalf("duration count = %q", got)
	}
	if got := sample(out, `comporta_request_duration_seconds_bucket{app="bot",operation="generation",le="2.5"}`); got != "1" {
		t.Fatalf("streaming duration ended at the first byte: %q", got)
	}
	if got := sample(out, `comporta_blocks_total{app="bot",scope="app",type="rate"}`); got != "1" {
		t.Fatalf("block = %q", got)
	}
}

func TestMoneyExportUsesMicrousd(t *testing.T) {
	e := newEnv(t, "")
	big := int64(1) << 53
	for _, cost := range []int64{1200, 1200, big} {
		e.service.ObserveConsumption(Consumption{App: "bot", Provider: "anthropic", Model: "fake-claude", Operation: OperationGeneration,
			CostMicrousd: cost, InputTokens: Some[int64](100), OutputTokens: Some[int64](30), SettlementConfirmed: true, UsageComplete: true})
	}
	if got := e.service.f.cost.value("bot", "anthropic", "fake-claude"); got != big+2400 {
		t.Fatalf("integer accumulator = %d", got)
	}
	e.service.ObserveConsumption(Consumption{App: "bot", Provider: "anthropic", Model: "fake-claude", Operation: OperationGeneration,
		CostMicrousd: 900, OutputTokens: Some[int64](5), SettlementConfirmed: true})
	e.service.ObserveConsumption(Consumption{App: "bot", Provider: "anthropic", Model: "fake-claude", Operation: OperationGeneration,
		CostMicrousd: 777, SettlementConfirmed: false})
	e.service.ObserveConsumption(Consumption{App: "bot", Provider: "anthropic", Model: "fake-claude", Operation: OperationCountTokens,
		CostMicrousd: 0, InputTokens: Some[int64](50), SettlementConfirmed: true, UsageComplete: true})

	out := e.scrape()
	want := fmt.Sprintf("%g", float64(big+2400+900))
	if got := sample(out, `comporta_cost_microusd_total{app="bot",model="fake-claude",provider="anthropic"}`); got != want {
		t.Fatalf("cost = %q, want %q", got, want)
	}
	if got := sample(out, `comporta_tokens_total{app="bot",direction="input",model="fake-claude",provider="anthropic"}`); got != "300" {
		t.Fatalf("input tokens = %q", got)
	}
	if strings.Contains(out, `direction="cache_read"`) {
		t.Fatal("an unknown token count was reported as zero")
	}
	if got := sample(out, `comporta_usage_missing_total{app="bot",model="fake-claude",provider="anthropic"}`); got != "1" {
		t.Fatalf("usage missing = %q", got)
	}
	if got := sample(out, `comporta_telemetry_failures_total{component="accounting_observer",reason="settlement_unconfirmed"}`); got != "1" {
		t.Fatalf("unconfirmed settlement not diagnosed: %q", got)
	}
}
