package translation

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite the expected golden files")

// normalize removes the values the gateway generates per answer, so goldens
// compare what was translated and not the random identifiers around it.
func normalize(t *testing.T, data []byte) any {
	t.Helper()
	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	var walk func(any) any
	walk = func(v any) any {
		switch typed := v.(type) {
		case map[string]any:
			for key, member := range typed {
				switch key {
				case "created", "created_at":
					typed[key] = "<time>"
				case "id":
					if text, ok := member.(string); ok && len(text) > 20 && !portableFixtureID[text] {
						typed[key] = "<id>"
					}
				default:
					typed[key] = walk(member)
				}
			}
		case []any:
			for i, member := range typed {
				typed[i] = walk(member)
			}
		}
		return v
	}
	return walk(value)
}

// portableFixtureID lists identifiers that come from the fixtures and must be
// kept as they are.
var portableFixtureID = map[string]bool{"call_0123456789abcdef": true}

func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name)
	if *update {
		var pretty any
		_ = json.Unmarshal(got, &pretty)
		data, _ := json.MarshalIndent(pretty, "", "  ")
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("missing golden %s: %v", name, err)
	}
	if !reflect.DeepEqual(normalize(t, got), normalize(t, want)) {
		t.Fatalf("%s differs\n got: %s\nwant: %s", name, got, want)
	}
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "golden", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestGoldenRequestsBothDirections(t *testing.T) {
	cases := []struct {
		input  string
		source WireFormat
		target WireFormat
		model  string
		want   string
	}{
		{"anthropic_tools.request.json", AnthropicMessages, OpenAIChat, "gpt-4.1", "anthropic_tools.to_chat.json"},
		{"chat_tools.request.json", OpenAIChat, AnthropicMessages, "claude-sonnet-4-5", "chat_tools.to_anthropic.json"},
		{"responses_tools.request.json", OpenAIResponses, OpenAIChat, "gpt-4.1", "responses_tools.to_chat.json"},
		{"responses_tools.request.json", OpenAIResponses, AnthropicMessages, "claude-sonnet-4-5", "responses_tools.to_anthropic.json"},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			request, err := ParseRequest(c.source, fixture(t, c.input), ParseOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if feature := Analyze(request, c.target); feature != "" {
				t.Fatalf("fixture is not representable: %s", feature)
			}
			body, err := RenderRequest(request, c.target, c.model)
			if err != nil {
				t.Fatal(err)
			}
			checkGolden(t, c.want, body)
			// The rendered body is itself a valid request of the target protocol
			// and carries the same conversation.
			again, err := ParseRequest(c.target, body, ParseOptions{})
			if err != nil || again.Blocked() != "" {
				t.Fatalf("rendered body does not parse back: %v %s", err, again.Blocked())
			}
			if len(again.Tools) != len(request.Tools) || countBlocks(again) != countBlocks(request) {
				t.Fatalf("content lost: %d/%d tools, %d/%d blocks", len(again.Tools), len(request.Tools), countBlocks(again), countBlocks(request))
			}
		})
	}
}

func countBlocks(r *Request) int {
	n := len(r.Instructions)
	for _, turn := range r.Turns {
		for _, block := range turn.Blocks {
			n++
			n += len(block.Result)
		}
	}
	return n
}

func TestGoldenResponsesBothDirections(t *testing.T) {
	now := time.Unix(1795478400, 0)
	cases := []struct {
		input  string
		source WireFormat
		client WireFormat
		usage  Usage
		want   string
	}{
		{"chat_tool_call.response.json", OpenAIChat, AnthropicMessages, Usage{Input: Known(30), Output: Known(12), CacheRead: Known(0)}, "chat_tool_call.to_anthropic.json"},
		{"chat_tool_call.response.json", OpenAIChat, OpenAIResponses, Usage{Input: Known(30), Output: Known(12), CacheRead: Known(0)}, "chat_tool_call.to_responses.json"},
		{"anthropic_text.response.json", AnthropicMessages, OpenAIChat, Usage{Input: Known(20), Output: Known(6), CacheRead: Known(4), CacheWrite: Known(0)}, "anthropic_text.to_chat.json"},
		{"anthropic_text.response.json", AnthropicMessages, OpenAIResponses, Usage{Input: Known(20), Output: Known(6), CacheRead: Known(4), CacheWrite: Known(0)}, "anthropic_text.to_responses.json"},
	}
	for _, c := range cases {
		t.Run(c.want, func(t *testing.T) {
			response, err := ParseResponse(c.source, fixture(t, c.input))
			if err != nil {
				t.Fatal(err)
			}
			body, err := RenderResponse(response, c.client, RenderOptions{Model: "fallback", Now: now, Usage: c.usage})
			if err != nil {
				t.Fatal(err)
			}
			checkGolden(t, c.want, body)
		})
	}
}
