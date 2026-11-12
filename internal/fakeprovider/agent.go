package fakeprovider

import (
	"encoding/json"
	"errors"
)

// AgentLoop is the client side of the agent-loop scenario: an agent that runs
// the tool the fake asks for and sends the result back, turn after turn. The
// context really grows every turn, while the last user message and the last
// tool result stay the same once the loop settles.
type AgentLoop struct {
	anthropic bool
	model     string
	user      string
	messages  []any
}

// NewAgentLoop starts a conversation with one task message.
func NewAgentLoop(anthropic bool, model, user, task string) *AgentLoop {
	return &AgentLoop{anthropic: anthropic, model: model, user: user, messages: []any{map[string]any{"role": "user", "content": task}}}
}

// Body is the next request of the agent.
func (a *AgentLoop) Body(stream bool) []byte {
	body := map[string]any{"model": a.model, "messages": a.messages, "stream": stream}
	if a.anthropic {
		body["max_tokens"] = 64
		body["metadata"] = map[string]any{"user_id": a.user}
	} else {
		body["user"] = a.user
	}
	data, _ := json.Marshal(body)
	return data
}

// Turns is how many tool results the agent sent back so far.
func (a *AgentLoop) Turns() int { return (len(a.messages) - 1) / 2 }

var errNoToolCall = errors.New("response has no tool call")

// Advance reads the tool call from a non-streaming response and appends it
// with the given tool result, as an agent runtime would.
func (a *AgentLoop) Advance(response []byte, result string) error {
	if a.anthropic {
		var parsed struct {
			Content []struct {
				Type  string          `json:"type"`
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		}
		if err := json.Unmarshal(response, &parsed); err != nil {
			return err
		}
		for _, block := range parsed.Content {
			if block.Type == "tool_use" {
				a.messages = append(a.messages,
					map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": block.ID, "name": block.Name, "input": block.Input}}},
					map[string]any{"role": "user", "content": []any{map[string]any{"type": "tool_result", "tool_use_id": block.ID, "content": result}}})
				return nil
			}
		}
		return errNoToolCall
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				ToolCalls []struct {
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(response, &parsed); err != nil {
		return err
	}
	if len(parsed.Choices) == 0 || len(parsed.Choices[0].Message.ToolCalls) == 0 {
		return errNoToolCall
	}
	call := parsed.Choices[0].Message.ToolCalls[0]
	a.messages = append(a.messages,
		map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": call.ID, "type": call.Type, "function": map[string]any{"name": call.Function.Name, "arguments": call.Function.Arguments}}}},
		map[string]any{"role": "tool", "tool_call_id": call.ID, "content": result})
	return nil
}
