package translation

import (
	"encoding/json"
)

// parseChatRequest reads a Chat Completions body. The API documents most
// members as nullable, so null is the same as absent.
func parseChatRequest(root object) *Request {
	r := &Request{Source: OpenAIChat}
	for _, key := range root.keys() {
		value, ok := root.present(key, true)
		if !ok {
			continue
		}
		switch key {
		case "model":
			r.Model, _ = asString(value)
		case "messages":
			parseChatMessages(r, value)
		case "max_tokens", "max_completion_tokens":
			r.MaxOutputTokens, _ = asPositive(value)
		case "temperature":
			r.Temperature, _ = asNumber(value)
		case "top_p":
			r.TopP, _ = asNumber(value)
		case "stop":
			if text, isText := asString(value); isText {
				r.StopSequences = []string{text}
				continue
			}
			sequences, ok := asStrings(value)
			if !ok {
				r.block(FeatureUnsupportedBlock)
			}
			r.StopSequences = sequences
		case "stream":
			r.Stream, _ = asBool(value)
		case "stream_options":
			if options, ok := decodeObject(value); ok {
				if flag, has := options.present("include_usage", true); has {
					r.IncludeUsage, _ = asBool(flag)
				}
			}
		case "tools":
			parseChatTools(r, value)
		case "tool_choice":
			parseChatToolChoice(r, value)
		case "parallel_tool_calls":
			parallel, _ := asBool(value)
			r.ParallelToolCalls = &parallel
		case "user":
			r.User, _ = asString(value)
		case "metadata":
			// Chat metadata only labels stored completions; it never reaches the
			// model, so dropping it changes no answer.
		case "store":
			if stored, _ := asBool(value); stored {
				r.block(FeatureStatefulResponse)
			}
		case "n":
			if string(value) != "1" {
				r.block(FeatureUnknownField)
			}
		case "response_format":
			if format, ok := decodeObject(value); !ok || !isTextFormat(format) {
				r.block(FeatureStructuredOutput)
			}
		case "reasoning_effort":
			r.block(FeatureReasoning)
		case "modalities":
			if modalities, ok := asStrings(value); !ok || len(modalities) != 1 || modalities[0] != "text" {
				r.block(FeatureAudio)
			}
		case "audio":
			r.block(FeatureAudio)
		case "functions", "function_call", "web_search_options":
			r.block(FeatureToolType)
		default:
			r.block(FeatureUnknownField)
		}
	}
	return r
}

func isTextFormat(format object) bool {
	kind, _ := format.str("type")
	return kind == "text" && len(format) == 1
}

// chatParts reads a string or an array of content parts.
func chatParts(r *Request, raw json.RawMessage, allowImages bool) ([]Block, bool) {
	if text, isText := asString(raw); isText {
		return []Block{{Kind: BlockText, Text: text}}, true
	}
	parts, ok := decodeArray(raw)
	if !ok {
		return nil, false
	}
	blocks := make([]Block, 0, len(parts))
	for _, item := range parts {
		part, ok := decodeObject(item)
		if !ok {
			r.block(FeatureUnsupportedBlock)
			continue
		}
		kind, _ := part.str("type")
		switch kind {
		case "text":
			if !onlyMembers(part, "type", "text") {
				r.block(FeatureUnknownField)
			}
			text, _ := part.str("text")
			blocks = append(blocks, Block{Kind: BlockText, Text: text})
		case "image_url":
			if !allowImages {
				r.block(FeatureUnsupportedBlock)
				continue
			}
			image, feature := parseChatImage(part["image_url"])
			if feature != "" {
				r.block(feature)
				continue
			}
			blocks = append(blocks, Block{Kind: BlockImage, Image: image})
		case "input_audio":
			r.block(FeatureAudio)
		case "file":
			r.block(FeatureFile)
		default:
			r.block(FeatureUnsupportedBlock)
		}
	}
	return blocks, true
}

// parseChatImage reads an image_url object. detail is a quality hint without
// an equivalent elsewhere and is not carried over.
func parseChatImage(raw json.RawMessage) (Image, Feature) {
	value, ok := decodeObject(raw)
	if !ok || !onlyMembers(value, "url", "detail") {
		return Image{}, FeatureImageSource
	}
	url, _ := value.str("url")
	if url == "" {
		return Image{}, FeatureImageSource
	}
	image, ok := splitDataURL(url)
	if !ok {
		return Image{}, FeatureImageSource
	}
	return image, ""
}

func parseChatMessages(r *Request, value json.RawMessage) {
	messages, ok := decodeArray(value)
	if !ok {
		r.block(FeatureUnsupportedBlock)
		return
	}
	for _, raw := range messages {
		message, ok := decodeObject(raw)
		if !ok {
			r.block(FeatureUnsupportedBlock)
			continue
		}
		role, _ := message.str("role")
		switch role {
		case "system", "developer":
			parseChatInstruction(r, message)
		case "user":
			if !onlyMembers(message, "role", "content") {
				r.block(FeatureUnknownField)
			}
			blocks, ok := chatParts(r, message["content"], true)
			if !ok {
				r.block(FeatureUnsupportedBlock)
				continue
			}
			r.Turns = appendTurn(r.Turns, RoleUser, blocks, false)
		case "assistant":
			parseChatAssistant(r, message)
		case "tool":
			parseChatTool(r, message)
		case "function":
			r.block(FeatureToolType)
		default:
			r.block(FeatureUnsupportedRole)
		}
	}
}

// parseChatInstruction accepts system and developer messages only before the
// conversation starts: other protocols have no instruction in the middle of a
// conversation.
func parseChatInstruction(r *Request, message object) {
	if len(r.Turns) > 0 {
		r.block(FeatureUnsupportedRole)
		return
	}
	if !onlyMembers(message, "role", "content") {
		r.block(FeatureUnknownField)
	}
	blocks, ok := chatParts(r, message["content"], false)
	if !ok {
		r.block(FeatureUnsupportedBlock)
		return
	}
	for _, block := range blocks {
		r.Instructions = append(r.Instructions, block.Text)
	}
}

func parseChatAssistant(r *Request, message object) {
	var blocks []Block
	for _, key := range message.keys() {
		value, ok := message.present(key, true)
		if !ok {
			continue
		}
		switch key {
		case "role":
		case "content":
			parts, ok := chatParts(r, value, false)
			if !ok {
				r.block(FeatureUnsupportedBlock)
			}
			for _, part := range parts {
				if part.Text != "" {
					blocks = append(blocks, part)
				}
			}
		case "tool_calls":
			calls, ok := decodeArray(value)
			if !ok {
				r.block(FeatureUnsupportedBlock)
				continue
			}
			for _, raw := range calls {
				if call, ok := parseChatToolCall(r, raw); ok {
					blocks = append(blocks, call)
				}
			}
		case "refusal":
			r.block(FeatureUnsupportedBlock)
		case "function_call":
			r.block(FeatureToolType)
		case "audio":
			r.block(FeatureAudio)
		default:
			r.block(FeatureUnknownField)
		}
	}
	r.Turns = appendTurn(r.Turns, RoleAssistant, blocks, false)
}

func parseChatToolCall(r *Request, raw json.RawMessage) (Block, bool) {
	call, ok := decodeObject(raw)
	if !ok || !onlyMembers(call, "id", "type", "function") {
		r.block(FeatureUnsupportedBlock)
		return Block{}, false
	}
	if kind, _ := call.str("type"); kind != "function" {
		r.block(FeatureToolType)
		return Block{}, false
	}
	id, _ := call.str("id")
	function, ok := decodeObject(call["function"])
	name, _ := function.str("name")
	arguments, _ := function.str("arguments")
	compact, valid := compactObject(arguments)
	if !ok || id == "" || name == "" || !valid {
		r.block(FeatureUnsupportedBlock)
		return Block{}, false
	}
	return Block{Kind: BlockToolCall, ToolCallID: id, ToolName: name, Arguments: compact}, true
}

func parseChatTool(r *Request, message object) {
	if !onlyMembers(message, "role", "content", "tool_call_id") {
		r.block(FeatureUnknownField)
	}
	id, _ := message.str("tool_call_id")
	parts, ok := chatParts(r, message["content"], false)
	if id == "" || !ok {
		r.block(FeatureToolResult)
		return
	}
	join := len(r.Turns) > 0 && onlyToolResults(r.Turns[len(r.Turns)-1])
	r.Turns = appendTurn(r.Turns, RoleUser, []Block{{Kind: BlockToolResult, ToolCallID: id, Result: parts}}, join)
}

func parseChatTools(r *Request, value json.RawMessage) {
	tools, ok := decodeArray(value)
	if !ok {
		r.block(FeatureToolType)
		return
	}
	for _, raw := range tools {
		tool, ok := decodeObject(raw)
		if kind, _ := tool.str("type"); !ok || kind != "function" {
			r.block(FeatureToolType)
			continue
		}
		if !onlyMembers(tool, "type", "function") {
			r.block(FeatureUnknownField)
		}
		function, ok := decodeObject(tool["function"])
		if !ok {
			r.block(FeatureToolType)
			continue
		}
		parsed, ok := parseFunction(r, function)
		if ok {
			r.Tools = append(r.Tools, parsed)
		}
	}
}

// parseFunction reads a function definition shared by Chat and Responses.
func parseFunction(r *Request, function object) (Tool, bool) {
	tool := Tool{}
	for _, key := range function.keys() {
		value, ok := function.present(key, true)
		if !ok {
			continue
		}
		switch key {
		case "type":
		case "name":
			tool.Name, _ = asString(value)
		case "description":
			tool.Description, _ = asString(value)
		case "parameters":
			if !isObject(value) {
				r.block(FeatureToolType)
				return Tool{}, false
			}
			tool.Schema = compactRaw(value)
		case "strict":
			strict, _ := asBool(value)
			tool.Strict = &strict
		default:
			r.block(FeatureUnknownField)
		}
	}
	if tool.Name == "" {
		r.block(FeatureToolType)
		return Tool{}, false
	}
	if tool.Schema == nil {
		tool.Schema = json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return tool, true
}

func parseChatToolChoice(r *Request, value json.RawMessage) {
	if mode, isText := asString(value); isText {
		switch mode {
		case "auto":
			r.ToolChoice = ToolChoice{Mode: ChoiceAuto}
		case "none":
			r.ToolChoice = ToolChoice{Mode: ChoiceNone}
		case "required":
			r.ToolChoice = ToolChoice{Mode: ChoiceRequired}
		default:
			r.block(FeatureToolChoice)
		}
		return
	}
	choice, ok := decodeObject(value)
	if kind, _ := choice.str("type"); !ok || kind != "function" {
		r.block(FeatureToolChoice)
		return
	}
	function, ok := decodeObject(choice["function"])
	name, _ := function.str("name")
	if !ok || name == "" {
		r.block(FeatureToolChoice)
		return
	}
	r.ToolChoice = ToolChoice{Mode: ChoiceNamed, Name: name}
}

type chatImageURL struct {
	URL string `json:"url"`
}

type chatPart struct {
	Type     string        `json:"type"`
	Text     *string       `json:"text,omitempty"`
	ImageURL *chatImageURL `json:"image_url,omitempty"`
}

type chatFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function chatFunctionCall `json:"function"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type chatFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      *bool           `json:"strict,omitempty"`
}

type chatTool struct {
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatRequest struct {
	Model         string          `json:"model"`
	Messages      []chatMessage   `json:"messages"`
	MaxTokens     int64           `json:"max_tokens,omitempty"`
	Temperature   json.RawMessage `json:"temperature,omitempty"`
	TopP          json.RawMessage `json:"top_p,omitempty"`
	Stop          []string        `json:"stop,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
	Tools             []chatTool `json:"tools,omitempty"`
	ToolChoice        any        `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool      `json:"parallel_tool_calls,omitempty"`
	User              string     `json:"user,omitempty"`
}

// chatContent writes text alone as a string, the form every compatible server
// accepts, and anything else as parts.
func chatContent(blocks []Block) any {
	if len(blocks) == 1 && blocks[0].Kind == BlockText {
		return blocks[0].Text
	}
	parts := make([]chatPart, 0, len(blocks))
	for _, block := range blocks {
		switch block.Kind {
		case BlockText:
			text := block.Text
			parts = append(parts, chatPart{Type: "text", Text: &text})
		case BlockImage:
			parts = append(parts, chatPart{Type: "image_url", ImageURL: &chatImageURL{URL: block.Image.dataURL()}})
		}
	}
	return parts
}

func textOf(blocks []Block) string {
	text := ""
	for _, block := range blocks {
		if block.Kind == BlockText {
			text += block.Text
		}
	}
	return text
}

func renderChatRequest(r *Request, model string) ([]byte, error) {
	out := chatRequest{Model: model, MaxTokens: r.MaxOutputTokens, Temperature: number(r.Temperature), TopP: number(r.TopP),
		Stop: r.StopSequences, Stream: r.Stream, ParallelToolCalls: r.ParallelToolCalls, User: r.User}
	if r.Stream {
		// The gateway settles from the final usage chunk whatever the client
		// asked; the client's own choice is applied when the answer is rendered.
		out.StreamOptions = &struct {
			IncludeUsage bool `json:"include_usage"`
		}{true}
	}
	for _, text := range r.Instructions {
		out.Messages = append(out.Messages, chatMessage{Role: "system", Content: text})
	}
	for _, turn := range r.Turns {
		out.Messages = append(out.Messages, chatMessages(turn)...)
	}
	for _, tool := range r.Tools {
		out.Tools = append(out.Tools, chatTool{Type: "function", Function: chatFunction{Name: tool.Name, Description: tool.Description, Parameters: tool.Schema, Strict: tool.Strict}})
	}
	switch r.ToolChoice.Mode {
	case ChoiceAuto:
		out.ToolChoice = "auto"
	case ChoiceNone:
		out.ToolChoice = "none"
	case ChoiceRequired:
		out.ToolChoice = "required"
	case ChoiceNamed:
		out.ToolChoice = map[string]any{"type": "function", "function": map[string]string{"name": r.ToolChoice.Name}}
	}
	if len(r.Tools) == 0 {
		out.ParallelToolCalls = nil
	}
	return marshal(out)
}

// chatMessages splits one turn into Chat messages: tool results become tool
// messages in their original order, and other user content follows them in a
// user message.
func chatMessages(turn Turn) []chatMessage {
	if turn.Role == RoleAssistant {
		message := chatMessage{Role: "assistant"}
		var text []Block
		for _, block := range turn.Blocks {
			switch block.Kind {
			case BlockText:
				text = append(text, block)
			case BlockToolCall:
				message.ToolCalls = append(message.ToolCalls, chatToolCall{ID: ToolID(block.ToolCallID), Type: "function",
					Function: chatFunctionCall{Name: block.ToolName, Arguments: string(block.Arguments)}})
			}
		}
		if len(text) > 0 {
			message.Content = textOf(text)
		} else if len(message.ToolCalls) == 0 {
			message.Content = ""
		}
		return []chatMessage{message}
	}
	var out []chatMessage
	var pending []Block
	flush := func() {
		if len(pending) > 0 {
			out = append(out, chatMessage{Role: "user", Content: chatContent(pending)})
			pending = nil
		}
	}
	for _, block := range turn.Blocks {
		if block.Kind != BlockToolResult {
			pending = append(pending, block)
			continue
		}
		flush()
		out = append(out, chatMessage{Role: "tool", ToolCallID: ToolID(block.ToolCallID), Content: textOf(block.Result)})
	}
	flush()
	if len(out) == 0 {
		out = append(out, chatMessage{Role: "user", Content: ""})
	}
	return out
}
