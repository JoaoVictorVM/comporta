package translation

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type providerEvent struct{ name, data string }

type clientEvent struct {
	name string
	data map[string]any
	done bool
}

// translate runs provider events through a translator and parses every frame
// it produced, which also proves each frame is well formed.
func translate(t *testing.T, source, client WireFormat, options StreamOptions, events []providerEvent, usage Usage) []clientEvent {
	t.Helper()
	translator, err := NewStreamTranslator(source, client, options)
	if err != nil {
		t.Fatal(err)
	}
	var out []clientEvent
	for _, event := range events {
		frames, err := translator.Feed(event.name, []byte(event.data), usage)
		if err != nil {
			t.Fatalf("feed %s: %v", event.data, err)
		}
		for _, frame := range frames {
			out = append(out, parseFrame(t, string(frame)))
		}
	}
	if !translator.Done() {
		t.Fatal("stream not completed")
	}
	return out
}

func parseFrame(t *testing.T, frame string) clientEvent {
	t.Helper()
	if !strings.HasSuffix(frame, "\n\n") {
		t.Fatalf("frame not terminated: %q", frame)
	}
	var event clientEvent
	for _, line := range strings.Split(strings.TrimSuffix(frame, "\n\n"), "\n") {
		field, value, _ := strings.Cut(line, ": ")
		switch field {
		case "event":
			event.name = value
		case "data":
			if value == "[DONE]" {
				event.done = true
				continue
			}
			if err := json.Unmarshal([]byte(value), &event.data); err != nil {
				t.Fatalf("frame data is not JSON: %q", value)
			}
		default:
			t.Fatalf("unexpected line %q", line)
		}
	}
	return event
}

var anthropicToolStream = []providerEvent{
	{"message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","content":[],"stop_reason":null,"usage":{"input_tokens":9,"output_tokens":1}}}`},
	{"ping", `{"type":"ping"}`},
	{"content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`},
	{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Vou "}}`},
	{"content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ver."}}`},
	{"content_block_stop", `{"type":"content_block_stop","index":0}`},
	{"content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_9","name":"get_weather","input":{}}}`},
	{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}`},
	{"content_block_delta", `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"Recife\"}"}}`},
	{"content_block_stop", `{"type":"content_block_stop","index":1}`},
	{"message_delta", `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":15}}`},
	{"message_stop", `{"type":"message_stop"}`},
}

var chatToolStream = []providerEvent{
	{"", `{"id":"c1","object":"chat.completion.chunk","model":"gpt-x","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}`},
	{"", `{"id":"c1","object":"chat.completion.chunk","model":"gpt-x","choices":[{"index":0,"delta":{"content":"Vou "},"finish_reason":null}]}`},
	{"", `{"id":"c1","object":"chat.completion.chunk","model":"gpt-x","choices":[{"index":0,"delta":{"content":"ver."},"finish_reason":null}]}`},
	{"", `{"id":"c1","object":"chat.completion.chunk","model":"gpt-x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}`},
	{"", `{"id":"c1","object":"chat.completion.chunk","model":"gpt-x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"Recife\"}"}}]},"finish_reason":null}]}`},
	{"", `{"id":"c1","object":"chat.completion.chunk","model":"gpt-x","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"get_weather","arguments":"{\"city\":"}}]},"finish_reason":null}]}`},
	{"", `{"id":"c1","object":"chat.completion.chunk","model":"gpt-x","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"\"Natal\"}"}}]},"finish_reason":null}]}`},
	{"", `{"id":"c1","object":"chat.completion.chunk","model":"gpt-x","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`},
	{"", `{"id":"c1","object":"chat.completion.chunk","model":"gpt-x","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":20,"total_tokens":32}}`},
	{"", `[DONE]`},
}

func TestAnthropicToChatStreamLifecycle(t *testing.T) {
	usage := Usage{Input: Known(9), Output: Known(15)}
	events := translate(t, AnthropicMessages, OpenAIChat, StreamOptions{IncludeUsage: true, Now: time.Unix(1, 0)}, anthropicToolStream, usage)
	var text, arguments, finish string
	var sawUsage bool
	for _, event := range events[:len(events)-1] {
		if event.done || event.name != "" {
			t.Fatalf("unexpected frame %+v", event)
		}
		if event.data["model"] != "claude-x" || event.data["object"] != "chat.completion.chunk" {
			t.Fatalf("chunk envelope: %v", event.data)
		}
		choices := event.data["choices"].([]any)
		if len(choices) == 0 {
			sawUsage = event.data["usage"].(map[string]any)["completion_tokens"] == 15.0
			continue
		}
		choice := choices[0].(map[string]any)
		delta := choice["delta"].(map[string]any)
		if content, ok := delta["content"].(string); ok {
			text += content
		}
		if calls, ok := delta["tool_calls"].([]any); ok {
			call := calls[0].(map[string]any)
			if id, ok := call["id"]; ok && id != "toolu_9" {
				t.Fatalf("tool id lost: %v", id)
			}
			arguments += call["function"].(map[string]any)["arguments"].(string)
		}
		if reason, ok := choice["finish_reason"].(string); ok {
			finish = reason
		}
	}
	if text != "Vou ver." || arguments != `{"city":"Recife"}` || finish != "tool_calls" || !sawUsage || !events[len(events)-1].done {
		t.Fatalf("text=%q arguments=%q finish=%q usage=%v", text, arguments, finish, sawUsage)
	}
	without := translate(t, AnthropicMessages, OpenAIChat, StreamOptions{}, anthropicToolStream, usage)
	for _, event := range without {
		if !event.done && len(event.data["choices"].([]any)) == 0 {
			t.Fatal("usage chunk sent to a client that did not ask for it")
		}
	}
}

func TestChatToAnthropicStreamLifecycle(t *testing.T) {
	events := translate(t, OpenAIChat, AnthropicMessages, StreamOptions{}, chatToolStream, Usage{Input: Known(12), Output: Known(20)})
	var names []string
	arguments := map[float64]string{}
	for _, event := range events {
		names = append(names, event.name)
		if event.data["type"] != event.name {
			t.Fatalf("event name and type differ: %v", event)
		}
		if event.name == "content_block_delta" {
			delta := event.data["delta"].(map[string]any)
			if partial, ok := delta["partial_json"].(string); ok {
				arguments[event.data["index"].(float64)] += partial
			}
		}
	}
	want := "message_start content_block_start content_block_delta content_block_delta content_block_stop " +
		"content_block_start content_block_delta content_block_stop content_block_start content_block_delta content_block_delta content_block_stop " +
		"message_delta message_stop"
	if strings.Join(names, " ") != want {
		t.Fatalf("lifecycle:\n got %s\nwant %s", strings.Join(names, " "), want)
	}
	if arguments[1] != `{"city":"Recife"}` || arguments[2] != `{"city":"Natal"}` {
		t.Fatalf("arguments: %v", arguments)
	}
	final := events[len(events)-2].data
	if final["delta"].(map[string]any)["stop_reason"] != "tool_use" || final["usage"].(map[string]any)["output_tokens"] != 20.0 {
		t.Fatalf("message_delta: %v", final)
	}
}

func checkResponsesSequence(t *testing.T, events []clientEvent) map[string]any {
	t.Helper()
	for i, event := range events {
		if event.data["sequence_number"] != float64(i) || event.data["type"] != event.name {
			t.Fatalf("event %d: %v", i, event.data)
		}
	}
	if events[0].name != "response.created" || events[1].name != "response.in_progress" {
		t.Fatalf("lifecycle start: %s %s", events[0].name, events[1].name)
	}
	last := events[len(events)-1]
	if last.name != "response.completed" {
		t.Fatalf("lifecycle end: %s", last.name)
	}
	return last.data["response"].(map[string]any)
}

func TestAnthropicToResponsesStreamLifecycle(t *testing.T) {
	events := translate(t, AnthropicMessages, OpenAIResponses, StreamOptions{Now: time.Unix(1, 0)}, anthropicToolStream, Usage{Input: Known(9), Output: Known(15)})
	response := checkResponsesSequence(t, events)
	output := response["output"].([]any)
	message, call := output[0].(map[string]any), output[1].(map[string]any)
	text := message["content"].([]any)[0].(map[string]any)["text"]
	if text != "Vou ver." || call["arguments"] != `{"city":"Recife"}` || call["call_id"] != "toolu_9" || response["status"] != "completed" {
		t.Fatalf("final response: %v", response)
	}
	if response["usage"].(map[string]any)["output_tokens"] != 15.0 {
		t.Fatalf("usage: %v", response["usage"])
	}
}

func TestChatToResponsesFunctionCallStream(t *testing.T) {
	events := translate(t, OpenAIChat, OpenAIResponses, StreamOptions{}, chatToolStream, Usage{Input: Known(12), Output: Known(20)})
	response := checkResponsesSequence(t, events)
	var deltas, dones int
	for _, event := range events {
		switch event.name {
		case "response.function_call_arguments.delta":
			deltas++
		case "response.function_call_arguments.done":
			dones++
		}
	}
	output := response["output"].([]any)
	if len(output) != 3 || deltas != 3 || dones != 2 || output[2].(map[string]any)["arguments"] != `{"city":"Natal"}` {
		t.Fatalf("output=%v deltas=%d dones=%d", output, deltas, dones)
	}
}

func TestStreamChunkBoundaries(t *testing.T) {
	// Arguments split one character per event must arrive whole.
	events := []providerEvent{anthropicToolStream[0], anthropicToolStream[6]}
	for _, char := range `{"city":"Recife"}` {
		data, _ := json.Marshal(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": string(char)}})
		events = append(events, providerEvent{"content_block_delta", string(data)})
	}
	events = append(events, anthropicToolStream[9:]...)
	out := translate(t, AnthropicMessages, OpenAIResponses, StreamOptions{}, events, Usage{})
	response := checkResponsesSequence(t, out)
	if response["output"].([]any)[0].(map[string]any)["arguments"] != `{"city":"Recife"}` {
		t.Fatalf("arguments: %v", response["output"])
	}
}

func TestStreamCRLFAndComments(t *testing.T) {
	translator, _ := NewStreamTranslator(OpenAIChat, AnthropicMessages, StreamOptions{})
	for _, data := range []string{"", " "} {
		frames, err := translator.Feed("", []byte(data), Usage{})
		if err == nil && len(frames) > 0 {
			t.Fatal("an empty event produced output")
		}
	}
	translator, _ = NewStreamTranslator(AnthropicMessages, OpenAIChat, StreamOptions{})
	if frames, err := translator.Feed("ping", []byte(`{"type":"ping"}`), Usage{}); err != nil || len(frames) != 0 {
		t.Fatalf("ping: %v %v", frames, err)
	}
	if _, err := translator.Feed("", []byte(`{"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"x"}}`), Usage{}); err == nil {
		t.Fatal("reasoning in a stream must fail the translation")
	}
}

func TestStreamEventLimit(t *testing.T) {
	translator, _ := NewStreamTranslator(AnthropicMessages, OpenAIResponses, StreamOptions{MaxOutputBytes: 8})
	for _, event := range anthropicToolStream[:3] {
		if _, err := translator.Feed(event.name, []byte(event.data), Usage{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := translator.Feed("content_block_delta", []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"0123456789"}}`), Usage{}); err == nil {
		t.Fatal("output over the limit was accepted")
	}
	errorFrame := string(translator.ErrorEvent("response_translation_failed", "response translation failed"))
	if !strings.HasPrefix(errorFrame, "event: error\n") || !strings.Contains(errorFrame, `"code":"response_translation_failed"`) {
		t.Fatalf("error frame: %q", errorFrame)
	}
}
