package fakeprovider

import (
	"context"
	"fmt"
	"time"
)

// Scenario selects how the fake behaves. It is chosen when the handler is created,
// never by anything in a request, so a client of the gateway cannot select one.
type Scenario string

const (
	ScenarioSuccess             Scenario = "success"
	ScenarioRateLimit           Scenario = "rate-limit"
	ScenarioOverload            Scenario = "overload"
	ScenarioHeaderTimeout       Scenario = "header-timeout"
	ScenarioSlowStream          Scenario = "slow-stream"
	ScenarioDisconnectMidstream Scenario = "disconnect-midstream"
	// ScenarioAgentLoop answers every conversation with the same tool call, the
	// shape of an agent stuck repeating one step. AgentLoop is its client side.
	ScenarioAgentLoop Scenario = "agent-loop"
)

// Scenarios lists every valid scenario.
func Scenarios() []Scenario {
	return []Scenario{
		ScenarioSuccess, ScenarioRateLimit, ScenarioOverload,
		ScenarioHeaderTimeout, ScenarioSlowStream, ScenarioDisconnectMidstream, ScenarioAgentLoop,
	}
}

// ParseScenario validates a scenario name.
func ParseScenario(name string) (Scenario, error) {
	for _, scenario := range Scenarios() {
		if string(scenario) == name {
			return scenario, nil
		}
	}
	return "", fmt.Errorf("unknown scenario; use one of %v", Scenarios())
}

// Deterministic token counts of every fixture.
const (
	FixtureInputTokens  = 10
	FixtureOutputTokens = 2
	fixtureText         = "Olá"
	fixtureSecondText   = " mundo"
)

const defaultSlowDelay = 100 * time.Millisecond

// sleepContext is the default wait between slow events.
func sleepContext(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func anthropicMessage(model string) string {
	return fmt.Sprintf(`{"id":"msg_fake_1","type":"message","role":"assistant","model":%q,"content":[{"type":"text","text":%q}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":%d,"output_tokens":%d}}`,
		model, fixtureText, FixtureInputTokens, FixtureOutputTokens)
}

func openAICompletion(model string) string {
	return fmt.Sprintf(`{"id":"chatcmpl_fake_1","object":"chat.completion","created":0,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
		model, fixtureText, FixtureInputTokens, FixtureOutputTokens, FixtureInputTokens+FixtureOutputTokens)
}

// Fixed tool call of the agent-loop scenario. The IDs never change, so a client
// that keeps answering with the same result keeps the same loop signature.
const (
	LoopToolName   = "read_status"
	LoopToolUseID  = "toolu_fake_loop"
	LoopToolCallID = "call_fake_loop"
	LoopToolInput  = `{"path":"status.txt"}`
	LoopToolResult = "status: pending"
)

func anthropicToolUse(model string) string {
	return fmt.Sprintf(`{"id":"msg_fake_loop","type":"message","role":"assistant","model":%q,"content":[{"type":"tool_use","id":%q,"name":%q,"input":%s}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":%d,"output_tokens":%d}}`,
		model, LoopToolUseID, LoopToolName, LoopToolInput, FixtureInputTokens, FixtureOutputTokens)
}

func openAIToolCall(model string) string {
	return fmt.Sprintf(`{"id":"chatcmpl_fake_loop","object":"chat.completion","created":0,"model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":%q,"type":"function","function":{"name":%q,"arguments":%q}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`,
		model, LoopToolCallID, LoopToolName, LoopToolInput, FixtureInputTokens, FixtureOutputTokens, FixtureInputTokens+FixtureOutputTokens)
}

func anthropicToolEvents(model string) []string {
	event := func(name, data string) string { return "event: " + name + "\ndata: " + data + "\n\n" }
	return []string{
		event("message_start", fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_fake_loop","type":"message","role":"assistant","model":%q,"content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":%d,"output_tokens":1}}}`, model, FixtureInputTokens)),
		event("content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":%q,"name":%q,"input":{}}}`, LoopToolUseID, LoopToolName)),
		event("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":%q}}`, LoopToolInput)),
		event("content_block_stop", `{"type":"content_block_stop","index":0}`),
		event("message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":%d}}`, FixtureOutputTokens)),
		event("message_stop", `{"type":"message_stop"}`),
	}
}

func openAIToolEvents(model string, includeUsage bool) []string {
	chunk := func(delta, finish string) string {
		return fmt.Sprintf(`data: {"id":"chatcmpl_fake_loop","object":"chat.completion.chunk","created":0,"model":%q,"choices":[{"index":0,"delta":%s,"finish_reason":%s}]}`+"\n\n", model, delta, finish)
	}
	events := []string{
		chunk(fmt.Sprintf(`{"role":"assistant","content":null,"tool_calls":[{"index":0,"id":%q,"type":"function","function":{"name":%q,"arguments":""}}]}`, LoopToolCallID, LoopToolName), "null"),
		chunk(fmt.Sprintf(`{"tool_calls":[{"index":0,"function":{"arguments":%q}}]}`, LoopToolInput), "null"),
		chunk(`{}`, `"tool_calls"`),
	}
	if includeUsage {
		events = append(events, fmt.Sprintf(`data: {"id":"chatcmpl_fake_loop","object":"chat.completion.chunk","created":0,"model":%q,"choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`+"\n\n",
			model, FixtureInputTokens, FixtureOutputTokens, FixtureInputTokens+FixtureOutputTokens))
	}
	return append(events, "data: [DONE]\n\n")
}

func anthropicEvents(model string) []string {
	event := func(name, data string) string { return "event: " + name + "\ndata: " + data + "\n\n" }
	return []string{
		event("message_start", fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_fake_1","type":"message","role":"assistant","model":%q,"content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":%d,"output_tokens":1}}}`, model, FixtureInputTokens)),
		event("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		event("ping", `{"type":"ping"}`),
		event("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, fixtureText)),
		event("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":%q}}`, fixtureSecondText)),
		event("content_block_stop", `{"type":"content_block_stop","index":0}`),
		event("message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":%d}}`, FixtureOutputTokens)),
		event("message_stop", `{"type":"message_stop"}`),
	}
}

func openAIEvents(model string, includeUsage bool) []string {
	chunk := func(delta, finish string) string {
		return fmt.Sprintf(`data: {"id":"chatcmpl_fake_1","object":"chat.completion.chunk","created":0,"model":%q,"choices":[{"index":0,"delta":%s,"finish_reason":%s}]}`+"\n\n", model, delta, finish)
	}
	events := []string{
		chunk(`{"role":"assistant","content":""}`, "null"),
		chunk(fmt.Sprintf(`{"content":%q}`, fixtureText), "null"),
		chunk(fmt.Sprintf(`{"content":%q}`, fixtureSecondText), "null"),
		chunk(`{}`, `"stop"`),
	}
	if includeUsage {
		events = append(events, fmt.Sprintf(`data: {"id":"chatcmpl_fake_1","object":"chat.completion.chunk","created":0,"model":%q,"choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d}}`+"\n\n",
			model, FixtureInputTokens, FixtureOutputTokens, FixtureInputTokens+FixtureOutputTokens))
	}
	return append(events, "data: [DONE]\n\n")
}
