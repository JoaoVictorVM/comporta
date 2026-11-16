package integration

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/storage/sqlite"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

// TestCacheHitReleasesGenerationBudgetAndKeepsRequestRate covers the
// configuration, the request hash and temperature, the user hash and the
// reservation release deciding together what a hit does.
func TestCacheHitReleasesGenerationBudgetAndKeepsRequestRate(t *testing.T) {
	gw := startCacheGateway(t, cacheOn+"    budget: {usd_per_day: 10, tokens_per_day: 1000000}\n    limits: {requests_per_minute: 10, max_concurrent: 2}\n", fakeprovider.Options{}, gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	user := testkit.UserMarker()
	body := zeroTemp(testkit.AnthropicBody())
	gw.callAs("/v1/messages", key, user, body)
	gw.waitRecords(t, 1)
	spent := gw.limitView(nil, "apps.bot.budget.usd_per_day").Used
	tokens := gw.limitView(nil, "apps.bot.budget.tokens_per_day").Used
	if spent == 0 || tokens == 0 {
		t.Fatalf("the miss consumed nothing: %d %d", spent, tokens)
	}
	response, _ := gw.callAs("/v1/messages", key, user, body)
	if response.Header.Get("X-Comporta-Cache") != "hit" {
		t.Fatal("expected a hit")
	}
	gw.waitRecords(t, 2)
	budget := gw.limitView(nil, "apps.bot.budget.usd_per_day")
	if budget.Used != spent || budget.Reserved != 0 {
		t.Fatalf("a hit reserves and spends nothing: %+v", budget)
	}
	if v := gw.limitView(nil, "apps.bot.budget.tokens_per_day"); v.Used != tokens || v.Reserved != 0 {
		t.Fatalf("a hit consumes no tokens: %+v", v)
	}
	if v := gw.limitView(nil, "apps.bot.limits.requests_per_minute"); v.Used != 2 {
		t.Fatalf("a hit is still a request: %+v", v)
	}
	if v := gw.limitView(nil, "apps.bot.limits.max_concurrent"); v.Used != 0 {
		t.Fatalf("the concurrency slot is freed after the replay: %+v", v)
	}
	// A reload that changes an unrelated setting is a new generation: the
	// configuration in force governs the cache.
	gw.reload(func(text string) string { return strings.Replace(text, "max_concurrent: 2", "max_concurrent: 3", 1) })
	if response, _ := gw.callAs("/v1/messages", key, user, body); response.Header.Get("X-Comporta-Cache") != "" {
		t.Fatal("an applied reload starts an empty cache")
	}
	if gw.providerCalls() != 2 {
		t.Fatal("provider calls")
	}
}

func TestCacheCannotBypassBudgetOrHardCap(t *testing.T) {
	// Each request reserves about 0.025 USD for its output cap: the budget and
	// the hard cap fit one reservation at a time.
	for name, limit := range map[string]string{
		"budget":   "    budget: {usd_per_day: 0.04}\n",
		"hard cap": "    on_limit: warn\n    budget: {usd_per_day: 0.001}\n    hard_cap: {usd_per_day: 0.04}\n",
	} {
		t.Run(name, func(t *testing.T) {
			release := make(chan struct{})
			fake := fakeprovider.Options{
				ByModel:   map[string]fakeprovider.Scenario{"fake-claude": fakeprovider.ScenarioSlowStream},
				SlowDelay: time.Millisecond,
				Sleep: func(ctx context.Context, _ time.Duration) error {
					select {
					case <-release:
						return nil
					case <-ctx.Done():
						return ctx.Err()
					}
				},
			}
			gw := startCacheGateway(t, cacheOn+limit, fake, gatewayOptions{})
			key := gw.createKey("bot").Key.Reveal()
			body := zeroTemp(gptBody)
			if response, text := gw.call("/v1/chat/completions", key, body); response.StatusCode != 200 {
				t.Fatalf("miss %d %s", response.StatusCode, text)
			}
			held := make(chan struct{})
			go func() {
				defer close(held)
				request, _ := http.NewRequest(http.MethodPost, gw.proxyURL("/v1/messages"), strings.NewReader(`{"model":"fake-claude","max_tokens":4000,"messages":[{"role":"user","content":"hold"}],"stream":true}`))
				request.Header.Set("Content-Type", "application/json")
				request.Header.Set("X-Api-Key", key)
				request.Header.Set("Anthropic-Version", "2023-06-01")
				if response, err := http.DefaultClient.Do(request); err == nil {
					_, _ = io.Copy(io.Discard, response.Body)
					_ = response.Body.Close()
				}
			}()
			deadline := time.Now().Add(5 * time.Second)
			for gw.limitView(nil, "apps.bot.limits.max_concurrent").Used == 0 {
				if time.Now().After(deadline) {
					t.Fatal("the holding request never started")
				}
				time.Sleep(5 * time.Millisecond)
			}
			response, _ := gw.call("/v1/chat/completions", key, body)
			close(release)
			<-held
			if response.StatusCode != http.StatusTooManyRequests || response.Header.Get("X-Comporta-Cache") != "" {
				t.Fatalf("a budget that cannot reserve refuses even a cached request: %d", response.StatusCode)
			}
			gw.waitRecords(t, 3)
			if response, _ := gw.call("/v1/chat/completions", key, body); response.Header.Get("X-Comporta-Cache") != "hit" {
				t.Fatalf("with room again the entry is served: %d", response.StatusCode)
			}
		})
	}
}

func TestCacheCannotBypassRPMOrConcurrency(t *testing.T) {
	gw := startCacheGateway(t, cacheOn+"    limits: {requests_per_minute: 2}\n", fakeprovider.Options{}, gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	body := zeroTemp(gptBody)
	gw.call("/v1/chat/completions", key, body)
	if response, _ := gw.call("/v1/chat/completions", key, body); response.Header.Get("X-Comporta-Cache") != "hit" {
		t.Fatal("expected a hit")
	}
	if response, _ := gw.call("/v1/chat/completions", key, body); response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("a hit counts toward the request rate: %d", response.StatusCode)
	}
}

func TestCachedReplayDisconnectCostsZero(t *testing.T) {
	gw := startCacheGateway(t, cacheOn+"    budget: {usd_per_day: 10}\n", fakeprovider.Options{}, gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	body := zeroTemp(gptStream)
	gw.call("/v1/chat/completions", key, body)
	gw.waitRecords(t, 1)
	spent := gw.limitView(nil, "apps.bot.budget.usd_per_day").Used
	ctx, cancel := context.WithCancel(context.Background())
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, gw.proxyURL("/v1/chat/completions"), strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+key)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	_ = response.Body.Close()
	records := gw.waitRecords(t, 2)
	if len(records) != 2 || len(gw.cachedRecords(t)) != 1 {
		t.Fatalf("records %+v", records)
	}
	if v := gw.limitView(nil, "apps.bot.budget.usd_per_day"); v.Used != spent || v.Reserved != 0 {
		t.Fatalf("an interrupted replay costs nothing: %+v", v)
	}
}

// flakySettlement fails the durable completion of the first cached result, as
// a storage outage would.
type flakySettlement struct {
	*sqlite.Store
	failed atomic.Bool
}

func (f *flakySettlement) CompleteSettled(ctx context.Context, r storage.Record, st storage.Settlement) (int64, error) {
	if r.Outcome == "cached" && f.failed.CompareAndSwap(false, true) {
		return 0, storage.ErrUnavailable
	}
	return f.Store.CompleteSettled(ctx, r, st)
}

func TestCachedReleaseRecovery(t *testing.T) {
	var flaky *flakySettlement
	gw := startCacheGateway(t, cacheOn+"    budget: {usd_per_day: 10}\n", fakeprovider.Options{}, gatewayOptions{
		wrap: func(s *sqlite.Store) storage.LimitStore {
			flaky = &flakySettlement{Store: s}
			return flaky
		},
	})
	key := gw.createKey("bot").Key.Reveal()
	body := zeroTemp(gptBody)
	gw.call("/v1/chat/completions", key, body)
	gw.waitRecords(t, 1)
	spent := gw.limitView(nil, "apps.bot.budget.usd_per_day").Used
	if response, _ := gw.call("/v1/chat/completions", key, body); response.Header.Get("X-Comporta-Cache") != "hit" {
		t.Fatal("expected a hit")
	}
	if !flaky.failed.Load() {
		t.Fatal("the failure was not injected")
	}
	// The release stays conservatively held until the limit worker retries it;
	// it never becomes a charge.
	deadline := time.Now().Add(10 * time.Second)
	for {
		v := gw.limitView(nil, "apps.bot.budget.usd_per_day")
		if v.Reserved == 0 {
			if v.Used != spent {
				t.Fatalf("the retried release charged the hit: %+v", v)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the release was never retried: %+v", v)
		}
		time.Sleep(50 * time.Millisecond)
	}
	records := gw.waitRecords(t, 2)
	for _, r := range records {
		if r.Outcome == "cached" && r.Cost != 0 {
			t.Fatalf("hit record %+v", r)
		}
	}
}
