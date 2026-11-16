package integration

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

func usd(microusd int64) string {
	return fmt.Sprintf("%d.%06d", microusd/1_000_000, microusd%1_000_000)
}

// reservationOf measures what one attempt on fake-sonnet reserves for the
// AnthropicBody, with a probe user that no later budget applies to.
func (g *gateway) reservationOf(t *testing.T, key string) int64 {
	t.Helper()
	response, _ := g.callAs("/v1/messages", key, "probe-user", modelBody(testkit.AnthropicBody(), "fake-sonnet"))
	if response.StatusCode != 200 {
		t.Fatalf("probe %d", response.StatusCode)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, r := range g.records(t) {
			if r.ModelUsed == "fake-sonnet" {
				return int64(r.ReservedCost)
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("no probe record")
	return 0
}

func TestFallbackReservesEachModelAndCountsRequestOnce(t *testing.T) {
	gw := startFallbackGateway(t, "    limits: {requests_per_minute: 1}\n    fallback: [fake-haiku]\n", map[string]fakeprovider.Scenario{"fake-claude": fakeprovider.ScenarioOverload}, gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	// One request per minute: the fallback is the same request, not a second one.
	if response, body := gw.call("/v1/messages", key, testkit.AnthropicBody()); response.StatusCode != 200 {
		t.Fatalf("%d %s", response.StatusCode, body)
	}
	if response, _ := gw.call("/v1/messages", key, testkit.AnthropicBody()); response.StatusCode != 429 {
		t.Fatalf("a second request got through: %d", response.StatusCode)
	}
	gw.results.waitCount(t, 2)
	var first, second storage.Record
	for _, r := range gw.records(t) {
		switch {
		case r.AttemptIndex == 1 && r.ModelUsed == "fake-claude" && r.FallbackFrom == "" && r.Outcome != "blocked":
			first = r
		case r.AttemptIndex == 2:
			second = r
		}
	}
	// Each attempt reserved with its own price: output costs 6 for fake-claude
	// and 1 for fake-haiku per million tokens.
	if first.Quote.Price.Output != 6*second.Quote.Price.Output || first.ReservedCost <= second.ReservedCost || second.ReservedCost == 0 {
		t.Fatalf("quotes %+v / %+v", first.Quote, second.Quote)
	}
	if first.Cost != first.ReservedCost || !first.UsageMissing {
		t.Fatalf("failed attempt charged %d of %d", first.Cost, first.ReservedCost)
	}
	if err := gw.accounting.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := gw.usage.Daily(context.Background(), storage.Filter{AppName: "bot"})
	if err != nil || len(rows) != 1 || rows[0].Requests != 1 || rows[0].Attempts != 2 || rows[0].Cost != first.Cost+second.Cost {
		t.Fatalf("daily %+v %v", rows, err)
	}
	if v := gw.limitView(nil, "apps.bot.limits.requests_per_minute"); v.Used != 1 {
		t.Fatalf("requests per minute %+v", v)
	}
}

func TestFallbackPolicyMatrix(t *testing.T) {
	failing := map[string]fakeprovider.Scenario{"fake-claude": fakeprovider.ScenarioOverload, "fake-haiku": fakeprovider.ScenarioOverload}
	// Each case: the bot's settings, the user's limit in multiples of one
	// attempt's reservation, the chain, the expected answer and models sent.
	cases := []struct {
		name, settings, limit, chain string
		status                       int
		code                         string
		sent                         string
	}{
		{"block", "    on_limit: block\n", "budget", "[fake-sonnet]", 429, "budget_exceeded", "[fake-claude]"},
		{"warn", "    on_limit: warn\n", "budget", "[fake-sonnet]", 200, "", "[fake-claude fake-sonnet]"},
		{"warn under hard cap", "    on_limit: warn\n", "hard_cap", "[fake-sonnet]", 429, "hard_cap_exceeded", "[fake-claude]"},
		{"downgrade to a failed target", "    on_limit: downgrade\n    downgrade_to: fake-haiku\n", "budget", "[fake-haiku, fake-sonnet]", 503, "fallback_policy_target_exhausted", "[fake-claude fake-haiku]"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gw := startFallbackGateway(t, c.settings+"    fallback: "+c.chain+"\n    per_user:\n      budget: {usd_per_day: 100}\n", failing, gatewayOptions{})
			key := gw.createKey("bot").Key.Reveal()
			reserve := gw.reservationOf(t, key)
			gw.sentModels(t, 1)
			// One attempt fits, two do not: the first model is admitted, its failure
			// is charged at its reservation, and the policy decides the next one.
			ceiling := "budget: {usd_per_day: " + usd(reserve*3/2) + "}"
			if c.limit == "hard_cap" {
				ceiling = "budget: {usd_per_day: " + usd(reserve/2) + "}\n      hard_cap: {usd_per_day: " + usd(reserve*3/2) + "}"
			}
			gw.reload(func(text string) string {
				return strings.Replace(text, "budget: {usd_per_day: 100}", ceiling, 1)
			})
			response, body := gw.callAs("/v1/messages", key, testkit.UserMarker(), testkit.AnthropicBody())
			if response.StatusCode != c.status || (c.code != "" && decodeLimitError(t, body).Error.Code != c.code) {
				t.Fatalf("%d %s", response.StatusCode, body)
			}
			if got := fmt.Sprint(gw.sentModels(t, strings.Count(c.sent, " ")+1)); got != c.sent {
				t.Fatalf("sent %s", got)
			}
			if c.status == 200 {
				return
			}
			// The refusal ends the request: the last attempt sent is its terminal
			// record, with the refusal as the answer, and nothing is recorded as a
			// late block.
			gw.results.waitCount(t, 1+strings.Count(c.sent, " ")+1)
			terminal := 0
			for _, r := range gw.records(t) {
				if r.RequestCount == 1 && r.ModelUsed != "fake-sonnet" {
					terminal++
					if r.TerminalErrorCode != c.code {
						t.Fatalf("terminal %+v", r)
					}
				}
			}
			if terminal != 1 || len(gw.policyEvents(t, events.Blocked)) != 0 {
				t.Fatalf("terminal records %d", terminal)
			}
		})
	}
}

func TestFallbackConcurrentBudgetSafety(t *testing.T) {
	for _, mode := range []string{"fail_closed", "fail_open"} {
		t.Run(mode, func(t *testing.T) {
			gw := startFallbackGateway(t, "    fail_mode: "+mode+"\n    fallback: [fake-sonnet]\n    budget: {usd_per_day: 100}\n", map[string]fakeprovider.Scenario{"fake-claude": fakeprovider.ScenarioOverload}, gatewayOptions{})
			key := gw.createKey("bot").Key.Reveal()
			reserve := gw.reservationOf(t, key)
			probe := gw.dailyCost(storage.Filter{})
			limit := probe + 40*reserve
			gw.reload(func(text string) string {
				return strings.Replace(text, "budget: {usd_per_day: 100}", "budget: {usd_per_day: "+usd(limit)+"}", 1)
			})
			const operations = 500
			statuses := make(chan int, operations)
			var wg sync.WaitGroup
			gate := make(chan struct{}, 50)
			for range operations {
				wg.Add(1)
				gate <- struct{}{}
				go func() {
					defer wg.Done()
					defer func() { <-gate }()
					response, _ := gw.call("/v1/messages", key, testkit.AnthropicBody())
					statuses <- response.StatusCode
				}()
			}
			wg.Wait()
			close(statuses)
			answered := 0
			for status := range statuses {
				switch status {
				case 200:
					answered++
				case 429:
				default:
					t.Fatalf("unexpected status %d", status)
				}
			}
			if answered == 0 {
				t.Fatal("no operation fell back successfully")
			}
			// Everything settles: no reservation left, spend within the budget,
			// one terminal record per logical request.
			deadline := time.Now().Add(10 * time.Second)
			for gw.limitView(nil, "apps.bot.budget.usd_per_day").Reserved != 0 && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			view := gw.limitView(nil, "apps.bot.budget.usd_per_day")
			if view.Reserved != 0 || view.Used > limit {
				t.Fatalf("budget %+v of %d", view, limit)
			}
			if err := gw.accounting.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			terminal := map[string]int{}
			filter := storage.RecordFilter{AppName: "bot", Limit: 1000}
			for {
				page, err := gw.usage.Records(context.Background(), filter)
				if err != nil {
					t.Fatal(err)
				}
				for _, r := range page.Records {
					terminal[r.RequestID] += r.RequestCount
				}
				if page.Next == nil {
					break
				}
				filter.After = page.Next
			}
			for id, n := range terminal {
				if n > 1 {
					t.Fatalf("request %s counted %d times", id, n)
				}
			}
			if cost := gw.dailyCost(storage.Filter{}); cost > limit {
				t.Fatalf("daily cost %d over %d", cost, limit)
			}
		})
	}
}
