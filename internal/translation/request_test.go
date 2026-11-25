package translation

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func mustParse(t *testing.T, format WireFormat, body string) *Request {
	t.Helper()
	request, err := ParseRequest(format, []byte(body), ParseOptions{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return request
}

func mustRender(t *testing.T, r *Request, target WireFormat, model string) map[string]any {
	t.Helper()
	body, err := RenderRequest(r, target, model)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("rendered body is not JSON: %v", err)
	}
	return out
}

// roundTrip renders a request in its own protocol and parses it again: the
// canonical form must not change.
func roundTrip(t *testing.T, format WireFormat, body string) {
	t.Helper()
	first := mustParse(t, format, body)
	if first.Blocked() != "" {
		t.Fatalf("fixture blocked: %s", first.Blocked())
	}
	rendered, err := RenderRequest(first, format, first.Model)
	if err != nil {
		t.Fatal(err)
	}
	second := mustParse(t, format, string(rendered))
	second.IncludeUsage = first.IncludeUsage
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("round trip changed the request:\n%s", rendered)
	}
}

func TestAnthropicRequestRoundTripNative(t *testing.T) {
	roundTrip(t, AnthropicMessages, string(fixture(t, "anthropic_tools.request.json")))
}

func TestAnthropicSystemImagesAndTools(t *testing.T) {
	r := mustParse(t, AnthropicMessages, `{"model":"m","max_tokens":10,
		"system":[{"type":"text","text":"a","cache_control":{"type":"ephemeral"}},{"type":"text","text":"b"}],
		"messages":[{"role":"user","content":[{"type":"image","source":{"type":"url","url":"https://example.com/x.png"}},{"type":"text","text":"q"}]}],
		"tools":[{"name":"f","input_schema":{"type":"object"},"cache_control":{"type":"ephemeral"}}],
		"tool_choice":{"type":"tool","name":"f"}}`)
	if r.Blocked() != "" {
		t.Fatalf("cache hints must not block: %s", r.Blocked())
	}
	if !reflect.DeepEqual(r.Instructions, []string{"a", "b"}) {
		t.Fatalf("instructions: %v", r.Instructions)
	}
	chat := mustRender(t, r, OpenAIChat, "gpt")
	messages := chat["messages"].([]any)
	if len(messages) != 3 || messages[0].(map[string]any)["role"] != "system" || messages[1].(map[string]any)["content"] != "b" {
		t.Fatalf("system order lost: %v", messages)
	}
	parts := messages[2].(map[string]any)["content"].([]any)
	if parts[0].(map[string]any)["image_url"].(map[string]any)["url"] != "https://example.com/x.png" || parts[1].(map[string]any)["text"] != "q" {
		t.Fatalf("content order lost: %v", parts)
	}
	choice := chat["tool_choice"].(map[string]any)
	if choice["function"].(map[string]any)["name"] != "f" {
		t.Fatalf("named choice lost: %v", choice)
	}
}

func TestAnthropicToolResultCorrelation(t *testing.T) {
	r := mustParse(t, AnthropicMessages, `{"model":"m","max_tokens":10,"messages":[
		{"role":"user","content":"q"},
		{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"f","input":{}},{"type":"tool_use","id":"id with space","name":"f","input":{"a":1}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"r1"}]},{"type":"tool_result","tool_use_id":"id with space","content":"r2"}]}]}`)
	chat := mustRender(t, r, OpenAIChat, "gpt")
	messages := chat["messages"].([]any)
	calls := messages[1].(map[string]any)["tool_calls"].([]any)
	first, second := calls[0].(map[string]any)["id"].(string), calls[1].(map[string]any)["id"].(string)
	if first != "toolu_1" || second == "id with space" || !portableID.MatchString(second) {
		t.Fatalf("ids: %q %q", first, second)
	}
	if messages[2].(map[string]any)["tool_call_id"] != first || messages[3].(map[string]any)["tool_call_id"] != second {
		t.Fatalf("results lost their calls: %v", messages)
	}
}

func TestChatRequestRoundTripNative(t *testing.T) {
	roundTrip(t, OpenAIChat, `{"model":"gpt","messages":[{"role":"system","content":"s"},{"role":"user","content":"q"},
		{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_1","content":"r"}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}],
		"tool_choice":"required","max_tokens":5,"user":"u"}`)
}

func TestChatDeveloperAndSystemOrder(t *testing.T) {
	r := mustParse(t, OpenAIChat, `{"model":"gpt","messages":[{"role":"developer","content":"d"},{"role":"system","content":"s"},{"role":"user","content":"q"}]}`)
	if !reflect.DeepEqual(r.Instructions, []string{"d", "s"}) {
		t.Fatalf("instructions: %v", r.Instructions)
	}
	late := mustParse(t, OpenAIChat, `{"model":"gpt","messages":[{"role":"user","content":"q"},{"role":"system","content":"s"}]}`)
	if Analyze(late, AnthropicMessages) != FeatureUnsupportedRole {
		t.Fatalf("a system message in the middle has no equivalent: %s", Analyze(late, AnthropicMessages))
	}
}

func TestChatParallelToolCalls(t *testing.T) {
	for _, c := range []struct {
		body string
		want any
	}{
		{`"parallel_tool_calls":false`, true},
		{`"parallel_tool_calls":true`, nil},
		{`"parallel_tool_calls":false,"tool_choice":"none"`, nil},
	} {
		r := mustParse(t, OpenAIChat, `{"model":"gpt","messages":[{"role":"user","content":"q"}],"tools":[{"type":"function","function":{"name":"f"}}],`+c.body+`}`)
		anthropic := mustRender(t, r, AnthropicMessages, "claude")
		choice, _ := anthropic["tool_choice"].(map[string]any)
		if got := choice["disable_parallel_tool_use"]; got != c.want {
			t.Fatalf("%s: disable_parallel_tool_use = %v", c.body, got)
		}
	}
	r := mustParse(t, AnthropicMessages, `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"q"}],"tools":[{"name":"f","input_schema":{"type":"object"}}],"tool_choice":{"type":"auto","disable_parallel_tool_use":true}}`)
	if chat := mustRender(t, r, OpenAIChat, "gpt"); chat["parallel_tool_calls"] != false {
		t.Fatalf("parallel flag not inverted: %v", chat["parallel_tool_calls"])
	}
}

func TestResponsesStringAndItemInput(t *testing.T) {
	r := mustParse(t, OpenAIResponses, `{"model":"gpt","input":"olá","instructions":"seja breve"}`)
	if len(r.Turns) != 1 || r.Turns[0].Blocks[0].Text != "olá" || r.Instructions[0] != "seja breve" {
		t.Fatalf("string input: %+v", r.Turns)
	}
	r = mustParse(t, OpenAIResponses, `{"model":"gpt","input":[{"role":"developer","content":"d"},{"role":"user","content":[{"type":"input_text","text":"q"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"a","annotations":[]}]},{"role":"user","content":"q2"}]}`)
	if r.Blocked() != "" || len(r.Turns) != 3 || r.Instructions[0] != "d" {
		t.Fatalf("item input: blocked=%s turns=%d", r.Blocked(), len(r.Turns))
	}
	for _, bad := range []string{`{"model":"gpt"}`, `{"model":"gpt","input":5}`, `{"model":"gpt","input":[1]}`,
		`{"model":"gpt","input":[{"type":"function_call","name":"f"}]}`, `{"model":"gpt","input":"q","tools":{}}`} {
		if _, err := ParseRequest(OpenAIResponses, []byte(bad), ParseOptions{}); err == nil {
			t.Fatalf("malformed body accepted: %s", bad)
		}
	}
}

func TestResponsesFunctionLoop(t *testing.T) {
	r := mustParse(t, OpenAIResponses, string(fixture(t, "responses_tools.request.json")))
	if len(r.Turns) != 3 || r.Turns[1].Blocks[0].Kind != BlockToolCall || r.Turns[2].Blocks[0].Kind != BlockToolResult {
		t.Fatalf("function loop: %+v", r.Turns)
	}
	if r.Turns[1].Blocks[0].ToolCallID != r.Turns[2].Blocks[0].ToolCallID {
		t.Fatal("call and output lost their correlation")
	}
}

func TestResponsesRejectsStatefulFeatures(t *testing.T) {
	for member, want := range map[string]Feature{
		`"previous_response_id":"resp_1"`:           FeatureStatefulResponse,
		`"store":true`:                              FeatureStatefulResponse,
		`"background":true`:                         FeatureStatefulResponse,
		`"conversation":"conv_1"`:                   FeatureStatefulResponse,
		`"include":["reasoning.encrypted_content"]`: FeatureStatefulResponse,
		`"reasoning":{"effort":"low"}`:              FeatureReasoning,
		`"tools":[{"type":"web_search"}]`:           FeatureToolType,
		`"text":{"format":{"type":"json_schema"}}`:  FeatureStructuredOutput,
	} {
		r := mustParse(t, OpenAIResponses, `{"model":"gpt","input":"q",`+member+`}`)
		for _, target := range []WireFormat{OpenAIChat, AnthropicMessages} {
			if got := Analyze(r, target); got != want {
				t.Fatalf("%s to %s: got %q, want %q", member, target, got, want)
			}
		}
	}
	plain := mustParse(t, OpenAIResponses, `{"model":"gpt","input":"q","store":false,"text":{"format":{"type":"text"}},"include":[]}`)
	if Analyze(plain, OpenAIChat) != "" {
		t.Fatalf("defaults must not block: %s", Analyze(plain, OpenAIChat))
	}
}

func TestCompatibilityMatrix(t *testing.T) {
	cases := []struct {
		format WireFormat
		body   string
		target WireFormat
		want   Feature
	}{
		{AnthropicMessages, `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"q"}]}`, OpenAIChat, ""},
		{AnthropicMessages, `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"thinking","thinking":"x"}]}]}`, OpenAIChat, FeatureReasoning},
		{AnthropicMessages, `{"model":"m","max_tokens":1,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[]}`, OpenAIChat, FeatureReasoning},
		{AnthropicMessages, `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"document","source":{}}]}]}`, OpenAIChat, FeatureFile},
		{AnthropicMessages, `{"model":"m","max_tokens":1,"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[]}`, OpenAIChat, FeatureToolType},
		{AnthropicMessages, `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":[{"type":"image","source":{"type":"url","url":"https://x"}}]}]}]}`, OpenAIChat, FeatureToolResult},
		{AnthropicMessages, `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","is_error":true,"content":"x"}]}]}`, OpenAIChat, FeatureToolResult},
		{AnthropicMessages, `{"model":"m","max_tokens":1,"tool_choice":{"type":"any"},"messages":[]}`, OpenAIChat, FeatureToolChoice},
		{OpenAIChat, `{"model":"g","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/bmp;base64,AAAA"}}]}]}`, AnthropicMessages, FeatureImageSource},
		{OpenAIChat, `{"model":"g","messages":[{"role":"user","content":[{"type":"input_audio","input_audio":{}}]}]}`, AnthropicMessages, FeatureAudio},
		{OpenAIChat, `{"model":"g","messages":[],"response_format":{"type":"json_schema"}}`, AnthropicMessages, FeatureStructuredOutput},
		{OpenAIChat, `{"model":"g","messages":[],"response_format":{"type":"text"}}`, AnthropicMessages, ""},
		{OpenAIChat, `{"model":"g","messages":[],"functions":[]}`, AnthropicMessages, FeatureToolType},
		{OpenAIChat, `{"model":"g","messages":[{"role":"user","content":"q"}],"n":2}`, AnthropicMessages, FeatureUnknownField},
		{OpenAIChat, `{"model":"g","messages":[{"role":"user","content":"q"}],"seed":null,"n":1}`, AnthropicMessages, ""},
		{OpenAIChat, `{"model":"g","messages":[{"role":"user","content":"q"}],"tool_choice":{"type":"function","function":{"name":"missing"}}}`, AnthropicMessages, FeatureToolChoice},
		{OpenAIChat, `{"model":"g","messages":[{"role":"assistant","content":"x","refusal":"no"}]}`, AnthropicMessages, FeatureUnsupportedBlock},
	}
	for _, c := range cases {
		r := mustParse(t, c.format, c.body)
		if got := Analyze(r, c.target); got != c.want {
			t.Errorf("%s -> %s: got %q, want %q\n%s", c.format, c.target, got, c.want, c.body)
		}
		if Analyze(r, c.format) != "" {
			t.Errorf("a request is always representable in its own protocol: %s", c.body)
		}
	}
	for _, feature := range Features() {
		if !feature.Valid() || strings.ToLower(string(feature)) != string(feature) {
			t.Fatalf("feature code %q is not a stable closed value", feature)
		}
	}
}

func TestUnknownSemanticFieldIsIneligible(t *testing.T) {
	r := mustParse(t, AnthropicMessages, `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"q"}],"future_field":{"x":1}}`)
	if Analyze(r, OpenAIChat) != FeatureUnknownField {
		t.Fatalf("an unknown member must block translation, got %q", Analyze(r, OpenAIChat))
	}
	if _, err := RenderRequest(r, OpenAIChat, "g"); err == nil {
		t.Fatal("an ineligible request was rendered")
	}
	if _, err := RenderRequest(r, AnthropicMessages, "m"); err != nil {
		t.Fatal("the native protocol must still render")
	}
}

func TestProviderBetaIsIneligible(t *testing.T) {
	r, err := ParseRequest(AnthropicMessages, []byte(`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"q"}]}`), ParseOptions{ProviderBeta: true})
	if err != nil {
		t.Fatal(err)
	}
	if Analyze(r, OpenAIChat) != FeatureProviderBeta {
		t.Fatalf("beta must block: %q", Analyze(r, OpenAIChat))
	}
}

func TestRequestDiagnosticsHoldNoContent(t *testing.T) {
	r := mustParse(t, AnthropicMessages, `{"model":"m","max_tokens":1,"system":"CANARY-SYSTEM","messages":[{"role":"user","content":"CANARY-TEXT"}],"tools":[{"name":"CANARY-TOOL","input_schema":{"type":"object"}}]}`)
	data, _ := json.Marshal(r)
	block, _ := json.Marshal(r.Turns[0].Blocks[0])
	for _, out := range []string{fmt.Sprintf("%v %+v %#v %s", *r, *r, *r, *r), string(data), fmt.Sprintf("%v", r.Turns[0].Blocks[0]), string(block)} {
		if strings.Contains(out, "CANARY") {
			t.Fatalf("diagnostic output leaked content: %s", out)
		}
	}
}
