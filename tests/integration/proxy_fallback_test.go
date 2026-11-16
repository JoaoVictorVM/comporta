package integration

import (
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

const fallbackHeaderName = "X-Comporta-Fallback"

// withFallbackModels adds, next to the policy models, fake-sonnet: an
// Anthropic model priced like fake-claude, so a fallback between them costs
// the same per attempt.
func withFallbackModels(text string) string {
	text = withPolicyModels(text)
	text = strings.Replace(text, "models: [fake-claude, fake-haiku]", "models: [fake-claude, fake-haiku, fake-sonnet]", 1)
	text = strings.Replace(text, "allowed_models: [fake-claude, fake-gpt, fake-haiku, fake-gpt-mini]", "allowed_models: [fake-claude, fake-gpt, fake-haiku, fake-gpt-mini, fake-sonnet]", 1)
	return strings.Replace(text, "pricing:\n", "pricing:\n  fake-sonnet: {input: 2, output: 6, cache_write: 2.5, cache_read: 0.2}\n", 1)
}

func startFallbackGateway(t *testing.T, settings string, byModel map[string]fakeprovider.Scenario, options gatewayOptions) *gateway {
	t.Helper()
	options.metered = true
	edit := options.edit
	options.edit = func(text string) string {
		text = withFallbackModels(withBot(settings)(text))
		if edit != nil {
			text = edit(text)
		}
		return text
	}
	return startConfiguredGateway(t, fakeprovider.Options{ByModel: byModel}, options)
}

func modelBody(body, model string) string {
	return strings.Replace(body, `"model":"fake-claude"`, `"model":"`+model+`"`, 1)
}

// sentModels drains the models the provider received so far.
func (g *gateway) sentModels(t *testing.T, want int) []string {
	t.Helper()
	var out []string
	for range want {
		model, _ := requestModel(t, g.lastRequest())
		out = append(out, model)
	}
	g.noProviderCall(t)
	return out
}

func byAttempt(records []storage.Record, request string) map[int]storage.Record {
	out := map[int]storage.Record{}
	for _, r := range records {
		if r.RequestID == request || request == "" {
			out[r.AttemptIndex] = r
		}
	}
	return out
}

func TestFallback529ThenSuccess(t *testing.T) {
	gw := startFallbackGateway(t, "    fallback: [fake-haiku]\n", map[string]fakeprovider.Scenario{"fake-claude": fakeprovider.ScenarioOverload}, gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	response, body := gw.call("/v1/messages", key, testkit.AnthropicBody())
	if response.StatusCode != 200 || !strings.Contains(body, `"model":"fake-haiku"`) {
		t.Fatalf("%d %s", response.StatusCode, body)
	}
	if got := response.Header.Get(fallbackHeaderName); got != "fake-claude->fake-haiku (529)" {
		t.Fatalf("header %q", got)
	}
	if got := gw.sentModels(t, 2); fmt.Sprint(got) != "[fake-claude fake-haiku]" {
		t.Fatalf("sent %v", got)
	}
	gw.results.waitCount(t, 2)
	attempts := byAttempt(gw.records(t), "")
	first, second := attempts[1], attempts[2]
	if len(attempts) != 2 || first.RequestCount != 0 || second.RequestCount != 1 || first.ErrorCode != "provider_overloaded" ||
		second.Outcome != "fallback" || second.FallbackFrom != "fake-claude" || second.FallbackCause != "529" || second.ModelRequested != "fake-claude" {
		t.Fatalf("records %+v", attempts)
	}
	fallback := gw.policyEvents(t, events.Fallback)
	if len(fallback) != 1 || fallback[0].ValidateFallback() != nil || fallback[0].Payload.ToModel != "fake-haiku" || fallback[0].RequestID != second.RequestID {
		t.Fatalf("events %+v", fallback)
	}
	// The same chain in the OpenAI format, streaming.
	gw2 := startFallbackGateway(t, "    fallback: [fake-gpt-mini]\n", map[string]fakeprovider.Scenario{"fake-gpt": fakeprovider.ScenarioStreamErrorBeforeChunk}, gatewayOptions{})
	key = gw2.createKey("bot").Key.Reveal()
	response, body = gw2.call("/v1/chat/completions", key, testkit.OpenAIStreamBody())
	if response.StatusCode != 200 || !strings.Contains(body, "[DONE]") || strings.Contains(body, "overloaded") || response.Header.Get(fallbackHeaderName) != "fake-gpt->fake-gpt-mini (529)" {
		t.Fatalf("stream %d %q %s", response.StatusCode, response.Header.Get(fallbackHeaderName), body)
	}
}

func TestFallback400DoesNotRetry(t *testing.T) {
	gw := startFallbackGateway(t, "    fallback: [fake-haiku]\n", map[string]fakeprovider.Scenario{"fake-claude": fakeprovider.ScenarioBadRequest}, gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	response, body := gw.call("/v1/messages", key, testkit.AnthropicBody())
	if response.StatusCode != 400 || response.Header.Get(fallbackHeaderName) != "" {
		t.Fatalf("%d %s", response.StatusCode, body)
	}
	if got := gw.sentModels(t, 1); fmt.Sprint(got) != "[fake-claude]" {
		t.Fatalf("sent %v", got)
	}
	gw.results.waitCount(t, 1)
	if records := gw.records(t); len(records) != 1 || records[0].RequestCount != 1 {
		t.Fatalf("records %+v", records)
	}
	if n := len(gw.policyEvents(t, events.Fallback)); n != 0 {
		t.Fatalf("fallback events %d", n)
	}
}

func TestFallbackStreamAfterChunkDoesNotRetry(t *testing.T) {
	gw := startFallbackGateway(t, "    fallback: [fake-haiku]\n", map[string]fakeprovider.Scenario{"fake-claude": fakeprovider.ScenarioStreamErrorAfterChunk}, gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	response, body := gw.call("/v1/messages", key, testkit.AnthropicStreamBody())
	if response.StatusCode != 200 || !strings.Contains(body, "message_start") || !strings.Contains(body, `"comporta_code":"provider_overloaded"`) || strings.Contains(body, "fake overload") {
		t.Fatalf("%d %s", response.StatusCode, body)
	}
	if got := gw.sentModels(t, 1); fmt.Sprint(got) != "[fake-claude]" {
		t.Fatalf("sent %v", got)
	}
	// Disconnecting mid-stream is not retried either.
	gw2 := startFallbackGateway(t, "    fallback: [fake-haiku]\n", map[string]fakeprovider.Scenario{"fake-claude": fakeprovider.ScenarioDisconnectMidstream}, gatewayOptions{})
	key = gw2.createKey("bot").Key.Reveal()
	gw2.call("/v1/messages", key, testkit.AnthropicStreamBody())
	if got := gw2.sentModels(t, 1); fmt.Sprint(got) != "[fake-claude]" {
		t.Fatalf("sent %v", got)
	}
}

func TestFallbackAllModelsFail(t *testing.T) {
	gw := startFallbackGateway(t, "    fallback: [fake-haiku, fake-sonnet]\n", map[string]fakeprovider.Scenario{
		"fake-claude": fakeprovider.ScenarioOverload, "fake-haiku": fakeprovider.ScenarioServerError, "fake-sonnet": fakeprovider.ScenarioRateLimit,
	}, gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	response, body := gw.call("/v1/messages", key, testkit.AnthropicBody())
	decoded := decodeLimitError(t, body)
	if response.StatusCode != 502 || decoded.Error.Code != "fallback_exhausted" ||
		decoded.Error.Message != "all fallback models failed (fake-claude: 529, fake-haiku: 500, fake-sonnet: 429)" {
		t.Fatalf("%d %s", response.StatusCode, body)
	}
	if response.Header.Get(fallbackHeaderName) != "fake-claude->fake-haiku (529); fake-haiku->fake-sonnet (500)" || response.Header.Get("Retry-After") != "" {
		t.Fatalf("headers %v", response.Header)
	}
	gw.results.waitCount(t, 3)
	attempts := byAttempt(gw.records(t), "")
	last := attempts[3]
	if len(attempts) != 3 || last.RequestCount != 1 || last.TerminalErrorCode != "fallback_exhausted" || last.TerminalStatus == nil || *last.TerminalStatus != 502 || last.ErrorCode != "provider_rate_limited" {
		t.Fatalf("records %+v", attempts)
	}
	if n := len(gw.policyEvents(t, events.Fallback)); n != 2 {
		t.Fatalf("fallback events %d", n)
	}
}

func TestFallbackReloadPinsRoutingAndPrice(t *testing.T) {
	var gw *gateway
	var reloaded atomic.Bool
	gw = startFallbackGateway(t, "    fallback: [fake-haiku]\n", map[string]fakeprovider.Scenario{"fake-claude": fakeprovider.ScenarioOverload}, gatewayOptions{
		upstream: func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// A reload lands while the first model is being called: the chain
				// disappears and the next model gets dearer.
				if reloaded.CompareAndSwap(false, true) {
					gw.reload(func(text string) string {
						text = strings.Replace(text, "    fallback: [fake-haiku]\n", "", 1)
						return strings.Replace(text, "  fake-haiku: {input: 1, output: 1,", "  fake-haiku: {input: 1, output: 3,", 1)
					})
				}
				next.ServeHTTP(w, r)
			})
		},
	})
	key := gw.createKey("bot").Key.Reveal()
	response, body := gw.call("/v1/messages", key, testkit.AnthropicBody())
	if response.StatusCode != 200 || response.Header.Get(fallbackHeaderName) != "fake-claude->fake-haiku (529)" {
		t.Fatalf("the admitted operation lost its chain on reload: %d %s", response.StatusCode, body)
	}
	gw.results.waitCount(t, 2)
	second := byAttempt(gw.records(t), "")[2]
	if second.Quote.Price.Output != 1_000_000 {
		t.Fatalf("the continuation was priced on the new snapshot: %+v", second.Quote.Price)
	}
	// A new request sees the new configuration: no chain, new price.
	response, _ = gw.call("/v1/messages", key, testkit.AnthropicBody())
	if response.StatusCode != 529 {
		t.Fatalf("new request after reload: %d", response.StatusCode)
	}
	response, _ = gw.call("/v1/messages", key, modelBody(testkit.AnthropicBody(), "fake-haiku"))
	gw.results.waitCount(t, 4)
	for _, r := range gw.records(t) {
		if r.ModelUsed == "fake-haiku" && r.AttemptIndex == 1 && r.Quote.Price.Output != 3_000_000 {
			t.Fatalf("new price not applied: %+v", r.Quote.Price)
		}
	}
}
