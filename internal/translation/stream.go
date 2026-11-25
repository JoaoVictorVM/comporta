package translation

import (
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// StreamOptions describe the stream being translated.
type StreamOptions struct {
	// Model is the fallback when the provider's events name none.
	Model string
	// IncludeUsage is whether an OpenAI Chat client asked for the usage chunk.
	IncludeUsage bool
	Now          time.Time
	// MaxOutputBytes bounds what a Responses client must receive again in the
	// done events and in the final object.
	MaxOutputBytes int
}

// StreamTranslator converts the events of one provider stream into events of
// the client's protocol, one event at a time. It keeps only the state the
// client protocol needs to stay valid: indices, identifiers, the stop and, for
// a Responses client, the text it must repeat in its done events.
type StreamTranslator struct {
	decoder decoder
	encoder encoder
	usage   Usage
	done    bool
	out     [][]byte
}

// streamEvent is the canonical unit between a decoder and an encoder.
type streamEvent struct {
	kind     eventKind
	model    string
	tool     bool
	id, name string
	text     string
	stop     StopReason
	sequence string
}

type eventKind int

const (
	eventStart eventKind = iota + 1
	eventBlockStart
	eventText
	eventArguments
	eventBlockStop
	eventStop
	eventDone
)

type decoder interface {
	decode(name string, data []byte, emit func(streamEvent) error) error
}

type encoder interface {
	encode(event streamEvent, usage Usage) ([][]byte, error)
	errorEvent(code, message string) []byte
}

// NewStreamTranslator returns the translator from a provider protocol to a
// client protocol.
func NewStreamTranslator(source, client WireFormat, options StreamOptions) (*StreamTranslator, error) {
	if source == client {
		return nil, errors.New("nothing to translate")
	}
	t := &StreamTranslator{}
	switch source {
	case AnthropicMessages:
		t.decoder = &anthropicDecoder{}
	case OpenAIChat:
		t.decoder = &chatDecoder{}
	default:
		return nil, errors.New("streams are not decoded from this format")
	}
	switch client {
	case AnthropicMessages:
		t.encoder = &anthropicEncoder{model: options.Model}
	case OpenAIChat:
		t.encoder = &chatEncoder{model: options.Model, includeUsage: options.IncludeUsage, created: options.Now.Unix()}
	case OpenAIResponses:
		limit := options.MaxOutputBytes
		if limit <= 0 {
			limit = 64_000_000
		}
		t.encoder = &responsesEncoder{model: options.Model, created: options.Now, limit: limit}
	default:
		return nil, errors.New("unknown client format")
	}
	return t, nil
}

// Feed translates one provider event, given the usage known after it, and
// returns the client events it produced, each a complete SSE frame. Comments
// and events of no interest produce nothing.
func (t *StreamTranslator) Feed(name string, data []byte, usage Usage) ([][]byte, error) {
	if t.done {
		return nil, nil
	}
	t.usage = usage
	t.out = t.out[:0]
	err := t.decoder.decode(name, data, func(event streamEvent) error {
		frames, err := t.encoder.encode(event, t.usage)
		if err != nil {
			return err
		}
		t.out = append(t.out, frames...)
		if event.kind == eventDone {
			t.done = true
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return t.out, nil
}

// Done reports whether the client stream was completed.
func (t *StreamTranslator) Done() bool { return t.done }

// ErrorEvent renders an error in the client protocol for a stream already
// committed. Code and message come from the gateway's fixed vocabulary.
func (t *StreamTranslator) ErrorEvent(code, message string) []byte {
	return t.encoder.errorEvent(code, message)
}

func namedFrame(name string, payload any) ([]byte, error) {
	data, err := marshal(payload)
	if err != nil {
		return nil, err
	}
	return append(append([]byte("event: "+name+"\ndata: "), data...), "\n\n"...), nil
}

func dataFrame(payload any) ([]byte, error) {
	data, err := marshal(payload)
	if err != nil {
		return nil, err
	}
	return append(append([]byte("data: "), data...), "\n\n"...), nil
}

// anthropicDecoder reads Messages events. Reasoning, citations and server
// tools fail the translation instead of being dropped.
type anthropicDecoder struct {
	started, open, stopped bool
	tool                   bool
}

func (d *anthropicDecoder) decode(name string, data []byte, emit func(streamEvent) error) error {
	if len(data) == 0 {
		return nil
	}
	root, ok := decodeObject(data)
	if !ok {
		return badResponse("event")
	}
	kind, _ := root.str("type")
	if kind == "" {
		kind = name
	}
	switch kind {
	case "message_start":
		message, ok := decodeObject(root["message"])
		if !ok || d.started {
			return badResponse("message_start")
		}
		if items, ok := decodeArray(message["content"]); ok && len(items) > 0 {
			return badResponse("prefilled content")
		}
		d.started = true
		model, _ := message.str("model")
		return emit(streamEvent{kind: eventStart, model: model})
	case "content_block_start":
		block, ok := decodeObject(root["content_block"])
		if !ok || !d.started || d.open {
			return badResponse("content_block_start")
		}
		blockType, _ := block.str("type")
		switch blockType {
		case "text":
			d.open, d.tool = true, false
			if err := emit(streamEvent{kind: eventBlockStart}); err != nil {
				return err
			}
			if text, _ := block.str("text"); text != "" {
				return emit(streamEvent{kind: eventText, text: text})
			}
			return nil
		case "tool_use":
			id, _ := block.str("id")
			toolName, _ := block.str("name")
			if id == "" || toolName == "" {
				return badResponse("tool_use")
			}
			d.open, d.tool = true, true
			return emit(streamEvent{kind: eventBlockStart, tool: true, id: id, name: toolName})
		}
		return badResponse("content block type")
	case "content_block_delta":
		delta, ok := decodeObject(root["delta"])
		if !ok || !d.open {
			return badResponse("content_block_delta")
		}
		deltaType, _ := delta.str("type")
		switch {
		case deltaType == "text_delta" && !d.tool:
			text, _ := delta.str("text")
			return emit(streamEvent{kind: eventText, text: text})
		case deltaType == "input_json_delta" && d.tool:
			partial, _ := delta.str("partial_json")
			return emit(streamEvent{kind: eventArguments, text: partial})
		}
		return badResponse("delta type")
	case "content_block_stop":
		if !d.open {
			return badResponse("content_block_stop")
		}
		d.open = false
		return emit(streamEvent{kind: eventBlockStop})
	case "message_delta":
		delta, ok := decodeObject(root["delta"])
		if !ok || d.open {
			return badResponse("message_delta")
		}
		reason, has := delta.str("stop_reason")
		if !has {
			return nil
		}
		stop, err := anthropicStop(reason)
		if err != nil {
			return err
		}
		sequence, _ := delta.str("stop_sequence")
		d.stopped = true
		return emit(streamEvent{kind: eventStop, stop: stop, sequence: sequence})
	case "message_stop":
		if !d.stopped {
			return badResponse("message_stop without stop reason")
		}
		return emit(streamEvent{kind: eventDone})
	}
	// ping, error and event types added later carry nothing to translate; the
	// proxy already handled an error event.
	return nil
}

// chatDecoder reads Chat Completions chunks.
type chatDecoder struct {
	started, stopped bool
	open             int // 0 none, 1 text, 2 tool
	toolIndex        int
}

const (
	openNone = iota
	openText
	openTool
)

type chatChunk struct {
	Model   string `json:"model"`
	Choices []struct {
		Index        int             `json:"index"`
		Delta        json.RawMessage `json:"delta"`
		FinishReason *string         `json:"finish_reason"`
	} `json:"choices"`
}

type chatDeltaCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func (d *chatDecoder) closeBlock(emit func(streamEvent) error) error {
	if d.open == openNone {
		return nil
	}
	d.open = openNone
	return emit(streamEvent{kind: eventBlockStop})
}

func (d *chatDecoder) decode(_ string, data []byte, emit func(streamEvent) error) error {
	if len(data) == 0 {
		return nil
	}
	if strings.TrimSpace(string(data)) == "[DONE]" {
		if !d.stopped {
			return badResponse("stream ended without finish reason")
		}
		return emit(streamEvent{kind: eventDone})
	}
	var chunk chatChunk
	if json.Unmarshal(data, &chunk) != nil {
		return badResponse("chunk")
	}
	if len(chunk.Choices) > 1 {
		return badResponse("choices")
	}
	if !d.started {
		d.started = true
		if err := emit(streamEvent{kind: eventStart, model: chunk.Model}); err != nil {
			return err
		}
	}
	if len(chunk.Choices) == 0 {
		return nil
	}
	choice := chunk.Choices[0]
	if d.stopped {
		return badResponse("content after finish reason")
	}
	if len(choice.Delta) > 0 && !isNull(choice.Delta) {
		if err := d.delta(choice.Delta, emit); err != nil {
			return err
		}
	}
	if choice.FinishReason == nil {
		return nil
	}
	stop, err := chatStop(*choice.FinishReason)
	if err != nil {
		return err
	}
	if err := d.closeBlock(emit); err != nil {
		return err
	}
	d.stopped = true
	return emit(streamEvent{kind: eventStop, stop: stop})
}

func (d *chatDecoder) delta(raw json.RawMessage, emit func(streamEvent) error) error {
	delta, ok := decodeObject(raw)
	if !ok {
		return badResponse("delta")
	}
	for _, key := range []string{"refusal", "function_call", "audio", "reasoning", "reasoning_content"} {
		if value, has := delta.present(key, true); has {
			if text, isText := asString(value); isText && text == "" {
				continue
			}
			return badResponse("delta member")
		}
	}
	if content, has := delta.present("content", true); has {
		text, isText := asString(content)
		if !isText {
			return badResponse("content")
		}
		if text != "" {
			if d.open != openText {
				if err := d.closeBlock(emit); err != nil {
					return err
				}
				d.open = openText
				if err := emit(streamEvent{kind: eventBlockStart}); err != nil {
					return err
				}
			}
			if err := emit(streamEvent{kind: eventText, text: text}); err != nil {
				return err
			}
		}
	}
	raw, has := delta.present("tool_calls", true)
	if !has {
		return nil
	}
	var calls []chatDeltaCall
	if json.Unmarshal(raw, &calls) != nil {
		return badResponse("tool_calls")
	}
	for _, call := range calls {
		if call.Type != "" && call.Type != "function" {
			return badResponse("tool call type")
		}
		if d.open != openTool || call.Index != d.toolIndex {
			if call.ID == "" || call.Function.Name == "" {
				return badResponse("tool call start")
			}
			if err := d.closeBlock(emit); err != nil {
				return err
			}
			d.open, d.toolIndex = openTool, call.Index
			if err := emit(streamEvent{kind: eventBlockStart, tool: true, id: call.ID, name: call.Function.Name}); err != nil {
				return err
			}
		}
		if call.Function.Arguments != "" {
			if err := emit(streamEvent{kind: eventArguments, text: call.Function.Arguments}); err != nil {
				return err
			}
		}
	}
	return nil
}

// anthropicEncoder writes Messages events. Chat reports usage only at the
// end, so message_start carries zero and message_delta the real counters.
type anthropicEncoder struct {
	model    string
	index    int
	open     bool
	stop     *streamEvent
	finished bool
}

func (e *anthropicEncoder) encode(event streamEvent, usage Usage) ([][]byte, error) {
	var payload any
	name := ""
	switch event.kind {
	case eventStart:
		if event.model != "" {
			e.model = event.model
		}
		name = "message_start"
		payload = map[string]any{"type": name, "message": map[string]any{
			"id": newID("msg_"), "type": "message", "role": "assistant", "model": e.model, "content": []any{},
			"stop_reason": nil, "stop_sequence": nil, "usage": anthropicUsageOf(Usage{Input: usage.Input, CacheRead: usage.CacheRead, CacheWrite: usage.CacheWrite}),
		}}
	case eventBlockStart:
		e.open = true
		name = "content_block_start"
		block := map[string]any{"type": "text", "text": ""}
		if event.tool {
			block = map[string]any{"type": "tool_use", "id": ToolID(event.id), "name": event.name, "input": map[string]any{}}
		}
		payload = map[string]any{"type": name, "index": e.index, "content_block": block}
	case eventText:
		name = "content_block_delta"
		payload = map[string]any{"type": name, "index": e.index, "delta": map[string]any{"type": "text_delta", "text": event.text}}
	case eventArguments:
		name = "content_block_delta"
		payload = map[string]any{"type": name, "index": e.index, "delta": map[string]any{"type": "input_json_delta", "partial_json": event.text}}
	case eventBlockStop:
		name = "content_block_stop"
		payload = map[string]any{"type": name, "index": e.index}
		e.index++
		e.open = false
	case eventStop:
		stored := event
		e.stop = &stored
		return nil, nil
	case eventDone:
		if e.stop == nil {
			return nil, badResponse("no stop reason")
		}
		reason, err := anthropicStopName(e.stop.stop)
		if err != nil {
			return nil, err
		}
		var sequence any
		if e.stop.stop == StopSequence && e.stop.sequence != "" {
			sequence = e.stop.sequence
		}
		delta, err := namedFrame("message_delta", map[string]any{"type": "message_delta",
			"delta": map[string]any{"stop_reason": reason, "stop_sequence": sequence}, "usage": anthropicUsageOf(usage)})
		if err != nil {
			return nil, err
		}
		stop, err := namedFrame("message_stop", map[string]any{"type": "message_stop"})
		if err != nil {
			return nil, err
		}
		e.finished = true
		return [][]byte{delta, stop}, nil
	}
	frame, err := namedFrame(name, payload)
	if err != nil {
		return nil, err
	}
	return [][]byte{frame}, nil
}

func (e *anthropicEncoder) errorEvent(code, message string) []byte {
	frame, _ := namedFrame("error", map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": message, "comporta_code": code}})
	return frame
}

// chatEncoder writes Chat Completions chunks.
type chatEncoder struct {
	id           string
	model        string
	created      int64
	includeUsage bool
	tools        int
	stop         *StopReason
}

func (e *chatEncoder) chunk(delta map[string]any, finish any) ([]byte, error) {
	return dataFrame(map[string]any{"id": e.id, "object": "chat.completion.chunk", "created": e.created, "model": e.model,
		"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish, "logprobs": nil}}})
}

func (e *chatEncoder) encode(event streamEvent, usage Usage) ([][]byte, error) {
	var frame []byte
	var err error
	switch event.kind {
	case eventStart:
		if event.model != "" {
			e.model = event.model
		}
		e.id = newID("chatcmpl-")
		frame, err = e.chunk(map[string]any{"role": "assistant", "content": ""}, nil)
	case eventBlockStart:
		if !event.tool {
			return nil, nil
		}
		e.tools++
		frame, err = e.chunk(map[string]any{"tool_calls": []any{map[string]any{"index": e.tools - 1, "id": ToolID(event.id), "type": "function",
			"function": map[string]any{"name": event.name, "arguments": ""}}}}, nil)
	case eventText:
		frame, err = e.chunk(map[string]any{"content": event.text}, nil)
	case eventArguments:
		frame, err = e.chunk(map[string]any{"tool_calls": []any{map[string]any{"index": e.tools - 1, "function": map[string]any{"arguments": event.text}}}}, nil)
	case eventBlockStop:
		return nil, nil
	case eventStop:
		reason, nameErr := chatStopName(event.stop)
		if nameErr != nil {
			return nil, nameErr
		}
		stop := event.stop
		e.stop = &stop
		frame, err = e.chunk(map[string]any{}, reason)
	case eventDone:
		if e.stop == nil {
			return nil, badResponse("no stop reason")
		}
		var frames [][]byte
		if e.includeUsage {
			usageFrame, err := dataFrame(map[string]any{"id": e.id, "object": "chat.completion.chunk", "created": e.created, "model": e.model,
				"choices": []any{}, "usage": chatUsageOf(usage)})
			if err != nil {
				return nil, err
			}
			frames = append(frames, usageFrame)
		}
		return append(frames, []byte("data: [DONE]\n\n")), nil
	}
	if err != nil {
		return nil, err
	}
	return [][]byte{frame}, nil
}

func (e *chatEncoder) errorEvent(code, message string) []byte {
	frame, _ := dataFrame(map[string]any{"error": map[string]any{"type": "server_error", "message": message, "comporta_code": code}})
	return frame
}

// responsesEncoder writes Responses API events with sequence numbers. The done
// events and the final object repeat the output, so its text is kept, up to
// the response size limit.
type responsesEncoder struct {
	model    string
	created  time.Time
	limit    int
	size     int
	sequence int
	object   responsesObject
	current  *responsesItem
	text     strings.Builder
	stop     *StopReason
}

func (e *responsesEncoder) frame(name string, fields map[string]any) ([]byte, error) {
	fields["type"] = name
	fields["sequence_number"] = e.sequence
	e.sequence++
	return namedFrame(name, fields)
}

func (e *responsesEncoder) grow(n int) error {
	e.size += n
	if e.size > e.limit {
		return badResponse("output over the size limit")
	}
	return nil
}

func (e *responsesEncoder) encode(event streamEvent, usage Usage) ([][]byte, error) {
	var frames [][]byte
	add := func(name string, fields map[string]any) error {
		frame, err := e.frame(name, fields)
		if err == nil {
			frames = append(frames, frame)
		}
		return err
	}
	var err error
	switch event.kind {
	case eventStart:
		if event.model != "" {
			e.model = event.model
		}
		e.object = newResponsesObject(newID("resp_"), e.model, e.created)
		snapshot := e.object
		if err = add("response.created", map[string]any{"response": snapshot}); err == nil {
			err = add("response.in_progress", map[string]any{"response": snapshot})
		}
	case eventBlockStart:
		index := len(e.object.Output)
		e.text.Reset()
		if event.tool {
			empty := ""
			item := responsesItem{ID: newID("fc_"), Type: "function_call", Status: "in_progress", CallID: ToolID(event.id), Name: event.name, Arguments: &empty}
			e.current = &item
			err = add("response.output_item.added", map[string]any{"output_index": index, "item": item})
			break
		}
		item := responsesItem{ID: newID("msg_"), Type: "message", Status: "in_progress", Role: "assistant", Content: []responsesContent{}}
		e.current = &item
		if err = add("response.output_item.added", map[string]any{"output_index": index, "item": item}); err == nil {
			err = add("response.content_part.added", map[string]any{"item_id": item.ID, "output_index": index, "content_index": 0,
				"part": responsesContent{Type: "output_text", Text: "", Annotations: []any{}}})
		}
	case eventText, eventArguments:
		if e.current == nil {
			return nil, badResponse("delta outside an item")
		}
		if err = e.grow(len(event.text)); err != nil {
			return nil, err
		}
		e.text.WriteString(event.text)
		index := len(e.object.Output)
		if event.kind == eventText {
			err = add("response.output_text.delta", map[string]any{"item_id": e.current.ID, "output_index": index, "content_index": 0, "delta": event.text, "logprobs": []any{}})
		} else {
			err = add("response.function_call_arguments.delta", map[string]any{"item_id": e.current.ID, "output_index": index, "delta": event.text})
		}
	case eventBlockStop:
		if e.current == nil {
			return nil, badResponse("stop outside an item")
		}
		index := len(e.object.Output)
		item := *e.current
		item.Status = "completed"
		text := e.text.String()
		if item.Type == "function_call" {
			item.Arguments = &text
			if err = add("response.function_call_arguments.done", map[string]any{"item_id": item.ID, "output_index": index, "name": item.Name, "arguments": text}); err == nil {
				err = add("response.output_item.done", map[string]any{"output_index": index, "item": item})
			}
		} else {
			part := responsesContent{Type: "output_text", Text: text, Annotations: []any{}}
			item.Content = []responsesContent{part}
			if err = add("response.output_text.done", map[string]any{"item_id": item.ID, "output_index": index, "content_index": 0, "text": text, "logprobs": []any{}}); err == nil {
				if err = add("response.content_part.done", map[string]any{"item_id": item.ID, "output_index": index, "content_index": 0, "part": part}); err == nil {
					err = add("response.output_item.done", map[string]any{"output_index": index, "item": item})
				}
			}
		}
		e.object.Output = append(e.object.Output, item)
		e.current = nil
	case eventStop:
		stop := event.stop
		e.stop = &stop
	case eventDone:
		if e.stop == nil {
			return nil, badResponse("no stop reason")
		}
		if err = finishResponses(&e.object, *e.stop, usage); err != nil {
			return nil, err
		}
		name := "response.completed"
		if e.object.Status == "incomplete" {
			name = "response.incomplete"
		}
		err = add(name, map[string]any{"response": e.object})
	}
	if err != nil {
		return nil, err
	}
	return frames, nil
}

func (e *responsesEncoder) errorEvent(code, message string) []byte {
	frame, _ := e.frame("error", map[string]any{"code": code, "message": message, "param": nil})
	return frame
}
