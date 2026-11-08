package integration

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/limits"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/storage/sqlite"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

// previousUsage writes usage as a release without limits did: journal, result
// and batch, with no reservation and no checkpoint.
func previousUsage(t *testing.T, records ...storage.Record) func(string) {
	return func(dir string) {
		store, err := sqlite.Open(context.Background(), dir)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()
		ctx := context.Background()
		for _, r := range records {
			if err = store.Admit(ctx, r); err != nil {
				t.Fatal(err)
			}
		}
		if err = store.Commit(ctx, records); err != nil {
			t.Fatal(err)
		}
		if _, err = store.Records(ctx, storage.RecordFilter{AppName: "bot", Limit: 1}); err != nil {
			t.Fatal(err)
		}
	}
}

func historical(n int, at time.Time, timezone string, cost int64, tokens *int64) storage.Record {
	output := int64(0)
	r := storage.Record{RequestID: fmt.Sprintf("%032x", n), AttemptIndex: 1, StartedAt: at, FinishedAt: at, AppName: "bot", UserHash: fmt.Sprintf("%032x", 1000+n),
		Operation: "generation", Provider: "openai", ModelRequested: "fake-gpt", ModelUsed: "fake-gpt", Outcome: "ok", RequestCount: 1, ReservedCost: 100,
		Day: at.In(location(timezone)).Format("2006-01-02"), Timezone: timezone, Quote: storage.Quote{EstimatedInput: 40, EstimatedOutput: 60, ReservedCost: 100}}
	r.Cost = storageCost(cost)
	if tokens != nil {
		r.Input, r.Output = tokens, &output
	} else {
		r.UsageMissing = true
	}
	return r
}

func TestExistingUsageSeedsNewBudget(t *testing.T) {
	tokens := int64(500)
	now := time.Now().UTC()
	gw := startLimitedGateway(t, "    budget: {usd_per_day: 1.00, tokens_per_day: 1000}\n", gatewayOptions{
		prepare: previousUsage(t, historical(1, now, "UTC", 990_000, &tokens), historical(2, now.Add(-72*time.Hour), "UTC", 500_000, &tokens)),
	})
	if v := gw.limitView(nil, "apps.bot.budget.usd_per_day"); v.Used != 990_000 {
		t.Fatalf("today's usage not seeded once: %+v", v)
	}
	if v := gw.limitView(nil, "apps.bot.budget.tokens_per_day"); v.Used != 500 {
		t.Fatalf("token history: %+v", v)
	}
	key := gw.createKey("bot").Key.Reveal()
	response, body := gw.call("/v1/chat/completions", key, testkit.OpenAIBody())
	if response.StatusCode != 429 || !strings.Contains(body, "spent $0.99") {
		t.Fatalf("seeded budget ignored: %d %s", response.StatusCode, body)
	}
}

func TestTimezoneOverlapSeedsConservatively(t *testing.T) {
	tokens := int64(1)
	// A UTC day only partly inside the Tokyo day of today is counted in full.
	now := time.Now().UTC()
	gw := startLimitedGateway(t, "    timezone: Asia/Tokyo\n    budget: {usd_per_day: 1.00}\n", gatewayOptions{
		prepare: previousUsage(t, historical(1, now, "UTC", 400_000, &tokens)),
	})
	if v := gw.limitView(nil, "apps.bot.budget.usd_per_day"); v.Used != 400_000 {
		t.Fatalf("overlapping history: %+v", v)
	}
}

func TestMissingHistoricalTokensNeverSeedZero(t *testing.T) {
	now := time.Now().UTC()
	missing := historical(1, now, "UTC", 100, nil)
	gw := startLimitedGateway(t, "    budget: {tokens_per_day: 100000}\n", gatewayOptions{
		prepare: func(dir string) {
			previousUsage(t, missing)(dir)
			store, err := sqlite.Open(context.Background(), dir)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			// Retention removed the detailed record: the aggregate alone cannot say
			// how many tokens that call used.
			if err = store.Retain(context.Background(), now.Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
		},
	})
	if v := gw.limitView(nil, "apps.bot.budget.tokens_per_day"); !v.Unknown {
		t.Fatalf("unknown token history read as zero: %+v", v)
	}
	key := gw.createKey("bot").Key.Reveal()
	if response, body := gw.call("/v1/chat/completions", key, testkit.OpenAIBody()); response.StatusCode != 429 || !strings.Contains(body, "tokens_per_day") {
		t.Fatalf("unproven history did not block: %d %s", response.StatusCode, body)
	}
}

func TestRemoveAndReaddLimitKeepsSpend(t *testing.T) {
	gw := startLimitedGateway(t, "    budget: {usd_per_day: 1.00}\n", gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	gw.call("/v1/chat/completions", key, testkit.OpenAIBody())
	gw.reload(func(text string) string { return strings.Replace(text, "    budget: {usd_per_day: 1.00}\n", "", 1) })
	gw.call("/v1/chat/completions", key, testkit.OpenAIBody())
	gw.results.waitCount(t, 2)
	gw.reload(func(text string) string { return withBot("    budget: {usd_per_day: 1.00}\n")(text) })
	if v := gw.limitView(nil, "apps.bot.budget.usd_per_day"); v.Used != gw.dailyCost(storage.Filter{}) || v.Used == 0 {
		t.Fatalf("spend while the limit was absent was lost: %+v", v)
	}
}

func TestBootstrapHappensOnce(t *testing.T) {
	tokens := int64(1)
	now := time.Now().UTC()
	gw := startLimitedGateway(t, "    budget: {usd_per_day: 10.00}\n", gatewayOptions{prepare: previousUsage(t, historical(1, now, "UTC", 300_000, &tokens))})
	key := gw.createKey("bot").Key.Reveal()
	gw.call("/v1/chat/completions", key, testkit.OpenAIBody())
	gw.results.waitCount(t, 1)
	if err := gw.accounting.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := gw.limitView(nil, "apps.bot.budget.usd_per_day").Used
	if err := gw.limits.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	restarted := restartCoordinator(t, gw)
	if v := restarted.limitView(nil, "apps.bot.budget.usd_per_day"); v.Used != want {
		t.Fatalf("history imported again on restart: %d, want %d", v.Used, want)
	}
}

func location(name string) *time.Location {
	loaded, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return loaded
}

func storageCost(micros int64) config.Microusd { return config.Microusd(micros) }

// restartCoordinator rebuilds limit state from the database, as a restart does.
func restartCoordinator(t *testing.T, gw *gateway) *gateway {
	t.Helper()
	coordinator := limits.New(limits.Options{Store: gw.usage})
	if err := coordinator.Recover(context.Background(), gw.config.Capture()); err != nil {
		t.Fatal(err)
	}
	restarted := *gw
	restarted.limits = coordinator
	return &restarted
}
