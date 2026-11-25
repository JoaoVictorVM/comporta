package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/sensitivedata"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
	"github.com/JoaoVictorVM/comporta/internal/translation"
)

// translationPrompt is a marked prompt, so the privacy canary can tell content
// apart.
var translationPrompt = testkit.Prompt("Qual é o clima?")

var anthropicToolsBody = `{"model":"%s","max_tokens":256,"system":"Use ferramentas.","stream":%t,
	"messages":[{"role":"user","content":[{"type":"text","text":"` + translationPrompt + `"}]}],
	"tools":[{"name":"get_weather","description":"Consulta o clima","input_schema":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}]}`

func anthropicTools(model string, stream bool) string {
	return fmt.Sprintf(anthropicToolsBody, model, stream)
}

// seen records what a provider received, for the assertions on rendering.
type seen struct {
	mu      sync.Mutex
	path    string
	header  http.Header
	request map[string]any
}

func (s *seen) record(r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.path, s.header, s.request = r.URL.Path, r.Header.Clone(), body
}

func chatToolAnswer(s *seen) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-a","choices":[{"index":0,"message":{"role":"assistant","content":null,
			"tool_calls":[{"id":"call_0123456789abcdef","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"Recife\"}"}}]},"finish_reason":"tool_calls"}],
			"usage":{"prompt_tokens":30,"completion_tokens":12,"total_tokens":42}}`))
	}
}

func anthropicTextAnswer(s *seen) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"model-a","content":[{"type":"text","text":"Está ensolarado."}],
			"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":20,"output_tokens":6}}`))
	}
}

func sseAnswer(events ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range events {
			_, _ = w.Write([]byte(event))
			w.(http.Flusher).Flush()
		}
	}
}

var anthropicStreamEvents = []string{
	"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"model-a\",\"content\":[],\"usage\":{\"input_tokens\":9,\"output_tokens\":1}}}\n\n",
	"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n",
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Olá\"}}\n\n",
	"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n",
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":4}}\n\n",
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
}

var chatStreamEvents = []string{
	"data: {\"id\":\"c\",\"model\":\"gpt-a\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"},\"finish_reason\":null}]}\n\n",
	"data: {\"id\":\"c\",\"model\":\"gpt-a\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Olá\"},\"finish_reason\":null}]}\n\n",
	"data: {\"id\":\"c\",\"model\":\"gpt-a\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
	"data: {\"id\":\"c\",\"model\":\"gpt-a\",\"choices\":[],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":4,\"total_tokens\":13}}\n\n",
	"data: [DONE]\n\n",
}

// skipSessions admits everything and records translation skips.
type skipSessions struct {
	fakeSessions
	fail  error
	skips []TranslationSkip
}

func (s *skipSessions) AdmitSession(context.Context, *config.Snapshot, *Envelope, []Candidate) (AccountingSession, Admission, *Rejection) {
	return s, Admission{}, nil
}

func (s *skipSessions) RecordTranslationSkip(_ context.Context, _ *Envelope, skip TranslationSkip) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail != nil {
		return s.fail
	}
	s.skips = append(s.skips, skip)
	return nil
}

func (s *skipSessions) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, skip := range s.skips {
		out = append(out, fmt.Sprintf("%s@%d:%s", skip.Model, skip.Position, skip.Feature))
	}
	return out
}

func sessionFixture(t *testing.T, chain string, replies map[string]http.HandlerFunc, sessions *skipSessions) (*fixture, *modelUpstream) {
	t.Helper()
	upstream := &modelUpstream{replies: replies}
	text := strings.Replace(fallbackConfig, "    allowed_models:", "    fallback: ["+chain+"]\n    allowed_models:", 1)
	return newFixture(t, startServer(t, upstream), fixtureOptions{config: text, accounting: sessions}), upstream
}

func TestFallbackCrossFormatEligible(t *testing.T) {
	gpt := &seen{}
	fx, upstream := fallbackFixture(t, "model-a, gpt-a", map[string]http.HandlerFunc{"model-a": answerStatus(529), "gpt-a": chatToolAnswer(gpt)}, "")
	response, body := fx.post("/v1/messages", "anthropic", anthropicTools("model-a", false))
	if response.StatusCode != 200 || !slices.Equal(upstream.sent(), []string{"model-a", "gpt-a"}) {
		t.Fatalf("%d %s %v", response.StatusCode, body, upstream.sent())
	}
	if response.Header.Get(translatedHeader) != "openai->anthropic" || response.Header.Get(fallbackHeader) != "model-a->gpt-a (529)" {
		t.Fatalf("headers %v", response.Header)
	}
	var answer struct {
		Type       string `json:"type"`
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type  string         `json:"type"`
			ID    string         `json:"id"`
			Input map[string]any `json:"input"`
		} `json:"content"`
		Usage struct {
			Input  int `json:"input_tokens"`
			Output int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &answer); err != nil || answer.Type != "message" || answer.StopReason != "tool_use" ||
		len(answer.Content) != 1 || answer.Content[0].Type != "tool_use" || answer.Content[0].Input["city"] != "Recife" ||
		answer.Usage.Input != 30 || answer.Usage.Output != 12 {
		t.Fatalf("not a valid Anthropic answer: %s", body)
	}
	gpt.mu.Lock()
	defer gpt.mu.Unlock()
	if gpt.path != "/v1/chat/completions" || gpt.header.Get("Authorization") == "" || gpt.header.Get("X-Api-Key") != "" || gpt.header.Get("Anthropic-Version") != "" {
		t.Fatalf("translated request went out as %s %v", gpt.path, gpt.header)
	}
	if gpt.request["model"] != "gpt-a" || gpt.request["tools"] == nil || gpt.request["system"] != nil {
		t.Fatalf("body not rendered for Chat Completions: %v", gpt.request)
	}
}

func TestOpenAIClientTranslatedToAnthropic(t *testing.T) {
	claude := &seen{}
	fx, _ := fallbackFixture(t, "gpt-a, model-a", map[string]http.HandlerFunc{"gpt-a": answerStatus(503), "model-a": anthropicTextAnswer(claude)}, "")
	response, body := fx.post("/v1/chat/completions", "openai", `{"model":"gpt-a","messages":[{"role":"system","content":"s"},{"role":"user","content":"`+translationPrompt+`"}]}`)
	if response.StatusCode != 200 || response.Header.Get(translatedHeader) != "anthropic->openai" || !strings.Contains(body, `"object":"chat.completion"`) ||
		!strings.Contains(body, "Está ensolarado.") || !strings.Contains(body, `"prompt_tokens":20`) {
		t.Fatalf("%d %v %s", response.StatusCode, response.Header, body)
	}
	claude.mu.Lock()
	defer claude.mu.Unlock()
	if claude.path != "/v1/messages" || claude.header.Get("Anthropic-Version") != "2026-10-21" || claude.header.Get("X-Api-Key") == "" ||
		claude.request["max_tokens"] == nil || claude.request["system"] == nil {
		t.Fatalf("translated request went out as %s %v %v", claude.path, claude.header, claude.request)
	}
}

func TestFallbackSkipsUntranslatableWithoutAttempt(t *testing.T) {
	sessions := &skipSessions{}
	fx, upstream := sessionFixture(t, "model-a, gpt-a, model-b", map[string]http.HandlerFunc{"model-a": answerStatus(529)}, sessions)
	request := fx.request(http.MethodPost, "/v1/messages", "anthropic", anthropicTools("model-a", false))
	request.Header.Set("Anthropic-Beta", "some-beta-2026-01-01")
	response, body := fx.do(request)
	if response.StatusCode != 200 || !slices.Equal(upstream.sent(), []string{"model-a", "model-b"}) {
		t.Fatalf("%d %s %v", response.StatusCode, body, upstream.sent())
	}
	if got := sessions.recorded(); !slices.Equal(got, []string{"gpt-a@2:provider_beta"}) {
		t.Fatalf("skips %v", got)
	}
	// The skipped model was never admitted: only one continuation exists.
	if !slices.Equal(sessions.continued, []string{"model-a>model-b:529"}) || response.Header.Get(fallbackHeader) != "model-a->model-b (529)" {
		t.Fatalf("continuations %v header %q", sessions.continued, response.Header.Get(fallbackHeader))
	}
}

func TestFallbackAllTargetsUntranslatable(t *testing.T) {
	sessions := &skipSessions{}
	fx, upstream := sessionFixture(t, "gpt-b", nil, sessions)
	request := fx.request(http.MethodPost, "/v1/messages", "anthropic", anthropicTools("gpt-a", false))
	request.Header.Set("Anthropic-Beta", "some-beta-2026-01-01")
	response, body := fx.do(request)
	if response.StatusCode != 502 || comportaCode(t, body) != string(CodeNoEligibleFallbackModel) || len(upstream.sent()) != 0 ||
		!strings.Contains(body, "no eligible fallback model can represent this request (provider_beta)") {
		t.Fatalf("%d %s %v", response.StatusCode, body, upstream.sent())
	}
	if got := sessions.recorded(); !slices.Equal(got, []string{"gpt-a@0:provider_beta", "gpt-b@1:provider_beta"}) {
		t.Fatalf("skips %v", got)
	}
}

func TestTranslationSkipFailureStopsOperation(t *testing.T) {
	sessions := &skipSessions{fail: errors.New("storage down")}
	fx, upstream := sessionFixture(t, "gpt-b, model-a", nil, sessions)
	request := fx.request(http.MethodPost, "/v1/messages", "anthropic", anthropicTools("gpt-a", false))
	request.Header.Set("Anthropic-Beta", "some-beta-2026-01-01")
	response, body := fx.do(request)
	if response.StatusCode != 503 || comportaCode(t, body) != string(CodeUsageStorageUnavailable) || len(upstream.sent()) != 0 {
		t.Fatalf("%d %s %v", response.StatusCode, body, upstream.sent())
	}
}

func TestInitialModelReplacedWhenUntranslatable(t *testing.T) {
	sessions := &skipSessions{}
	fx, upstream := sessionFixture(t, "gpt-a, model-b", nil, sessions)
	request := fx.request(http.MethodPost, "/v1/messages", "anthropic", anthropicTools("gpt-a", false))
	request.Header.Set("Anthropic-Beta", "some-beta-2026-01-01")
	response, body := fx.do(request)
	if response.StatusCode != 200 || !slices.Equal(upstream.sent(), []string{"model-b"}) || response.Header.Get(translatedHeader) != "" {
		t.Fatalf("%d %s %v", response.StatusCode, body, upstream.sent())
	}
	if got := sessions.recorded(); !slices.Equal(got, []string{"gpt-a@1:provider_beta"}) {
		t.Fatalf("skips %v", got)
	}
}

func TestResponsesRouteExactMatch(t *testing.T) {
	if route, ok := MatchRoute(http.MethodPost, "/v1/responses"); !ok || route != RouteResponses || route.wire() != translation.OpenAIResponses {
		t.Fatal("POST /v1/responses must match")
	}
}

func TestResponsesRouteRejectsVariants(t *testing.T) {
	for _, c := range []struct{ method, path string }{
		{http.MethodGet, "/v1/responses"}, {http.MethodPost, "/v1/responses/"}, {http.MethodPost, "/v1//responses"},
		{http.MethodPost, "/v1/responses/resp_1"}, {http.MethodPost, "/v1/Responses"}, {http.MethodPost, "/v1/%72esponses"},
	} {
		if _, ok := MatchRoute(c.method, c.path); ok {
			t.Fatalf("%s %s matched", c.method, c.path)
		}
	}
}

func TestResponsesToChatNormal(t *testing.T) {
	gpt := &seen{}
	fx, _ := fallbackFixture(t, "", map[string]http.HandlerFunc{"gpt-a": chatToolAnswer(gpt)}, "")
	response, body := fx.post("/v1/responses", "openai", `{"model":"gpt-a","instructions":"seja breve","input":"Qual é o clima?",
		"tools":[{"type":"function","name":"get_weather","parameters":{"type":"object"}}],"max_output_tokens":64,"store":false}`)
	if response.StatusCode != 200 || response.Header.Get(translatedHeader) != "openai-chat->openai-responses" {
		t.Fatalf("%d %v %s", response.StatusCode, response.Header, body)
	}
	var answer map[string]any
	if err := json.Unmarshal([]byte(body), &answer); err != nil || answer["object"] != "response" || answer["status"] != "completed" {
		t.Fatalf("not a Response object: %s", body)
	}
	call := answer["output"].([]any)[0].(map[string]any)
	if call["type"] != "function_call" || call["arguments"] != `{"city":"Recife"}` || call["call_id"] != "call_0123456789abcdef" {
		t.Fatalf("function call: %v", call)
	}
	gpt.mu.Lock()
	defer gpt.mu.Unlock()
	if gpt.path != "/v1/chat/completions" || gpt.request["max_tokens"] != 64.0 || gpt.request["input"] != nil || gpt.request["user"] == nil {
		t.Fatalf("Responses body went upstream as %s %v", gpt.path, gpt.request)
	}
}

func TestResponsesToAnthropicNormal(t *testing.T) {
	claude := &seen{}
	fx, _ := fallbackFixture(t, "", map[string]http.HandlerFunc{"model-a": anthropicTextAnswer(claude)}, "")
	response, body := fx.post("/v1/responses", "openai", `{"model":"model-a","input":[{"role":"user","content":[{"type":"input_text","text":"`+translationPrompt+`"}]}]}`)
	if response.StatusCode != 200 || response.Header.Get(translatedHeader) != "anthropic->openai" || !strings.Contains(body, "Está ensolarado.") ||
		!strings.Contains(body, `"input_tokens":20`) {
		t.Fatalf("%d %v %s", response.StatusCode, response.Header, body)
	}
	claude.mu.Lock()
	defer claude.mu.Unlock()
	metadata, _ := claude.request["metadata"].(map[string]any)
	if claude.path != "/v1/messages" || metadata["user_id"] == nil {
		t.Fatalf("Anthropic request %s %v", claude.path, claude.request)
	}
}

func TestResponsesMalformedAndStateful(t *testing.T) {
	fx, upstream := fallbackFixture(t, "", nil, "")
	for body, code := range map[string]string{
		`{"model":"gpt-a"}`:                                    "invalid_request",
		`{"model":"gpt-a","input":5}`:                          "invalid_request",
		`{"model":"gpt-a","input":[{"type":"function_call"}]}`: "invalid_request",
		`{"model":"gpt-a","input":"` + translationPrompt + `","previous_response_id":"resp_1"}`:         string(CodeNoEligibleFallbackModel),
		`{"model":"gpt-a","input":"` + translationPrompt + `","tools":[{"type":"web_search_preview"}]}`: string(CodeNoEligibleFallbackModel),
		`{"model":"gpt-a","input":"` + translationPrompt + `","max_output_tokens":0}`:                   "invalid_request",
	} {
		response, answer := fx.post("/v1/responses", "openai", body)
		if comportaCode(t, answer) != code || (code == "invalid_request" && response.StatusCode != 400) {
			t.Fatalf("%s: %d %s", body, response.StatusCode, answer)
		}
	}
	if len(upstream.sent()) != 0 {
		t.Fatalf("a refused Responses request reached a provider: %v", upstream.sent())
	}
}

func TestResponsesEnvelope(t *testing.T) {
	request := mustParse(t, RouteResponses, `{"model":"gpt-a","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AA"}]},{"type":"function_call_output","call_id":"c","output":"x"}],"stream":true,"max_output_tokens":10,"temperature":null}`)
	e := request.envelope
	if e.Wire != translation.OpenAIResponses || e.Endpoint != "/v1/responses" || !e.Stream || e.MaxTokens.Value != 10 || e.OutputLimitField != "max_output_tokens" ||
		e.ImageCount != 1 || !e.LastMessagesHash.Known || e.Temperature.Known {
		t.Fatalf("envelope %+v", e)
	}
	if len(request.patches) != 0 {
		t.Fatal("a Responses body must not get the Chat usage patch")
	}
}

func TestResponsesIdentityPrecedence(t *testing.T) {
	request := mustParse(t, RouteResponses, `{"model":"gpt-a","input":"`+translationPrompt+`","user":"field-user","metadata":{"user_id":"meta-user"}}`, headerComportaUser, "header-user")
	raw, source, ok := request.envelope.RawUser()
	if !ok || raw != "header-user" || source != UserSourceHeader {
		t.Fatalf("user %q from %s", raw, source)
	}
	patches, rejected := identityPatches(request.document, RouteResponses, strings.Repeat("a", 32))
	if rejected != nil || len(patches) != 2 {
		t.Fatalf("every identity field must hold the pseudonym: %d patches", len(patches))
	}
}

func TestResponsesOutputCap(t *testing.T) {
	request := mustParse(t, RouteResponses, `{"model":"gpt-a","input":"`+translationPrompt+`"}`)
	if err := request.capOutput(50); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(request.effectiveBody()), `"max_output_tokens":50`) {
		t.Fatalf("cap not applied: %s", request.effectiveBody())
	}
}

func TestResponsesToAnthropicStream(t *testing.T) {
	fx, _ := fallbackFixture(t, "", map[string]http.HandlerFunc{"model-a": sseAnswer(anthropicStreamEvents...)}, "")
	response, body := fx.post("/v1/responses", "openai", `{"model":"model-a","input":"`+translationPrompt+`","stream":true}`)
	if response.StatusCode != 200 || response.Header.Get("Content-Type") != "text/event-stream" || response.Header.Get(translatedHeader) != "anthropic->openai" {
		t.Fatalf("%d %v", response.StatusCode, response.Header)
	}
	for _, want := range []string{"event: response.created", "event: response.output_text.delta", `"delta":"Olá"`, "event: response.completed", `"output_tokens":4`} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in\n%s", want, body)
		}
	}
	if strings.Contains(body, "message_start") {
		t.Fatal("provider events leaked to the client")
	}
}

func TestTranslatedStreamFallbackBeforeFirstClientEvent(t *testing.T) {
	fx, upstream := fallbackFixture(t, "model-a, gpt-a", map[string]http.HandlerFunc{"model-a": answerStatus(529), "gpt-a": sseAnswer(chatStreamEvents...)}, "")
	response, body := fx.post("/v1/messages", "anthropic", anthropicTools("model-a", true))
	if response.StatusCode != 200 || !slices.Equal(upstream.sent(), []string{"model-a", "gpt-a"}) || response.Header.Get(translatedHeader) != "openai->anthropic" {
		t.Fatalf("%d %v %s", response.StatusCode, upstream.sent(), body)
	}
	for _, want := range []string{"event: message_start", `"text":"Olá"`, `"stop_reason":"end_turn"`, `"output_tokens":4`, "event: message_stop"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %q in\n%s", want, body)
		}
	}
	if strings.Contains(body, "[DONE]") || strings.Contains(body, "chat.completion") {
		t.Fatal("provider chunks leaked to the client")
	}
}

func TestTranslationErrorBeforeCommit(t *testing.T) {
	thinking := "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n"
	fx, upstream := fallbackFixture(t, "model-a, gpt-a", map[string]http.HandlerFunc{"model-a": sseAnswer(anthropicStreamEvents[0], thinking)}, "")
	response, body := fx.post("/v1/chat/completions", "openai", `{"model":"model-a","messages":[{"role":"user","content":"`+translationPrompt+`"}],"stream":true}`)
	if response.StatusCode != 502 || comportaCode(t, body) != string(CodeResponseTranslationFailed) || !slices.Equal(upstream.sent(), []string{"model-a"}) {
		t.Fatalf("%d %s %v", response.StatusCode, body, upstream.sent())
	}
}

func TestTranslationErrorAfterCommit(t *testing.T) {
	broken := "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"citations_delta\",\"citation\":{}}}\n\n"
	fx, upstream := fallbackFixture(t, "model-a, gpt-a", map[string]http.HandlerFunc{"model-a": sseAnswer(append(slices.Clone(anthropicStreamEvents[:3]), broken)...)}, "")
	response, body := fx.post("/v1/chat/completions", "openai", `{"model":"model-a","messages":[{"role":"user","content":"`+translationPrompt+`"}],"stream":true}`)
	if response.StatusCode != 200 || !slices.Equal(upstream.sent(), []string{"model-a"}) {
		t.Fatalf("%d %v", response.StatusCode, upstream.sent())
	}
	if !strings.Contains(body, `"content":"Olá"`) || !strings.Contains(body, `"comporta_code":"response_translation_failed"`) || strings.Contains(body, "[DONE]") ||
		strings.Contains(body, "citation") {
		t.Fatalf("stream did not end with a client-format error:\n%s", body)
	}
}

func TestTranslatedStreamNeverRetriesAfterClientEvent(t *testing.T) {
	cut := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(anthropicStreamEvents[0] + anthropicStreamEvents[1] + anthropicStreamEvents[2]))
		w.(http.Flusher).Flush()
		dropConnection(w, r)
	}
	fx, upstream := fallbackFixture(t, "model-a, gpt-a", map[string]http.HandlerFunc{"model-a": cut}, "")
	response, body := fx.post("/v1/responses", "openai", `{"model":"model-a","input":"`+translationPrompt+`","stream":true}`)
	if response.StatusCode != 200 || !slices.Equal(upstream.sent(), []string{"model-a"}) || !strings.Contains(body, "event: error") ||
		strings.Contains(body, "response.completed") {
		t.Fatalf("%d %v\n%s", response.StatusCode, upstream.sent(), body)
	}
}

func TestNativeRequestsRemainBytePreserving(t *testing.T) {
	claude := &seen{}
	raw := `{"model":"model-a","max_tokens":5,"future_field":{"kept":true},"messages":[{"role":"user","content":"` + translationPrompt + `"}]}`
	fx, _ := fallbackFixture(t, "", map[string]http.HandlerFunc{"model-a": anthropicTextAnswer(claude)}, "")
	request := fx.request(http.MethodPost, "/v1/messages", "anthropic", raw)
	request.Header.Set("Anthropic-Beta", "some-beta-2026-01-01")
	response, body := fx.do(request)
	if response.StatusCode != 200 || response.Header.Get(translatedHeader) != "" || !strings.Contains(body, `"id":"msg_1"`) {
		t.Fatalf("%d %v %s", response.StatusCode, response.Header, body)
	}
	claude.mu.Lock()
	defer claude.mu.Unlock()
	if claude.request["future_field"] == nil || claude.header.Get("Anthropic-Beta") == "" {
		t.Fatalf("native request lost unknown fields or headers: %v %v", claude.request, claude.header)
	}
}

func TestSanitizedContentReachesEveryTranslatedAttempt(t *testing.T) {
	upstream := &modelUpstream{replies: map[string]http.HandlerFunc{"model-a": answerStatus(529), "gpt-a": chatToolAnswer(&seen{})}}
	text := strings.Replace(fallbackConfig, "  bot:\n", "  bot:\n    fallback: [model-a, gpt-a]\n"+detectionSettings("email: redact"), 1)
	stage := &SensitiveStage{Scanner: sensitivedata.NewScanner()}
	fx := newFixture(t, startServer(t, upstream), fixtureOptions{config: text, sensitive: stage})
	email := "ana.canario@example.org"
	for _, c := range []struct{ path, format, body string }{
		{"/v1/messages", "anthropic", `{"model":"model-a","max_tokens":16,"messages":[{"role":"user","content":"` + testkit.Prompt("escreva para "+email) + `"}]}`},
		{"/v1/responses", "openai", `{"model":"model-a","input":"` + testkit.Prompt("escreva para "+email) + `"}`},
	} {
		response, body := fx.post(c.path, c.format, c.body)
		if response.StatusCode != 200 || response.Header.Get(translatedHeader) == "" {
			t.Fatalf("%s: %d %s", c.path, response.StatusCode, body)
		}
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if len(upstream.bodies) != 4 {
		t.Fatalf("attempts %d", len(upstream.bodies))
	}
	for i, body := range upstream.bodies {
		if strings.Contains(string(body), email) || !strings.Contains(string(body), testkit.Marker()) || !strings.Contains(string(body), "[EMAIL_") {
			t.Fatalf("attempt %d (%s) did not carry the sanitized content: %s", i, upstream.calls[i], body)
		}
	}
}
