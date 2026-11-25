package translation

import (
	"errors"
	"time"
)

// ParseResponse reads a complete answer in the provider's protocol. Content
// without an equivalent, such as reasoning, citations or server tools, fails
// instead of being turned into text.
func ParseResponse(source WireFormat, body []byte) (*Response, error) {
	root, ok := decodeObject(body)
	if !ok {
		return nil, badResponse("not an object")
	}
	switch source {
	case AnthropicMessages:
		return parseAnthropicResponse(root)
	case OpenAIChat:
		return parseChatResponse(root)
	}
	return nil, errors.New("responses are not parsed from this format")
}

func parseAnthropicResponse(root object) (*Response, error) {
	out := &Response{}
	out.Model, _ = root.str("model")
	if kind, _ := root.str("type"); kind != "message" {
		return nil, badResponse("not a message")
	}
	items, ok := decodeArray(root["content"])
	if !ok {
		return nil, badResponse("content")
	}
	for _, raw := range items {
		block, ok := decodeObject(raw)
		if !ok {
			return nil, badResponse("content block")
		}
		kind, _ := block.str("type")
		switch kind {
		case "text":
			if citations, has := block.present("citations", true); has {
				if list, ok := decodeArray(citations); !ok || len(list) > 0 {
					return nil, badResponse("citations")
				}
			}
			text, _ := block.str("text")
			out.Blocks = append(out.Blocks, Block{Kind: BlockText, Text: text})
		case "tool_use":
			id, _ := block.str("id")
			name, _ := block.str("name")
			input := block["input"]
			if id == "" || name == "" || !isObject(input) {
				return nil, badResponse("tool_use")
			}
			arguments, _ := compactObject(string(input))
			out.Blocks = append(out.Blocks, Block{Kind: BlockToolCall, ToolCallID: id, ToolName: name, Arguments: arguments})
		default:
			return nil, badResponse("content type")
		}
	}
	reason, _ := root.str("stop_reason")
	sequence, _ := root.str("stop_sequence")
	stop, err := anthropicStop(reason)
	if err != nil {
		return nil, err
	}
	out.Stop, out.StopSequence = stop, sequence
	return out, nil
}

func anthropicStop(reason string) (StopReason, error) {
	switch reason {
	case "end_turn":
		return StopEnd, nil
	case "max_tokens":
		return StopMaxTokens, nil
	case "tool_use":
		return StopToolUse, nil
	case "stop_sequence":
		return StopSequence, nil
	case "refusal":
		return StopContentFilter, nil
	}
	return StopUnknown, badResponse("stop reason")
}

func chatStop(reason string) (StopReason, error) {
	switch reason {
	case "stop":
		return StopEnd, nil
	case "length":
		return StopMaxTokens, nil
	case "tool_calls":
		return StopToolUse, nil
	case "content_filter":
		return StopContentFilter, nil
	}
	return StopUnknown, badResponse("finish reason")
}

func parseChatResponse(root object) (*Response, error) {
	out := &Response{}
	out.Model, _ = root.str("model")
	choices, ok := decodeArray(root["choices"])
	if !ok || len(choices) != 1 {
		return nil, badResponse("choices")
	}
	choice, ok := decodeObject(choices[0])
	if !ok {
		return nil, badResponse("choice")
	}
	message, ok := decodeObject(choice["message"])
	if !ok {
		return nil, badResponse("message")
	}
	if _, refused := message.present("refusal", true); refused {
		return nil, badResponse("refusal")
	}
	if _, legacy := message.present("function_call", true); legacy {
		return nil, badResponse("function_call")
	}
	if _, audio := message.present("audio", true); audio {
		return nil, badResponse("audio")
	}
	if annotations, has := message.present("annotations", true); has {
		if list, ok := decodeArray(annotations); !ok || len(list) > 0 {
			return nil, badResponse("annotations")
		}
	}
	if content, has := message.present("content", true); has {
		text, isText := asString(content)
		if !isText {
			return nil, badResponse("content")
		}
		if text != "" {
			out.Blocks = append(out.Blocks, Block{Kind: BlockText, Text: text})
		}
	}
	if raw, has := message.present("tool_calls", true); has {
		calls, ok := decodeArray(raw)
		if !ok {
			return nil, badResponse("tool_calls")
		}
		for _, item := range calls {
			call, ok := decodeObject(item)
			if kind, _ := call.str("type"); !ok || kind != "function" {
				return nil, badResponse("tool call type")
			}
			id, _ := call.str("id")
			function, _ := decodeObject(call["function"])
			name, _ := function.str("name")
			arguments, _ := function.str("arguments")
			compact, valid := compactObject(arguments)
			if id == "" || name == "" || !valid {
				return nil, badResponse("tool call")
			}
			out.Blocks = append(out.Blocks, Block{Kind: BlockToolCall, ToolCallID: id, ToolName: name, Arguments: compact})
		}
	}
	reason, _ := choice.str("finish_reason")
	stop, err := chatStop(reason)
	if err != nil {
		return nil, err
	}
	out.Stop = stop
	return out, nil
}

// RenderOptions describe the answer being rendered.
type RenderOptions struct {
	// Model is the fallback when the provider's answer named none.
	Model string
	Now   time.Time
	Usage Usage
}

// RenderResponse writes a complete answer in the client's protocol.
func RenderResponse(r *Response, client WireFormat, options RenderOptions) ([]byte, error) {
	if r.Model == "" {
		r.Model = options.Model
	}
	switch client {
	case AnthropicMessages:
		return renderAnthropicResponse(r, options.Usage)
	case OpenAIChat:
		return renderChatResponse(r, options)
	case OpenAIResponses:
		return renderResponsesResponse(r, options)
	}
	return nil, errors.New("unknown client format")
}

func anthropicStopName(stop StopReason) (string, error) {
	switch stop {
	case StopEnd:
		return "end_turn", nil
	case StopMaxTokens:
		return "max_tokens", nil
	case StopToolUse:
		return "tool_use", nil
	case StopSequence:
		return "stop_sequence", nil
	case StopContentFilter:
		return "refusal", nil
	}
	return "", badResponse("stop reason")
}

func chatStopName(stop StopReason) (string, error) {
	switch stop {
	case StopEnd, StopSequence:
		return "stop", nil
	case StopMaxTokens:
		return "length", nil
	case StopToolUse:
		return "tool_calls", nil
	case StopContentFilter:
		return "content_filter", nil
	}
	return "", badResponse("stop reason")
}

type anthropicUsage struct {
	InputTokens              int64  `json:"input_tokens"`
	OutputTokens             int64  `json:"output_tokens"`
	CacheCreationInputTokens *int64 `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     *int64 `json:"cache_read_input_tokens,omitempty"`
}

func optionalCount(c Counter) *int64 {
	if !c.Known {
		return nil
	}
	value := c.Value
	return &value
}

// AnthropicUsage projects usage into the Messages counters. The SDK types
// require input and output, so an unreported one is written as zero; the
// settlement never reads this projection.
func anthropicUsageOf(u Usage) anthropicUsage {
	return anthropicUsage{InputTokens: u.Input.Value, OutputTokens: u.Output.Value,
		CacheCreationInputTokens: optionalCount(u.CacheWrite), CacheReadInputTokens: optionalCount(u.CacheRead)}
}

func renderAnthropicResponse(r *Response, usage Usage) ([]byte, error) {
	stop, err := anthropicStopName(r.Stop)
	if err != nil {
		return nil, err
	}
	content := make([]anthropicContent, 0, len(r.Blocks))
	for _, block := range r.Blocks {
		if rendered, ok := anthropicBlock(block); ok {
			content = append(content, rendered)
		}
	}
	var sequence *string
	if r.Stop == StopSequence && r.StopSequence != "" {
		sequence = &r.StopSequence
	}
	return marshal(struct {
		ID           string             `json:"id"`
		Type         string             `json:"type"`
		Role         string             `json:"role"`
		Model        string             `json:"model"`
		Content      []anthropicContent `json:"content"`
		StopReason   string             `json:"stop_reason"`
		StopSequence *string            `json:"stop_sequence"`
		Usage        anthropicUsage     `json:"usage"`
	}{newID("msg_"), "message", "assistant", r.Model, content, stop, sequence, anthropicUsageOf(usage)})
}

type chatUsage struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	TotalTokens         int64 `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
}

func chatUsageOf(u Usage) chatUsage {
	prompt := u.promptTotal()
	out := chatUsage{PromptTokens: prompt, CompletionTokens: u.Output.Value, TotalTokens: prompt + u.Output.Value}
	if u.CacheRead.Known {
		out.PromptTokensDetails = &struct {
			CachedTokens int64 `json:"cached_tokens"`
		}{u.CacheRead.Value}
	}
	return out
}

func renderChatResponse(r *Response, options RenderOptions) ([]byte, error) {
	finish, err := chatStopName(r.Stop)
	if err != nil {
		return nil, err
	}
	message := struct {
		Role      string         `json:"role"`
		Content   *string        `json:"content"`
		ToolCalls []chatToolCall `json:"tool_calls,omitempty"`
		Refusal   *string        `json:"refusal"`
	}{Role: "assistant"}
	text := ""
	hasText := false
	for _, block := range r.Blocks {
		switch block.Kind {
		case BlockText:
			text += block.Text
			hasText = true
		case BlockToolCall:
			message.ToolCalls = append(message.ToolCalls, chatToolCall{ID: ToolID(block.ToolCallID), Type: "function",
				Function: chatFunctionCall{Name: block.ToolName, Arguments: string(block.Arguments)}})
		}
	}
	if hasText || len(message.ToolCalls) == 0 {
		message.Content = &text
	}
	type choice struct {
		Index        int    `json:"index"`
		Message      any    `json:"message"`
		FinishReason string `json:"finish_reason"`
		Logprobs     any    `json:"logprobs"`
	}
	return marshal(struct {
		ID      string    `json:"id"`
		Object  string    `json:"object"`
		Created int64     `json:"created"`
		Model   string    `json:"model"`
		Choices []choice  `json:"choices"`
		Usage   chatUsage `json:"usage"`
	}{newID("chatcmpl-"), "chat.completion", options.Now.Unix(), r.Model, []choice{{0, message, finish, nil}}, chatUsageOf(options.Usage)})
}

type responsesUsage struct {
	InputTokens        int64 `json:"input_tokens"`
	InputTokensDetails struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
	OutputTokens        int64 `json:"output_tokens"`
	OutputTokensDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
	TotalTokens int64 `json:"total_tokens"`
}

func responsesUsageOf(u Usage) responsesUsage {
	out := responsesUsage{InputTokens: u.promptTotal(), OutputTokens: u.Output.Value}
	out.InputTokensDetails.CachedTokens = u.CacheRead.Value
	out.TotalTokens = out.InputTokens + out.OutputTokens
	return out
}

type responsesContent struct {
	Type        string `json:"type"`
	Text        string `json:"text"`
	Annotations []any  `json:"annotations"`
}

type responsesItem struct {
	ID        string             `json:"id"`
	Type      string             `json:"type"`
	Status    string             `json:"status"`
	Role      string             `json:"role,omitempty"`
	Content   []responsesContent `json:"content,omitempty"`
	CallID    string             `json:"call_id,omitempty"`
	Name      string             `json:"name,omitempty"`
	Arguments *string            `json:"arguments,omitempty"`
}

// MarshalJSON always writes the content list of a message item, even empty:
// the SDK stream accumulators append parts to it.
func (i responsesItem) MarshalJSON() ([]byte, error) {
	type plain responsesItem
	if i.Type != "message" {
		return marshal(plain(i))
	}
	content := i.Content
	if content == nil {
		content = []responsesContent{}
	}
	return marshal(struct {
		plain
		Content []responsesContent `json:"content"`
	}{plain(i), content})
}

// responsesObject is the Response object of the API with the members the SDK
// types read. Fields the gateway does not track carry their documented
// defaults.
type responsesObject struct {
	ID                string          `json:"id"`
	Object            string          `json:"object"`
	CreatedAt         int64           `json:"created_at"`
	Status            string          `json:"status"`
	Error             any             `json:"error"`
	IncompleteDetails any             `json:"incomplete_details"`
	Instructions      any             `json:"instructions"`
	Metadata          map[string]any  `json:"metadata"`
	Model             string          `json:"model"`
	Output            []responsesItem `json:"output"`
	ParallelToolCalls bool            `json:"parallel_tool_calls"`
	Temperature       any             `json:"temperature"`
	ToolChoice        string          `json:"tool_choice"`
	Tools             []any           `json:"tools"`
	TopP              any             `json:"top_p"`
	Usage             *responsesUsage `json:"usage"`
}

func newResponsesObject(id, model string, created time.Time) responsesObject {
	return responsesObject{ID: id, Object: "response", CreatedAt: created.Unix(), Status: "in_progress", Metadata: map[string]any{},
		Model: model, Output: []responsesItem{}, ParallelToolCalls: true, ToolChoice: "auto", Tools: []any{}}
}

// finishResponses sets the final status of a Responses object from the stop.
func finishResponses(object *responsesObject, stop StopReason, usage Usage) error {
	switch stop {
	case StopEnd, StopSequence, StopToolUse:
		object.Status = "completed"
	case StopMaxTokens:
		object.Status, object.IncompleteDetails = "incomplete", map[string]string{"reason": "max_output_tokens"}
	case StopContentFilter:
		object.Status, object.IncompleteDetails = "incomplete", map[string]string{"reason": "content_filter"}
	default:
		return badResponse("stop reason")
	}
	projected := responsesUsageOf(usage)
	object.Usage = &projected
	return nil
}

func renderResponsesResponse(r *Response, options RenderOptions) ([]byte, error) {
	object := newResponsesObject(newID("resp_"), r.Model, options.Now)
	var text []responsesContent
	for _, block := range r.Blocks {
		switch block.Kind {
		case BlockText:
			text = append(text, responsesContent{Type: "output_text", Text: block.Text, Annotations: []any{}})
		case BlockToolCall:
			if len(text) > 0 {
				object.Output = append(object.Output, responsesItem{ID: newID("msg_"), Type: "message", Status: "completed", Role: "assistant", Content: text})
				text = nil
			}
			arguments := string(block.Arguments)
			object.Output = append(object.Output, responsesItem{ID: newID("fc_"), Type: "function_call", Status: "completed",
				CallID: ToolID(block.ToolCallID), Name: block.ToolName, Arguments: &arguments})
		}
	}
	if len(text) > 0 {
		object.Output = append(object.Output, responsesItem{ID: newID("msg_"), Type: "message", Status: "completed", Role: "assistant", Content: text})
	}
	if err := finishResponses(&object, r.Stop, options.Usage); err != nil {
		return nil, err
	}
	return marshal(object)
}
