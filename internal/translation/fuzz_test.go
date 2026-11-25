package translation

import (
	"encoding/json"
	"testing"
	"time"
)

func FuzzParseRenderRequest(f *testing.F) {
	f.Add(uint8(0), []byte(`{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"q"}]}`))
	f.Add(uint8(1), []byte(`{"model":"g","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AA"}}]}]}`))
	f.Add(uint8(2), []byte(`{"model":"g","input":[{"type":"function_call","call_id":"c","name":"f","arguments":"{}"}]}`))
	formats := []WireFormat{AnthropicMessages, OpenAIChat, OpenAIResponses}
	f.Fuzz(func(t *testing.T, which uint8, body []byte) {
		source := formats[int(which)%len(formats)]
		request, err := ParseRequest(source, body, ParseOptions{})
		if err != nil {
			return
		}
		for _, target := range []WireFormat{AnthropicMessages, OpenAIChat} {
			if target != source && Analyze(request, target) != "" {
				if _, err := RenderRequest(request, target, "m"); err == nil {
					t.Fatal("an ineligible request was rendered")
				}
				continue
			}
			if target == source && source == OpenAIResponses {
				continue
			}
			out, err := RenderRequest(request, target, "m")
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if !json.Valid(out) {
				t.Fatalf("invalid JSON: %s", out)
			}
			again, err := ParseRequest(target, out, ParseOptions{})
			if err != nil || countToolCalls(again) != countToolCalls(request) {
				t.Fatalf("tool calls lost: %v", err)
			}
		}
	})
}

func countToolCalls(r *Request) int {
	n := 0
	for _, turn := range r.Turns {
		for _, block := range turn.Blocks {
			if block.Kind == BlockToolCall || block.Kind == BlockToolResult {
				n++
			}
		}
	}
	return n
}

func FuzzParseRenderResponse(f *testing.F) {
	f.Add(true, []byte(`{"type":"message","model":"m","content":[{"type":"text","text":"x"}],"stop_reason":"end_turn"}`))
	f.Add(false, []byte(`{"model":"m","choices":[{"message":{"role":"assistant","content":"x"},"finish_reason":"stop"}]}`))
	f.Fuzz(func(t *testing.T, anthropic bool, body []byte) {
		source := OpenAIChat
		if anthropic {
			source = AnthropicMessages
		}
		response, err := ParseResponse(source, body)
		if err != nil {
			return
		}
		for _, client := range []WireFormat{AnthropicMessages, OpenAIChat, OpenAIResponses} {
			out, err := RenderResponse(response, client, RenderOptions{Now: time.Unix(0, 0)})
			if err != nil {
				t.Fatalf("a parsed response must render: %v", err)
			}
			if !json.Valid(out) {
				t.Fatalf("invalid JSON: %s", out)
			}
		}
	})
}

func FuzzStreamTranslator(f *testing.F) {
	f.Add(true, []byte(anthropicToolStream[3].data))
	f.Add(false, []byte(chatToolStream[4].data))
	f.Fuzz(func(t *testing.T, anthropic bool, data []byte) {
		source, prefix := OpenAIChat, chatToolStream[:1]
		if anthropic {
			source, prefix = AnthropicMessages, anthropicToolStream[:3]
		}
		for _, client := range []WireFormat{AnthropicMessages, OpenAIChat, OpenAIResponses} {
			if client == source {
				continue
			}
			translator, _ := NewStreamTranslator(source, client, StreamOptions{MaxOutputBytes: 1 << 20})
			for _, event := range prefix {
				if _, err := translator.Feed(event.name, []byte(event.data), Usage{}); err != nil {
					t.Fatal(err)
				}
			}
			frames, err := translator.Feed("", data, Usage{})
			if err != nil {
				continue
			}
			for _, frame := range frames {
				if len(frame) < 2 || string(frame[len(frame)-2:]) != "\n\n" {
					t.Fatalf("frame not terminated: %q", frame)
				}
			}
		}
	})
}
