package integration

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/loopguard"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

func TestDowngradeCacheUsesEffectiveModel(t *testing.T) {
	gw := startCacheGateway(t, cacheOn+"    on_limit: downgrade\n    downgrade_to: fake-haiku\n"+tinyBudget+"    hard_cap: {usd_per_day: 1.00}\n", fakeprovider.Options{}, gatewayOptions{edit: withPolicyModels})
	key := gw.createKey("bot").Key.Reveal()
	body := zeroTemp(testkit.AnthropicBody())
	first, firstAnswer := gw.call("/v1/messages", key, body)
	if first.StatusCode != 200 || first.Header.Get(downgradedFrom) != "fake-claude" {
		t.Fatalf("first request not downgraded: %d", first.StatusCode)
	}
	if model, _ := requestModel(t, gw.lastRequest()); model != "fake-haiku" {
		t.Fatalf("provider got %s", model)
	}
	second, secondAnswer := gw.call("/v1/messages", key, body)
	if second.Header.Get("X-Comporta-Cache") != "hit" || second.Header.Get(downgradedFrom) != "fake-claude" || secondAnswer != firstAnswer {
		t.Fatalf("the downgraded answer is replayed under the target's key with the current policy headers: %v", second.Header)
	}
	gw.noProviderCall(t)
	gw.waitRecords(t, 2)
	hits := gw.cachedRecords(t)
	if len(hits) != 1 || hits[0].ModelRequested != "fake-claude" || hits[0].ModelUsed != "fake-haiku" || hits[0].PolicyOutcome != "downgraded" || hits[0].Cost != 0 {
		t.Fatalf("hit %+v", hits)
	}
}

func TestCacheDoesNotStoreFallbackResult(t *testing.T) {
	gw := startFallbackGateway(t, cacheOn+"    fallback: [fake-haiku]\n", map[string]fakeprovider.Scenario{"fake-claude": fakeprovider.ScenarioOverload}, gatewayOptions{cache: true})
	key := gw.createKey("bot").Key.Reveal()
	body := zeroTemp(testkit.AnthropicBody())
	for range 2 {
		response, text := gw.call("/v1/messages", key, body)
		if response.StatusCode != 200 || response.Header.Get("X-Comporta-Cache") != "" {
			t.Fatalf("%d %s", response.StatusCode, text)
		}
	}
	if models := gw.sentModels(t, 4); strings.Join(models, ",") != "fake-claude,fake-haiku,fake-claude,fake-haiku" {
		t.Fatalf("an answer from a later model is never stored: %v", models)
	}
	if s := gw.cache.Stats(); s.Entries != 0 || s.UsedBytes != 0 || s.Builders != 0 {
		t.Fatalf("stats %+v", s)
	}
}

func TestCacheDoesNotBypassLoopGuard(t *testing.T) {
	clock := loopguard.NewFakeClock(time.Now())
	gw := startCacheGateway(t, cacheOn+"    loop_guard:\n      similar_requests: 3\n", fakeprovider.Options{}, gatewayOptions{loop: true, loopClock: clock})
	key := gw.createKey("bot").Key.Reveal()
	body := zeroTemp(gptBody)
	cut := false
	for range 5 {
		clock.Advance(time.Second)
		response, text := gw.call("/v1/chat/completions", key, body)
		if response.StatusCode == http.StatusTooManyRequests {
			if loopCode(t, text) != "loop_detected" || response.Header.Get("X-Comporta-Cache") != "" {
				t.Fatalf("cut %s", text)
			}
			cut = true
			break
		}
	}
	if !cut {
		t.Fatal("cached repeats still count toward the loop guard")
	}
	if gw.providerCalls() != 1 {
		t.Fatal("only the first request reached the provider")
	}
}

func TestCacheRequiresCurrentAuthentication(t *testing.T) {
	gw := startCacheGateway(t, cacheOn, fakeprovider.Options{}, gatewayOptions{})
	created := gw.createKey("bot")
	key := created.Key.Reveal()
	body := zeroTemp(gptBody)
	gw.call("/v1/chat/completions", key, body)
	if response, _ := gw.call("/v1/chat/completions", "cmp_wrong", body); response.StatusCode != http.StatusUnauthorized || response.Header.Get("X-Comporta-Cache") != "" {
		t.Fatalf("an unauthenticated request never reaches the cache: %d", response.StatusCode)
	}
	if _, err := gw.store.Revoke(t.Context(), created.Metadata.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		response, _ := gw.call("/v1/chat/completions", key, body)
		if response.StatusCode == http.StatusUnauthorized {
			if response.Header.Get("X-Comporta-Cache") != "" {
				t.Fatal("a revoked key never gets a hit")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the revocation was not picked up")
		}
		time.Sleep(50 * time.Millisecond)
	}
}
