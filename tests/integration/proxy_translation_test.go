package integration

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

// responsesPrompt is a marked prompt, so the privacy canary can tell content apart.
var responsesPrompt = testkit.Prompt("Qual é o clima?")

func TestResponsesAcceptedAndMetered(t *testing.T) {
	gw := startFallbackGateway(t, "", nil, gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	response, body := gw.call("/v1/responses", key, `{"model":"fake-gpt","instructions":"Seja breve.","input":"`+responsesPrompt+`","max_output_tokens":32,"store":false}`)
	if response.StatusCode != 200 || response.Header.Get(translatedHeaderName) != "openai-chat->openai-responses" {
		t.Fatalf("%d %v %s", response.StatusCode, response.Header, body)
	}
	var answer struct {
		Object string `json:"object"`
		Status string `json:"status"`
		Output []struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			Input  int64 `json:"input_tokens"`
			Output int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &answer); err != nil || answer.Object != "response" || answer.Status != "completed" ||
		len(answer.Output) != 1 || answer.Output[0].Content[0].Type != "output_text" ||
		answer.Usage.Input != fakeprovider.FixtureInputTokens || answer.Usage.Output != fakeprovider.FixtureOutputTokens {
		t.Fatalf("not a Response object: %s", body)
	}
	if sent := gw.lastRequest(); sent.Path != "/v1/chat/completions" || !strings.Contains(string(sent.Body), `"max_tokens":32`) {
		t.Fatalf("upstream %s %s", sent.Path, sent.Body)
	}
	gw.results.waitCount(t, 1)
	records := gw.records(t)
	if len(records) != 1 || records[0].ModelUsed != "fake-gpt" || records[0].Cost <= 0 || records[0].Input == nil ||
		*records[0].Input != fakeprovider.FixtureInputTokens || records[0].Output == nil || *records[0].Output != fakeprovider.FixtureOutputTokens {
		t.Fatalf("records %+v", records)
	}
}

func TestResponsesToAnthropicNormal(t *testing.T) {
	gw := startFallbackGateway(t, "", nil, gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	response, body := gw.call("/v1/responses", key, `{"model":"fake-claude","input":[{"role":"user","content":[{"type":"input_text","text":"`+responsesPrompt+`"}]}]}`)
	if response.StatusCode != 200 || response.Header.Get(translatedHeaderName) != "anthropic->openai" || !strings.Contains(body, `"object":"response"`) {
		t.Fatalf("%d %v %s", response.StatusCode, response.Header, body)
	}
	sent := gw.lastRequest()
	if sent.Path != "/v1/messages" || !strings.Contains(string(sent.Body), `"max_tokens":`) || !strings.Contains(string(sent.Body), `"metadata":{"user_id":`) {
		t.Fatalf("upstream %s %s", sent.Path, sent.Body)
	}
}

func TestResponsesToAnthropicStream(t *testing.T) {
	gw := startFallbackGateway(t, "", nil, gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	response, body := gw.call("/v1/responses", key, `{"model":"fake-claude","input":"`+responsesPrompt+`","stream":true}`)
	if response.StatusCode != 200 || response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("%d %v %s", response.StatusCode, response.Header, body)
	}
	for _, want := range []string{"event: response.created", "event: response.output_text.delta", "event: response.output_text.done", "event: response.completed"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s in\n%s", want, body)
		}
	}
	gw.results.waitCount(t, 1)
	if records := gw.records(t); len(records) != 1 || records[0].Output == nil || *records[0].Output != fakeprovider.FixtureOutputTokens {
		t.Fatalf("records %+v", records)
	}
}

func TestTranslatedUsageUsesProviderNativeCounters(t *testing.T) {
	gw := startFallbackGateway(t, "", nil, gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	response, body := gw.call("/v1/messages", key, `{"model":"fake-gpt","max_tokens":16,"messages":[{"role":"user","content":"`+responsesPrompt+`"}]}`)
	if response.StatusCode != 200 || response.Header.Get(translatedHeaderName) != "openai->anthropic" {
		t.Fatalf("%d %v %s", response.StatusCode, response.Header, body)
	}
	var answer struct {
		Usage struct {
			Input  int64 `json:"input_tokens"`
			Output int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &answer); err != nil || answer.Usage.Input != fakeprovider.FixtureInputTokens || answer.Usage.Output != fakeprovider.FixtureOutputTokens {
		t.Fatalf("projected usage %s", body)
	}
	gw.results.waitCount(t, 1)
	records := gw.records(t)
	if len(records) != 1 || records[0].ModelUsed != "fake-gpt" || *records[0].Input != fakeprovider.FixtureInputTokens || *records[0].Output != fakeprovider.FixtureOutputTokens {
		t.Fatalf("records %+v", records)
	}
}

func TestTranslatedStreamBothDirections(t *testing.T) {
	gw := startFallbackGateway(t, "", nil, gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	response, body := gw.call("/v1/messages", key, `{"model":"fake-gpt","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"`+responsesPrompt+`"}]}`)
	if response.StatusCode != 200 || !strings.Contains(body, "event: message_start") || !strings.Contains(body, "event: message_stop") || strings.Contains(body, "[DONE]") {
		t.Fatalf("openai->anthropic stream %d\n%s", response.StatusCode, body)
	}
	response, body = gw.call("/v1/chat/completions", key, `{"model":"fake-claude","stream":true,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"`+responsesPrompt+`"}]}`)
	if response.StatusCode != 200 || !strings.Contains(body, `"object":"chat.completion.chunk"`) || !strings.Contains(body, "data: [DONE]") ||
		!strings.Contains(body, `"completion_tokens":`) || strings.Contains(body, "message_start") {
		t.Fatalf("anthropic->openai stream %d\n%s", response.StatusCode, body)
	}
	gw.results.waitCount(t, 2)
	for _, r := range gw.records(t) {
		if r.Output == nil || *r.Output != fakeprovider.FixtureOutputTokens {
			t.Fatalf("stream usage not settled from native counters: %+v", r)
		}
	}
}
