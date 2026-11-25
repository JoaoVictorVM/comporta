package translation

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func render(t *testing.T, r *Response, client WireFormat, usage Usage) map[string]any {
	t.Helper()
	body, err := RenderResponse(r, client, RenderOptions{Model: "m", Now: time.Unix(1, 0), Usage: usage})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestStopReasonMapping(t *testing.T) {
	cases := []struct {
		anthropic, chat, status string
		stop                    StopReason
	}{
		{"end_turn", "stop", "completed", StopEnd},
		{"max_tokens", "length", "incomplete", StopMaxTokens},
		{"tool_use", "tool_calls", "completed", StopToolUse},
		{"refusal", "content_filter", "incomplete", StopContentFilter},
	}
	for _, c := range cases {
		fromAnthropic, err := ParseResponse(AnthropicMessages, []byte(fmt.Sprintf(`{"type":"message","model":"m","content":[],"stop_reason":%q}`, c.anthropic)))
		if err != nil || fromAnthropic.Stop != c.stop {
			t.Fatalf("anthropic %s: %v %v", c.anthropic, fromAnthropic, err)
		}
		fromChat, err := ParseResponse(OpenAIChat, []byte(fmt.Sprintf(`{"model":"m","choices":[{"message":{"role":"assistant","content":"x"},"finish_reason":%q}]}`, c.chat)))
		if err != nil || fromChat.Stop != c.stop {
			t.Fatalf("chat %s: %v %v", c.chat, fromChat, err)
		}
		if got := render(t, fromChat, AnthropicMessages, Usage{})["stop_reason"]; got != c.anthropic {
			t.Fatalf("chat %s rendered as %v", c.chat, got)
		}
		if got := render(t, fromAnthropic, OpenAIChat, Usage{})["choices"].([]any)[0].(map[string]any)["finish_reason"]; got != c.chat {
			t.Fatalf("anthropic %s rendered as %v", c.anthropic, got)
		}
		if got := render(t, fromAnthropic, OpenAIResponses, Usage{})["status"]; got != c.status {
			t.Fatalf("anthropic %s rendered as status %v", c.anthropic, got)
		}
	}
	sequence, err := ParseResponse(AnthropicMessages, []byte(`{"type":"message","model":"m","content":[],"stop_reason":"stop_sequence","stop_sequence":"FIM"}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := render(t, sequence, OpenAIChat, Usage{})["choices"].([]any)[0].(map[string]any)["finish_reason"]; got != "stop" {
		t.Fatalf("stop sequence rendered as %v", got)
	}
}

func TestUsageProjectionKeepsNativeTotals(t *testing.T) {
	usage := Usage{Input: Known(100), Output: Known(20), CacheRead: Known(30), CacheWrite: Known(5)}
	response := &Response{Model: "m", Blocks: []Block{{Kind: BlockText, Text: "x"}}, Stop: StopEnd}
	chat := render(t, response, OpenAIChat, usage)["usage"].(map[string]any)
	if chat["prompt_tokens"] != 135.0 || chat["completion_tokens"] != 20.0 || chat["total_tokens"] != 155.0 ||
		chat["prompt_tokens_details"].(map[string]any)["cached_tokens"] != 30.0 {
		t.Fatalf("chat usage: %v", chat)
	}
	anthropic := render(t, response, AnthropicMessages, usage)["usage"].(map[string]any)
	if anthropic["input_tokens"] != 100.0 || anthropic["output_tokens"] != 20.0 || anthropic["cache_read_input_tokens"] != 30.0 || anthropic["cache_creation_input_tokens"] != 5.0 {
		t.Fatalf("anthropic usage: %v", anthropic)
	}
	responses := render(t, response, OpenAIResponses, usage)["usage"].(map[string]any)
	if responses["input_tokens"] != 135.0 || responses["output_tokens"] != 20.0 || responses["total_tokens"] != 155.0 {
		t.Fatalf("responses usage: %v", responses)
	}
	unknown := render(t, response, AnthropicMessages, Usage{Input: Known(1), Output: Known(2)})["usage"].(map[string]any)
	if _, invented := unknown["cache_read_input_tokens"]; invented {
		t.Fatalf("an unreported counter was invented: %v", unknown)
	}
}

func TestUnsupportedResponseShapeFails(t *testing.T) {
	for _, body := range []string{
		`{"type":"message","content":[{"type":"thinking","thinking":"x"}],"stop_reason":"end_turn"}`,
		`{"type":"message","content":[{"type":"text","text":"x","citations":[{"type":"char_location"}]}],"stop_reason":"end_turn"}`,
		`{"type":"message","content":[{"type":"server_tool_use","id":"x"}],"stop_reason":"end_turn"}`,
		`{"type":"message","content":[],"stop_reason":"pause_turn"}`,
	} {
		if _, err := ParseResponse(AnthropicMessages, []byte(body)); err == nil {
			t.Fatalf("accepted: %s", body)
		}
	}
	for _, body := range []string{
		`{"choices":[{"message":{"role":"assistant","content":null,"refusal":"no"},"finish_reason":"stop"}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"x"},"finish_reason":"stop"},{"message":{"role":"assistant","content":"y"},"finish_reason":"stop"}]}`,
		`{"choices":[{"message":{"role":"assistant","tool_calls":[{"id":"c","type":"function","function":{"name":"f","arguments":"not json"}}]},"finish_reason":"tool_calls"}]}`,
		`{"choices":[{"message":{"role":"assistant","content":"x","annotations":[{"type":"url_citation"}]},"finish_reason":"stop"}]}`,
	} {
		_, err := ParseResponse(OpenAIChat, []byte(body))
		if err == nil {
			t.Fatalf("accepted: %s", body)
		}
		if strings.Contains(err.Error(), "not json") || strings.Contains(err.Error(), "url_citation") {
			t.Fatalf("error echoes provider bytes: %v", err)
		}
	}
}

func TestToolIDsStableWithinOperation(t *testing.T) {
	ids := NewIDMap(4)
	first, _ := ids.Map("id with spaces")
	again, _ := ids.Map("id with spaces")
	if first != again || !portableID.MatchString(first) {
		t.Fatalf("ids not stable: %q %q", first, again)
	}
	if kept, _ := ids.Map("call_abc"); kept != "call_abc" {
		t.Fatalf("a portable id must be kept: %q", kept)
	}
	if ToolID("id with spaces") != first {
		t.Fatal("ids must be the same across requests")
	}
}

func TestToolIDsDoNotCollide(t *testing.T) {
	seen := map[string]string{}
	for i := range 2000 {
		original := fmt.Sprintf("id %d/x", i)
		mapped := ToolID(original)
		if previous, ok := seen[mapped]; ok {
			t.Fatalf("%q and %q map to %q", previous, original, mapped)
		}
		seen[mapped] = original
	}
}

func TestIDMapHasBound(t *testing.T) {
	ids := NewIDMap(2)
	_, _ = ids.Map("a")
	_, _ = ids.Map("b")
	if _, ok := ids.Map("c"); ok || ids.Len() != 2 {
		t.Fatal("the map grew past its bound")
	}
	if _, ok := ids.Map("a"); !ok {
		t.Fatal("a known id must still resolve")
	}
}
