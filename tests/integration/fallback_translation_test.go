package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/pricing"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

const translatedHeaderName = "X-Comporta-Translated"

// translationPrompt is a marked prompt, so the privacy canary can tell content apart.
var translationPrompt = testkit.Prompt("Qual é o clima?")

var anthropicToolsRequest = `{"model":"fake-claude","max_tokens":64,"system":"Use ferramentas.",
	"messages":[{"role":"user","content":[{"type":"text","text":"` + translationPrompt + `"}]}],
	"tools":[{"name":"read_status","description":"Lê o status","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}],
	"tool_choice":{"type":"any"}}`

var openAIToolsRequest = `{"model":"fake-gpt","max_tokens":64,"messages":[{"role":"developer","content":"Use ferramentas."},{"role":"user","content":"` + translationPrompt + `"}],
	"tools":[{"type":"function","function":{"name":"read_status","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}}],"tool_choice":"required"}`

// callWith sends a request with extra headers, such as a provider beta.
func (g *gateway) callWith(path, key, body string, headers map[string]string) (*http.Response, string) {
	g.t.Helper()
	request, err := http.NewRequest(http.MethodPost, g.proxyURL(path), strings.NewReader(body))
	if err != nil {
		g.t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	if strings.HasPrefix(path, "/v1/messages") {
		request.Header.Set("X-Api-Key", key)
		request.Header.Set("Anthropic-Version", "2023-06-01")
	} else {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		g.t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		g.t.Fatal(err)
	}
	return response, string(data)
}

func TestAnthropicToolsFallbackToOpenAI(t *testing.T) {
	gw := startFallbackGateway(t, "    fallback: [fake-gpt]\n", map[string]fakeprovider.Scenario{
		"fake-claude": fakeprovider.ScenarioOverload, "fake-gpt": fakeprovider.ScenarioAgentLoop,
	}, gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	response, body := gw.call("/v1/messages", key, anthropicToolsRequest)
	if response.StatusCode != 200 || response.Header.Get(translatedHeaderName) != "openai->anthropic" || response.Header.Get(fallbackHeaderName) != "fake-claude->fake-gpt (529)" {
		t.Fatalf("%d %v %s", response.StatusCode, response.Header, body)
	}
	var answer struct {
		Type       string `json:"type"`
		Model      string `json:"model"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(body), &answer); err != nil || answer.Type != "message" || answer.Model != "fake-gpt" || answer.StopReason != "tool_use" ||
		len(answer.Content) != 1 || answer.Content[0].Type != "tool_use" || answer.Content[0].ID != fakeprovider.LoopToolCallID ||
		answer.Content[0].Name != fakeprovider.LoopToolName || string(answer.Content[0].Input) != fakeprovider.LoopToolInput {
		t.Fatalf("not a valid Anthropic tool answer: %s", body)
	}
	first := gw.lastRequest()
	second := gw.lastRequest()
	if first.Path != "/v1/messages" || second.Path != "/v1/chat/completions" || strings.Contains(string(second.Body), `"system":`) ||
		!strings.Contains(string(second.Body), `"tool_choice":"required"`) {
		t.Fatalf("attempts went out as %s then %s %s", first.Path, second.Path, second.Body)
	}
	gw.results.waitCount(t, 2)
	attempts := byAttempt(gw.records(t), "")
	if len(attempts) != 2 || attempts[2].ModelUsed != "fake-gpt" || attempts[2].Outcome != "fallback" || attempts[2].RequestCount != 1 {
		t.Fatalf("records %+v", attempts)
	}
}

func TestOpenAIToolsFallbackToAnthropic(t *testing.T) {
	gw := startFallbackGateway(t, "    fallback: [fake-claude]\n", map[string]fakeprovider.Scenario{
		"fake-gpt": fakeprovider.ScenarioServerError, "fake-claude": fakeprovider.ScenarioAgentLoop,
	}, gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	response, body := gw.call("/v1/chat/completions", key, openAIToolsRequest)
	if response.StatusCode != 200 || response.Header.Get(translatedHeaderName) != "anthropic->openai" {
		t.Fatalf("%d %v %s", response.StatusCode, response.Header, body)
	}
	if !strings.Contains(body, `"object":"chat.completion"`) || !strings.Contains(body, `"finish_reason":"tool_calls"`) ||
		!strings.Contains(body, `"id":"`+fakeprovider.LoopToolUseID+`"`) {
		t.Fatalf("not a valid Chat answer: %s", body)
	}
	gw.lastRequest()
	anthropic := gw.lastRequest()
	if anthropic.Path != "/v1/messages" || anthropic.Header.Get("Anthropic-Version") != "2026-10-21" || !strings.Contains(string(anthropic.Body), `"tool_choice":{"type":"any"}`) ||
		!strings.Contains(string(anthropic.Body), `"system":[{"type":"text","text":"Use ferramentas."}]`) {
		t.Fatalf("Anthropic attempt %s %v %s", anthropic.Path, anthropic.Header, anthropic.Body)
	}
}

func TestTranslatedFallbackUsesDestinationQuote(t *testing.T) {
	gw := startFallbackGateway(t, "    fallback: [fake-gpt]\n", map[string]fakeprovider.Scenario{
		"fake-claude": fakeprovider.ScenarioOverload, "fake-gpt": fakeprovider.ScenarioAgentLoop,
	}, gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	if response, body := gw.call("/v1/messages", key, anthropicToolsRequest); response.StatusCode != 200 {
		t.Fatalf("%d %s", response.StatusCode, body)
	}
	gw.lastRequest()
	rendered := gw.lastRequest()
	gw.results.waitCount(t, 2)
	second := byAttempt(gw.records(t), "")[2]
	want, err := pricing.EstimateInput(int64(len(anthropicToolsRequest)), int64(len(rendered.Body)), 0)
	if err != nil {
		t.Fatal(err)
	}
	if second.Quote.EstimatedInput != want {
		t.Fatalf("the translated attempt was quoted on %d input tokens, its rendered body gives %d", second.Quote.EstimatedInput, want)
	}
}

func TestUntranslatableFeatureRecordedWithoutReservation(t *testing.T) {
	gw := startFallbackGateway(t, "    fallback: [fake-gpt, fake-haiku]\n", map[string]fakeprovider.Scenario{"fake-claude": fakeprovider.ScenarioOverload}, gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	response, body := gw.callWith("/v1/messages", key, anthropicToolsRequest, map[string]string{"Anthropic-Beta": "fake-beta-2026-01-01"})
	if response.StatusCode != 200 || response.Header.Get(fallbackHeaderName) != "fake-claude->fake-haiku (529)" || response.Header.Get(translatedHeaderName) != "" {
		t.Fatalf("%d %v %s", response.StatusCode, response.Header, body)
	}
	if got := gw.sentModels(t, 2); fmt.Sprint(got) != "[fake-claude fake-haiku]" {
		t.Fatalf("sent %v", got)
	}
	skips := gw.policyEvents(t, events.TranslationSkipped)
	if len(skips) != 1 || skips[0].ValidateTranslationSkip() != nil || skips[0].Payload.Model != "fake-gpt" || skips[0].Payload.Feature != "provider_beta" ||
		skips[0].Payload.Reason != "untranslatable_feature" {
		t.Fatalf("skip events %+v", skips)
	}
	gw.results.waitCount(t, 2)
	attempts := byAttempt(gw.records(t), "")
	for _, r := range attempts {
		if r.ModelUsed == "fake-gpt" {
			t.Fatalf("the skipped model has a record: %+v", r)
		}
	}
	if len(attempts) != 2 || attempts[2].ModelUsed != "fake-haiku" || attempts[2].FallbackFrom != "fake-claude" {
		t.Fatalf("records %+v", attempts)
	}
}

func TestNoModelCanRepresentRequest(t *testing.T) {
	gw := startFallbackGateway(t, "    fallback: [fake-gpt-mini]\n", nil, gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	body := strings.Replace(anthropicToolsRequest, `"model":"fake-claude"`, `"model":"fake-gpt"`, 1)
	response, answer := gw.callWith("/v1/messages", key, body, map[string]string{"Anthropic-Beta": "fake-beta-2026-01-01"})
	decoded := decodeLimitError(t, answer)
	if response.StatusCode != 502 || decoded.Error.Code != "no_eligible_fallback_model" ||
		decoded.Error.Message != "no eligible fallback model can represent this request (provider_beta)" {
		t.Fatalf("%d %s", response.StatusCode, answer)
	}
	gw.noProviderCall(t)
	if skips := gw.policyEvents(t, events.TranslationSkipped); len(skips) != 2 {
		t.Fatalf("skip events %+v", skips)
	}
}
