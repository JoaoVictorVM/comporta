package translation

import (
	"encoding/json"
)

// parseResponsesRequest reads the conversational subset of the Responses API.
// Structure errors are malformed requests. Valid members outside the subset,
// such as server-side state or hosted tools, block every target, since this
// protocol is never forwarded as it is.
func parseResponsesRequest(root object) (*Request, error) {
	r := &Request{Source: OpenAIResponses}
	if _, ok := root["input"]; !ok {
		return nil, malformed("input is required")
	}
	for _, key := range root.keys() {
		value, ok := root.present(key, true)
		if !ok {
			continue
		}
		var err error
		switch key {
		case "model":
			r.Model, _ = asString(value)
		case "input":
			err = parseResponsesInput(r, value)
		case "instructions":
			text, isText := asString(value)
			if !isText {
				return nil, malformed("instructions must be a string")
			}
			if text != "" {
				r.Instructions = append([]string{text}, r.Instructions...)
			}
		case "max_output_tokens":
			r.MaxOutputTokens, _ = asPositive(value)
		case "temperature":
			r.Temperature, _ = asNumber(value)
		case "top_p":
			r.TopP, _ = asNumber(value)
		case "stream":
			r.Stream, _ = asBool(value)
		case "stream_options":
			if !isObject(value) {
				return nil, malformed("stream_options must be an object")
			}
		case "tools":
			err = parseResponsesTools(r, value)
		case "tool_choice":
			err = parseResponsesToolChoice(r, value)
		case "parallel_tool_calls":
			parallel, valid := asBool(value)
			if !valid {
				return nil, malformed("parallel_tool_calls must be a boolean")
			}
			r.ParallelToolCalls = &parallel
		case "user":
			r.User, _ = asString(value)
		case "metadata":
			if !isObject(value) {
				return nil, malformed("metadata must be an object")
			}
		case "store":
			stored, valid := asBool(value)
			if !valid {
				return nil, malformed("store must be a boolean")
			}
			if stored {
				r.block(FeatureStatefulResponse)
			}
		case "background":
			if flag, _ := asBool(value); flag {
				r.block(FeatureStatefulResponse)
			}
		case "previous_response_id", "conversation", "prompt":
			r.block(FeatureStatefulResponse)
		case "include":
			if list, ok := decodeArray(value); !ok || len(list) > 0 {
				r.block(FeatureStatefulResponse)
			}
		case "reasoning":
			r.block(FeatureReasoning)
		case "text":
			if !isPlainText(value) {
				r.block(FeatureStructuredOutput)
			}
		case "truncation":
			if mode, _ := asString(value); mode != "disabled" {
				r.block(FeatureUnknownField)
			}
		default:
			r.block(FeatureUnknownField)
		}
		if err != nil {
			return nil, err
		}
	}
	return r, nil
}

// isPlainText accepts the default text configuration, which only asks for
// plain text output.
func isPlainText(value json.RawMessage) bool {
	config, ok := decodeObject(value)
	if !ok {
		return false
	}
	for key, member := range config {
		if key != "format" {
			return false
		}
		format, ok := decodeObject(member)
		if !ok || !isTextFormat(format) {
			return false
		}
	}
	return true
}

func parseResponsesInput(r *Request, value json.RawMessage) error {
	if text, isText := asString(value); isText {
		r.Turns = append(r.Turns, Turn{Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: text}}})
		return nil
	}
	items, ok := decodeArray(value)
	if !ok {
		return malformed("input must be a string or an array")
	}
	for _, raw := range items {
		item, ok := decodeObject(raw)
		if !ok {
			return malformed("input items must be objects")
		}
		kind, hasType := item.str("type")
		if !hasType {
			if _, hasRole := item["role"]; hasRole {
				kind = "message"
			}
		}
		var err error
		switch kind {
		case "message":
			err = parseResponsesMessage(r, item)
		case "function_call":
			err = parseResponsesCall(r, item)
		case "function_call_output":
			err = parseResponsesOutput(r, item)
		case "reasoning":
			r.block(FeatureReasoning)
		case "item_reference":
			r.block(FeatureStatefulResponse)
		case "web_search_call", "file_search_call", "computer_call", "computer_call_output", "code_interpreter_call",
			"image_generation_call", "local_shell_call", "local_shell_call_output", "mcp_call", "mcp_list_tools",
			"mcp_approval_request", "mcp_approval_response", "custom_tool_call", "custom_tool_call_output":
			r.block(FeatureToolType)
		case "":
			return malformed("input items need a type")
		default:
			r.block(FeatureUnsupportedBlock)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func parseResponsesMessage(r *Request, item object) error {
	role, _ := item.str("role")
	for key := range item {
		switch key {
		case "type", "role", "content", "id", "status":
		default:
			r.block(FeatureUnknownField)
		}
	}
	content, ok := item["content"]
	if !ok {
		return malformed("message items need content")
	}
	blocks, err := responsesParts(r, content)
	if err != nil {
		return err
	}
	switch role {
	case "system", "developer":
		if len(r.Turns) > 0 {
			r.block(FeatureUnsupportedRole)
			return nil
		}
		for _, block := range blocks {
			if block.Kind != BlockText {
				r.block(FeatureUnsupportedBlock)
				continue
			}
			r.Instructions = append(r.Instructions, block.Text)
		}
	case "user":
		r.Turns = appendTurn(r.Turns, RoleUser, blocks, false)
	case "assistant":
		for _, block := range blocks {
			if block.Kind != BlockText {
				r.block(FeatureUnsupportedBlock)
			}
		}
		r.Turns = appendTurn(r.Turns, RoleAssistant, blocks, false)
	default:
		return malformed("message role must be user, assistant, system or developer")
	}
	return nil
}

// responsesParts reads a string or an array of input and output parts.
func responsesParts(r *Request, raw json.RawMessage) ([]Block, error) {
	if text, isText := asString(raw); isText {
		return []Block{{Kind: BlockText, Text: text}}, nil
	}
	parts, ok := decodeArray(raw)
	if !ok {
		return nil, malformed("content must be a string or an array")
	}
	blocks := make([]Block, 0, len(parts))
	for _, item := range parts {
		part, ok := decodeObject(item)
		if !ok {
			return nil, malformed("content parts must be objects")
		}
		kind, _ := part.str("type")
		switch kind {
		case "input_text", "output_text":
			text, isText := part.str("text")
			if !isText {
				return nil, malformed("text parts need text")
			}
			for key, value := range part {
				switch key {
				case "type", "text":
				case "annotations", "logprobs":
					if list, ok := decodeArray(value); !ok || len(list) > 0 {
						r.block(FeatureUnsupportedBlock)
					}
				default:
					r.block(FeatureUnknownField)
				}
			}
			blocks = append(blocks, Block{Kind: BlockText, Text: text})
		case "input_image":
			if _, hasFile := part.present("file_id", true); hasFile {
				r.block(FeatureFile)
				continue
			}
			url, _ := part.str("image_url")
			image, valid := splitDataURL(url)
			if url == "" || !valid {
				r.block(FeatureImageSource)
				continue
			}
			for key := range part {
				switch key {
				case "type", "image_url", "detail", "file_id":
				default:
					r.block(FeatureUnknownField)
				}
			}
			blocks = append(blocks, Block{Kind: BlockImage, Image: image})
		case "input_file":
			r.block(FeatureFile)
		case "input_audio":
			r.block(FeatureAudio)
		case "refusal":
			r.block(FeatureUnsupportedBlock)
		case "":
			return nil, malformed("content parts need a type")
		default:
			r.block(FeatureUnsupportedBlock)
		}
	}
	return blocks, nil
}

func parseResponsesCall(r *Request, item object) error {
	id, _ := item.str("call_id")
	name, _ := item.str("name")
	arguments, isText := item.str("arguments")
	if id == "" || name == "" || !isText {
		return malformed("function_call items need call_id, name and arguments")
	}
	for key := range item {
		switch key {
		case "type", "call_id", "name", "arguments", "id", "status":
		default:
			r.block(FeatureUnknownField)
		}
	}
	compact, valid := compactObject(arguments)
	if !valid {
		r.block(FeatureUnsupportedBlock)
		return nil
	}
	join := len(r.Turns) > 0 && r.Turns[len(r.Turns)-1].Role == RoleAssistant
	r.Turns = appendTurn(r.Turns, RoleAssistant, []Block{{Kind: BlockToolCall, ToolCallID: id, ToolName: name, Arguments: compact}}, join)
	return nil
}

func parseResponsesOutput(r *Request, item object) error {
	id, _ := item.str("call_id")
	output, ok := item["output"]
	if id == "" || !ok {
		return malformed("function_call_output items need call_id and output")
	}
	for key := range item {
		switch key {
		case "type", "call_id", "output", "id", "status":
		default:
			r.block(FeatureUnknownField)
		}
	}
	parts, err := responsesParts(r, output)
	if err != nil {
		return err
	}
	join := len(r.Turns) > 0 && onlyToolResults(r.Turns[len(r.Turns)-1])
	r.Turns = appendTurn(r.Turns, RoleUser, []Block{{Kind: BlockToolResult, ToolCallID: id, Result: parts}}, join)
	return nil
}

func parseResponsesTools(r *Request, value json.RawMessage) error {
	tools, ok := decodeArray(value)
	if !ok {
		return malformed("tools must be an array")
	}
	for _, raw := range tools {
		tool, ok := decodeObject(raw)
		if !ok {
			return malformed("tools must be objects")
		}
		kind, _ := tool.str("type")
		switch kind {
		case "function":
			if name, _ := tool.str("name"); name == "" {
				return malformed("function tools need a name")
			}
			if parsed, ok := parseFunction(r, tool); ok {
				r.Tools = append(r.Tools, parsed)
			}
		case "":
			return malformed("tools need a type")
		default:
			r.block(FeatureToolType)
		}
	}
	return nil
}

func parseResponsesToolChoice(r *Request, value json.RawMessage) error {
	if mode, isText := asString(value); isText {
		switch mode {
		case "auto":
			r.ToolChoice = ToolChoice{Mode: ChoiceAuto}
		case "none":
			r.ToolChoice = ToolChoice{Mode: ChoiceNone}
		case "required":
			r.ToolChoice = ToolChoice{Mode: ChoiceRequired}
		default:
			return malformed("tool_choice must be auto, none, required or an object")
		}
		return nil
	}
	choice, ok := decodeObject(value)
	if !ok {
		return malformed("tool_choice must be auto, none, required or an object")
	}
	kind, _ := choice.str("type")
	switch kind {
	case "function":
		name, _ := choice.str("name")
		if name == "" {
			return malformed("tool_choice needs the function name")
		}
		r.ToolChoice = ToolChoice{Mode: ChoiceNamed, Name: name}
	case "allowed_tools":
		r.block(FeatureToolChoice)
	default:
		r.block(FeatureToolType)
	}
	return nil
}
