package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

// gauge returns a ratio series of the bot app.
func gauge(name, scope, dimension string) string {
	return fmt.Sprintf(`%s{app="bot",dimension="%s",kind="budget",scope="%s"}`, name, dimension, scope)
}

func TestLimitStateFeedsTelemetry(t *testing.T) {
	options := gatewayOptions{edit: appendTelemetry(otelOn)}
	spans := observed(&options)
	gw := startLimitedGateway(t, "    budget: {usd_per_day: 1.00}\n    per_user:\n      budget: {usd_per_day: 0.01}\n", options)
	key := gw.createKey("bot").Key.Reveal()
	first, second := testkit.UserMarker(), testkit.UserMarker()+"b"
	for _, user := range []string{first, second, second} {
		gw.callAs("/v1/messages", key, user, testkit.AnthropicBody())
	}
	gw.results.waitCount(t, 3)
	gw.telemetry.RefreshBudgets()
	out := gw.scrape(t)

	app := gw.limitView(nil, "apps.bot.budget.usd_per_day")
	if got, want := metric(out, gauge("comporta_budget_used_ratio", "app", "usd_per_day")), float64(app.Used)/float64(app.Limit); got != want || got <= 0 {
		t.Fatalf("app ratio %v, limit state %v\n%s", got, want, out)
	}
	// user_max follows the user who spent the most, never the sum of both.
	var largest float64
	for _, user := range []string{first, second} {
		v := gw.limitView(gw.userSubject(user), "apps.bot.per_user.budget.usd_per_day")
		largest = max(largest, float64(v.Used)/float64(v.Limit))
	}
	if got := metric(out, gauge("comporta_budget_used_ratio", "user_max", "usd_per_day")); got != largest {
		t.Fatalf("user max %v, want %v", got, largest)
	}
	if metric(out, `comporta_budget_users_observed{app="bot"}`) != 2 || metric(out, `comporta_limit_state_unavailable{app="bot"}`) != 0 {
		t.Fatalf("aggregate:\n%s", out)
	}
	if metric(out, gauge("comporta_budget_reserved_ratio", "app", "usd_per_day")) != 0 {
		t.Fatalf("a reservation is still reported after every request ended:\n%s", out)
	}
	for _, user := range []string{first, second} {
		if strings.Contains(out, gw.userSubject(user).Hash) {
			t.Fatal("a user hash is exposed")
		}
	}

	// Once the per-user ceiling drops below what the user spent, the refusal
	// is a block with the type and scope of the decision, in metrics and in
	// the trace.
	gw.reload(func(text string) string {
		return strings.Replace(text, "usd_per_day: 0.01", "usd_per_day: 0.000001", 1)
	})
	response, body := gw.callAs("/v1/messages", key, second, testkit.AnthropicBody())
	if response.StatusCode != 429 {
		t.Fatalf("over budget: %d %s", response.StatusCode, body)
	}
	decision := decodeLimitError(t, body).Error
	out = gw.waitMetric(t, `comporta_blocks_total{app="bot",scope="user",type="budget"}`, 1)
	if decision.Scope != "user" || decision.Code != "budget_exceeded" {
		t.Fatalf("decision %+v", decision)
	}
	blocked := traceWhere(t, gw.spans(t, spans), "comporta.limit.type", "budget")
	for _, s := range blocked {
		if s.Name == "comporta.request" {
			for _, kv := range s.Attributes {
				if string(kv.Key) == "comporta.limit.scope" && kv.Value.AsString() != decision.Scope {
					t.Fatalf("trace scope %s, decision %s", kv.Value.AsString(), decision.Scope)
				}
			}
		}
	}
}

func TestMeteringCostAndEventsFeedTelemetry(t *testing.T) {
	options := gatewayOptions{metered: true}
	observed(&options)
	gw := startConfiguredGateway(t, fakeprovider.Options{}, options)
	key := gw.createKey("bot").Key.Reveal()
	for range 3 {
		gw.call("/v1/messages", key, testkit.AnthropicBody())
	}
	gw.call("/v1/messages/count_tokens", key, testkit.CountBody())
	out := gw.waitMetric(t, okClaude, 3)
	records := gw.waitRecords(t, 4)
	var cost, input, output int64
	for _, r := range records {
		cost += int64(r.Cost)
		if r.Operation == "generation" && r.Input != nil && r.Output != nil {
			input += *r.Input
			output += *r.Output
		}
	}
	if metric(out, costClaude) != float64(cost) || metric(out, tokensInput) != float64(input) ||
		metric(out, `comporta_tokens_total{app="bot",direction="output",model="fake-claude",provider="anthropic"}`) != float64(output) {
		t.Fatalf("metering cost %d, input %d, output %d:\n%s", cost, input, output, out)
	}
	gw.waitMetric(t, `comporta_requests_total{app="bot",model="fake-claude",operation="count_tokens",outcome="ok"}`, 1)
	// The metering's completions reach the event bus asynchronously.
	if err := gw.accounting.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for metric(gw.scrape(t), `comporta_events_total{type="request_completed"}`) < 4 {
		if time.Now().After(deadline) {
			t.Fatalf("events not counted:\n%s", gw.scrape(t))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
