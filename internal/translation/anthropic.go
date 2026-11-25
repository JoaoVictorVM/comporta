package translation

import (
	"encoding/json"
)

// parseAnthropicRequest reads a Messages body. Members without an explicit
// rule block translation; cache_control is a caching hint without meaning for
// the answer and is the one member dropped on purpose.
func parseAnthropicRequest(root object) *Request {
	r := &Request{Source: AnthropicMessages}
	for _, key := range root.keys() {
		value := root[key]
		switch key {
		case "model":
			r.Model, _ = asString(value)
		case "max_tokens":
			r.MaxOutputTokens, _ = asPositive(value)
		case "messages":
			parseAnthropicMessages(r, value)
		case "system":
			parseAnthropicSystem(r, value)
		case "temperature":
			r.Temperature, _ = asNumber(value)
		case "top_p":
			r.TopP, _ = asNumber(value)
		case "stop_sequences":
			sequences, ok := asStrings(value)
			if !ok {
				r.block(FeatureUnsupportedBlock)
			}
			r.StopSequences = sequences
		case "stream":
			r.Stream, _ = asBool(value)
		case "tools":
			parseAnthropicTools(r, value)
		case "tool_choice":
			parseAnthropicToolChoice(r, value)
		case "metadata":
			parseAnthropicMetadata(r, value)
		case "thinking":
			if config, ok := decodeObject(value); !ok || !isDisabled(config) {
				r.block(FeatureReasoning)
			}
		case "mcp_servers", "container":
			r.block(FeatureToolType)
		default:
			r.block(FeatureUnknownField)
		}
	}
	return r
}

func isDisabled(config object) bool {
	kind, _ := config.str("type")
	return kind == "disabled" && len(config) == 1
}

func parseAnthropicMetadata(r *Request, value json.RawMessage) {
	metadata, ok := decodeObject(value)
	if !ok {
		r.block(FeatureUnknownField)
		return
	}
	for key, member := range metadata {
		if key != "user_id" {
			r.block(FeatureUnknownField)
			continue
		}
		r.User, _ = asString(member)
	}
}

func parseAnthropicSystem(r *Request, value json.RawMessage) {
	if text, ok := asString(value); ok {
		if text != "" {
			r.Instructions = append(r.Instructions, text)
		}
		return
	}
	blocks, ok := decodeArray(value)
	if !ok {
		r.block(FeatureUnsupportedBlock)
		return
	}
	for _, raw := range blocks {
		block, ok := decodeObject(raw)
		kind, _ := block.str("type")
		if !ok || kind != "text" {
			r.block(FeatureUnsupportedBlock)
			continue
		}
		if !onlyMembers(block, "type", "text", "cache_control") {
			r.block(FeatureUnknownField)
		}
		text, _ := block.str("text")
		r.Instructions = append(r.Instructions, text)
	}
}

// onlyMembers reports whether every member of o is one of the allowed names. A
// null citations member is the same as an absent one.
func onlyMembers(o object, allowed ...string) bool {
	for key, value := range o {
		if key == "citations" && isNull(value) {
			continue
		}
		found := false
		for _, name := range allowed {
			if key == name {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func parseAnthropicMessages(r *Request, value json.RawMessage) {
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
		if !onlyMembers(message, "role", "content") {
			r.block(FeatureUnknownField)
		}
		roleName, _ := message.str("role")
		role := Role(roleName)
		if role != RoleUser && role != RoleAssistant {
			r.block(FeatureUnsupportedRole)
			continue
		}
		content := message["content"]
		if text, isText := asString(content); isText {
			r.Turns = append(r.Turns, Turn{Role: role, Blocks: []Block{{Kind: BlockText, Text: text}}})
			continue
		}
		items, ok := decodeArray(content)
		if !ok {
			r.block(FeatureUnsupportedBlock)
			continue
		}
		blocks := make([]Block, 0, len(items))
		for _, item := range items {
			if block, ok := parseAnthropicBlock(r, item); ok {
				blocks = append(blocks, block)
			}
		}
		r.Turns = append(r.Turns, Turn{Role: role, Blocks: blocks})
	}
}

func parseAnthropicBlock(r *Request, raw json.RawMessage) (Block, bool) {
	block, ok := decodeObject(raw)
	if !ok {
		r.block(FeatureUnsupportedBlock)
		return Block{}, false
	}
	kind, _ := block.str("type")
	switch kind {
	case "text":
		if !onlyMembers(block, "type", "text", "cache_control") {
			r.block(FeatureUnsupportedBlock)
		}
		text, _ := block.str("text")
		return Block{Kind: BlockText, Text: text}, true
	case "image":
		if !onlyMembers(block, "type", "source", "cache_control") {
			r.block(FeatureUnknownField)
		}
		image, feature := parseAnthropicSource(block["source"])
		if feature != "" {
			r.block(feature)
			return Block{}, false
		}
		return Block{Kind: BlockImage, Image: image}, true
	case "tool_use":
		if !onlyMembers(block, "type", "id", "name", "input", "cache_control") {
			r.block(FeatureUnknownField)
		}
		id, _ := block.str("id")
		name, _ := block.str("name")
		input := block["input"]
		if id == "" || name == "" || !isObject(input) {
			r.block(FeatureUnsupportedBlock)
			return Block{}, false
		}
		arguments, _ := compactObject(string(input))
		return Block{Kind: BlockToolCall, ToolCallID: id, ToolName: name, Arguments: arguments}, true
	case "tool_result":
		return parseAnthropicToolResult(r, block)
	case "document":
		r.block(FeatureFile)
	case "thinking", "redacted_thinking":
		r.block(FeatureReasoning)
	case "server_tool_use", "web_search_tool_result", "web_fetch_tool_result", "code_execution_tool_result",
		"mcp_tool_use", "mcp_tool_result", "container_upload", "bash_code_execution_tool_result", "text_editor_code_execution_tool_result":
		r.block(FeatureToolType)
	default:
		r.block(FeatureUnsupportedBlock)
	}
	return Block{}, false
}

func parseAnthropicSource(raw json.RawMessage) (Image, Feature) {
	source, ok := decodeObject(raw)
	if !ok {
		return Image{}, FeatureImageSource
	}
	kind, _ := source.str("type")
	switch kind {
	case "base64":
		mediaType, _ := source.str("media_type")
		data, _ := source.str("data")
		if mediaType == "" || data == "" || !onlyMembers(source, "type", "media_type", "data") {
			return Image{}, FeatureImageSource
		}
		return Image{MediaType: mediaType, Data: data}, ""
	case "url":
		url, _ := source.str("url")
		if url == "" || !onlyMembers(source, "type", "url") {
			return Image{}, FeatureImageSource
		}
		return Image{URL: url}, ""
	case "file":
		return Image{}, FeatureFile
	}
	return Image{}, FeatureImageSource
}

func parseAnthropicToolResult(r *Request, block object) (Block, bool) {
	if !onlyMembers(block, "type", "tool_use_id", "content", "is_error", "cache_control") {
		r.block(FeatureUnknownField)
	}
	id, _ := block.str("tool_use_id")
	if id == "" {
		r.block(FeatureToolResult)
		return Block{}, false
	}
	out := Block{Kind: BlockToolResult, ToolCallID: id}
	if flag, ok := block.present("is_error", false); ok {
		out.IsError, _ = asBool(flag)
	}
	content, ok := block.present("content", false)
	if !ok {
		return out, true
	}
	if text, isText := asString(content); isText {
		out.Result = []Block{{Kind: BlockText, Text: text}}
		return out, true
	}
	items, ok := decodeArray(content)
	if !ok {
		r.block(FeatureToolResult)
		return Block{}, false
	}
	for _, item := range items {
		part, ok := parseAnthropicBlock(r, item)
		if !ok {
			continue
		}
		if part.Kind != BlockText && part.Kind != BlockImage {
			r.block(FeatureToolResult)
			continue
		}
		out.Result = append(out.Result, part)
	}
	return out, true
}

func parseAnthropicTools(r *Request, value json.RawMessage) {
	tools, ok := decodeArray(value)
	if !ok {
		r.block(FeatureToolType)
		return
	}
	for _, raw := range tools {
		tool, ok := decodeObject(raw)
		if !ok {
			r.block(FeatureToolType)
			continue
		}
		if kind, hasType := tool.str("type"); hasType && kind != "custom" {
			r.block(FeatureToolType)
			continue
		}
		if !onlyMembers(tool, "type", "name", "description", "input_schema", "cache_control") {
			r.block(FeatureUnknownField)
		}
		name, _ := tool.str("name")
		description, _ := tool.str("description")
		schema := tool["input_schema"]
		if name == "" || !isObject(schema) {
			r.block(FeatureToolType)
			continue
		}
		r.Tools = append(r.Tools, Tool{Name: name, Description: description, Schema: compactRaw(schema)})
	}
}

func parseAnthropicToolChoice(r *Request, value json.RawMessage) {
	choice, ok := decodeObject(value)
	if !ok {
		r.block(FeatureToolChoice)
		return
	}
	if !onlyMembers(choice, "type", "name", "disable_parallel_tool_use") {
		r.block(FeatureUnknownField)
	}
	if raw, ok := choice.present("disable_parallel_tool_use", false); ok {
		disabled, valid := asBool(raw)
		if !valid {
			r.block(FeatureToolChoice)
		}
		parallel := !disabled
		r.ParallelToolCalls = &parallel
	}
	kind, _ := choice.str("type")
	switch kind {
	case "auto":
		r.ToolChoice = ToolChoice{Mode: ChoiceAuto}
	case "any":
		r.ToolChoice = ToolChoice{Mode: ChoiceRequired}
	case "none":
		r.ToolChoice = ToolChoice{Mode: ChoiceNone}
	case "tool":
		name, _ := choice.str("name")
		if name == "" {
			r.block(FeatureToolChoice)
		}
		r.ToolChoice = ToolChoice{Mode: ChoiceNamed, Name: name}
	default:
		r.block(FeatureToolChoice)
	}
}

type anthropicText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type anthropicSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type anthropicContent struct {
	Type      string           `json:"type"`
	Text      *string          `json:"text,omitempty"`
	Source    *anthropicSource `json:"source,omitempty"`
	ID        string           `json:"id,omitempty"`
	Name      string           `json:"name,omitempty"`
	Input     json.RawMessage  `json:"input,omitempty"`
	ToolUseID string           `json:"tool_use_id,omitempty"`
	Content   any              `json:"content,omitempty"`
	IsError   bool             `json:"is_error,omitempty"`
}

type anthropicMessage struct {
	Role    Role               `json:"role"`
	Content []anthropicContent `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type anthropicToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use,omitempty"`
}

type anthropicRequest struct {
	Model         string               `json:"model"`
	MaxTokens     int64                `json:"max_tokens"`
	System        []anthropicText      `json:"system,omitempty"`
	Messages      []anthropicMessage   `json:"messages"`
	Tools         []anthropicTool      `json:"tools,omitempty"`
	ToolChoice    *anthropicToolChoice `json:"tool_choice,omitempty"`
	Temperature   json.RawMessage      `json:"temperature,omitempty"`
	TopP          json.RawMessage      `json:"top_p,omitempty"`
	StopSequences []string             `json:"stop_sequences,omitempty"`
	Stream        bool                 `json:"stream,omitempty"`
	Metadata      *struct {
		UserID string `json:"user_id"`
	} `json:"metadata,omitempty"`
}

func anthropicImage(image Image) *anthropicSource {
	if image.inline() {
		return &anthropicSource{Type: "base64", MediaType: image.MediaType, Data: image.Data}
	}
	return &anthropicSource{Type: "url", URL: image.URL}
}

func anthropicBlock(block Block) (anthropicContent, bool) {
	switch block.Kind {
	case BlockText:
		if block.Text == "" {
			return anthropicContent{}, false
		}
		text := block.Text
		return anthropicContent{Type: "text", Text: &text}, true
	case BlockImage:
		return anthropicContent{Type: "image", Source: anthropicImage(block.Image)}, true
	case BlockToolCall:
		return anthropicContent{Type: "tool_use", ID: ToolID(block.ToolCallID), Name: block.ToolName, Input: block.Arguments}, true
	case BlockToolResult:
		out := anthropicContent{Type: "tool_result", ToolUseID: ToolID(block.ToolCallID), IsError: block.IsError}
		parts := make([]anthropicContent, 0, len(block.Result))
		for _, part := range block.Result {
			if rendered, ok := anthropicBlock(part); ok {
				parts = append(parts, rendered)
			}
		}
		if len(parts) > 0 {
			out.Content = parts
		}
		return out, true
	}
	return anthropicContent{}, false
}

func renderAnthropicRequest(r *Request, model string) ([]byte, error) {
	out := anthropicRequest{Model: model, MaxTokens: r.MaxOutputTokens, Temperature: number(r.Temperature), TopP: number(r.TopP),
		StopSequences: r.StopSequences, Stream: r.Stream, Messages: make([]anthropicMessage, 0, len(r.Turns))}
	if out.MaxTokens == 0 {
		out.MaxTokens = DefaultMaxOutputTokens
	}
	for _, text := range r.Instructions {
		if text != "" {
			out.System = append(out.System, anthropicText{Type: "text", Text: text})
		}
	}
	for _, turn := range r.Turns {
		message := anthropicMessage{Role: turn.Role, Content: make([]anthropicContent, 0, len(turn.Blocks))}
		for _, block := range turn.Blocks {
			if rendered, ok := anthropicBlock(block); ok {
				message.Content = append(message.Content, rendered)
			}
		}
		if len(message.Content) == 0 {
			empty := ""
			message.Content = append(message.Content, anthropicContent{Type: "text", Text: &empty})
		}
		out.Messages = append(out.Messages, message)
	}
	for _, tool := range r.Tools {
		out.Tools = append(out.Tools, anthropicTool{Name: tool.Name, Description: tool.Description, InputSchema: tool.Schema})
	}
	disableParallel := r.ParallelToolCalls != nil && !*r.ParallelToolCalls
	switch r.ToolChoice.Mode {
	case ChoiceAuto:
		out.ToolChoice = &anthropicToolChoice{Type: "auto"}
	case ChoiceNone:
		out.ToolChoice = &anthropicToolChoice{Type: "none"}
	case ChoiceRequired:
		out.ToolChoice = &anthropicToolChoice{Type: "any"}
	case ChoiceNamed:
		out.ToolChoice = &anthropicToolChoice{Type: "tool", Name: r.ToolChoice.Name}
	case ChoiceUnset:
		if disableParallel && len(r.Tools) > 0 {
			out.ToolChoice = &anthropicToolChoice{Type: "auto"}
		}
	}
	if out.ToolChoice != nil && out.ToolChoice.Type != "none" {
		out.ToolChoice.DisableParallelToolUse = disableParallel
	}
	if r.User != "" {
		out.Metadata = &struct {
			UserID string `json:"user_id"`
		}{r.User}
	}
	return marshal(out)
}
