package proxy

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/JoaoVictorVM/comporta/internal/auth"
)

const maxSSEEventBytes = 10_000_000

var errEventTooLarge = errors.New("SSE event too large")

// sseEvent is one unit of the stream. Raw holds the original bytes and is valid only
// until the next call to the parser.
type sseEvent struct {
	Raw     []byte
	Name    string
	Data    []byte
	Comment bool
}

// sseParser frames a byte stream into events, for any chunk boundaries and for LF,
// CRLF or CR line endings. It buffers one event at most, never the whole response,
// because a usage event has to be seen whole to be removed.
type sseParser struct {
	buf        []byte
	eventStart int
	lineStart  int
	scan       int
	name       []byte
	data       []byte
	hasData    bool
	first      bool
}

func newSSEParser() *sseParser { return &sseParser{first: true} }

// Next returns the next complete event, or false when more input is needed.
func (p *sseParser) Next() (sseEvent, bool, error) {
	for {
		i := p.scan
		for i < len(p.buf) && p.buf[i] != '\n' && p.buf[i] != '\r' {
			i++
		}
		if i >= len(p.buf) {
			p.scan = i
			if len(p.buf)-p.eventStart > maxSSEEventBytes {
				return sseEvent{}, false, errEventTooLarge
			}
			return sseEvent{}, false, nil
		}
		next := i + 1
		if p.buf[i] == '\r' {
			// A CR at the end of the input may be half of a CRLF that the next
			// chunk completes, so the line waits.
			if next == len(p.buf) {
				p.scan = i
				return sseEvent{}, false, nil
			}
			if p.buf[next] == '\n' {
				next++
			}
		}
		begin := p.lineStart
		line := p.buf[begin:i]
		p.lineStart, p.scan = next, next
		if len(p.buf)-p.eventStart > maxSSEEventBytes {
			return sseEvent{}, false, errEventTooLarge
		}

		if p.first {
			p.first = false
			line = bytes.TrimPrefix(line, []byte("\xef\xbb\xbf"))
		}
		if len(line) == 0 {
			return p.emit(next), true, nil
		}
		// A comment alone is released at once; one inside an event stays with it.
		if line[0] == ':' && begin == p.eventStart {
			event := p.emit(next)
			event.Comment = true
			return event, true, nil
		}
		p.field(line)
	}
}

func (p *sseParser) field(line []byte) {
	name, value, _ := bytes.Cut(line, []byte(":"))
	value = bytes.TrimPrefix(value, []byte(" "))
	switch string(name) {
	case "event":
		p.name = append(p.name[:0], value...)
	case "data":
		if p.hasData {
			p.data = append(p.data, '\n')
		}
		p.data = append(p.data, value...)
		p.hasData = true
	}
}

func (p *sseParser) emit(end int) sseEvent {
	event := sseEvent{Raw: p.buf[p.eventStart:end], Name: string(p.name), Data: p.data}
	p.eventStart = end
	p.name, p.data, p.hasData = p.name[:0], nil, false
	return event
}

// Feed appends input. Bytes of events already returned are dropped first, which
// invalidates earlier Raw slices.
func (p *sseParser) Feed(chunk []byte) {
	if p.eventStart > 0 {
		kept := copy(p.buf, p.buf[p.eventStart:])
		p.buf = p.buf[:kept]
		p.lineStart -= p.eventStart
		p.scan -= p.eventStart
		p.eventStart = 0
	}
	p.buf = append(p.buf, chunk...)
}

// Finish handles the end of input. A final event with no closing blank line is
// returned when its lines are complete; a truncated line is simply discarded.
func (p *sseParser) Finish() (sseEvent, bool) {
	if p.scan < len(p.buf) && p.buf[p.scan] == '\r' {
		line := p.buf[p.lineStart:p.scan]
		p.scan++
		p.lineStart = p.scan
		if len(line) > 0 {
			p.field(line)
		}
	}
	if !p.hasData && len(p.name) == 0 {
		return sseEvent{}, false
	}
	return p.emit(p.lineStart), true
}

// eventAction is what the stream does with one event after inspecting it.
type eventAction int

const (
	actionForward eventAction = iota
	actionDrop
)

// streamState follows usage and completion across the events of one stream.
type streamState struct {
	format      auth.Format
	askedUsage  bool
	usage       Usage
	outputFinal bool
	terminal    bool
}

// streamFailure is the sanitized form of an error found inside the stream.
type streamFailure struct {
	failure Failure
}

func (e *streamFailure) Error() string { return e.failure.Message }

func invalidUpstream(message string) *streamFailure {
	return &streamFailure{Failure{502, CodeInvalidUpstreamResponse, ClassProtocol, message}}
}

// inspect updates the state with one event. Data that is not JSON belongs to an
// event the gateway does not understand, and it stays intact for the client.
func (s *streamState) inspect(event sseEvent, provider string) (eventAction, *streamFailure) {
	if event.Comment || len(event.Data) == 0 {
		return actionForward, nil
	}
	if s.format == auth.FormatOpenAI {
		return s.inspectOpenAI(event, provider)
	}
	return s.inspectAnthropic(event, provider)
}

func checkEventDepth(data []byte) *streamFailure {
	if jsonDepthExceeds(data, maxJSONDepth) {
		return invalidUpstream("provider sent an event nested too deeply")
	}
	return nil
}

func (s *streamState) inspectOpenAI(event sseEvent, provider string) (eventAction, *streamFailure) {
	if string(bytes.TrimSpace(event.Data)) == "[DONE]" {
		s.terminal = true
		return actionForward, nil
	}
	if !json.Valid(event.Data) {
		return actionForward, nil
	}
	if failure := checkEventDepth(event.Data); failure != nil {
		return actionForward, failure
	}
	var parsed struct {
		Choices json.RawMessage `json:"choices"`
		Usage   json.RawMessage `json:"usage"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(event.Data, &parsed); err != nil {
		return actionForward, nil
	}
	if len(parsed.Error) > 0 && !isJSONNull(parsed.Error) {
		return actionForward, &streamFailure{providerStreamFailure(openAIErrorKind(parsed.Error), provider)}
	}
	if len(parsed.Usage) == 0 || isJSONNull(parsed.Usage) {
		return actionForward, nil
	}
	if parsed.Usage[0] != '{' {
		return actionForward, invalidUpstream("provider sent invalid usage")
	}
	if err := s.usage.mergeOpenAI(parsed.Usage); err != nil {
		return actionForward, invalidUpstream("provider sent invalid usage")
	}
	s.outputFinal = s.usage.Output.Known
	if !s.askedUsage && isEmptyArray(parsed.Choices) {
		return actionDrop, nil
	}
	return actionForward, nil
}

func isEmptyArray(raw json.RawMessage) bool {
	var list []json.RawMessage
	return len(raw) > 0 && raw[0] == '[' && json.Unmarshal(raw, &list) == nil && len(list) == 0
}

func (s *streamState) inspectAnthropic(event sseEvent, provider string) (eventAction, *streamFailure) {
	if !json.Valid(event.Data) {
		return actionForward, nil
	}
	if failure := checkEventDepth(event.Data); failure != nil {
		return actionForward, failure
	}
	var parsed struct {
		Type    string `json:"type"`
		Message struct {
			Usage json.RawMessage `json:"usage"`
		} `json:"message"`
		Usage json.RawMessage `json:"usage"`
		Error struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(event.Data, &parsed); err != nil {
		return actionForward, nil
	}
	kind := parsed.Type
	if kind == "" {
		kind = event.Name
	}
	var usage json.RawMessage
	switch kind {
	case "message_start":
		usage = parsed.Message.Usage
	case "message_delta":
		usage = parsed.Usage
	case "message_stop":
		s.terminal = true
	case "error":
		return actionForward, &streamFailure{providerStreamFailure(parsed.Error.Type, provider)}
	}
	if len(usage) == 0 || isJSONNull(usage) {
		return actionForward, nil
	}
	if usage[0] != '{' || s.usage.mergeAnthropic(usage) != nil {
		return actionForward, invalidUpstream("provider sent invalid usage")
	}
	if kind == "message_delta" {
		var counters anthropicUsage
		if json.Unmarshal(usage, &counters) == nil && counters.Output != nil {
			s.outputFinal = true
		}
	}
	return actionForward, nil
}

func openAIErrorKind(raw json.RawMessage) string {
	var parsed struct {
		Type string `json:"type"`
		Code string `json:"code"`
	}
	if json.Unmarshal(raw, &parsed) != nil {
		return ""
	}
	if parsed.Type != "" {
		return parsed.Type
	}
	return parsed.Code
}

// providerStreamFailure rebuilds a provider's stream error from its category. The
// provider's message is never reused, since it may echo the prompt.
func providerStreamFailure(kind, provider string) Failure {
	switch kind {
	case "rate_limit_error", "rate_limit_exceeded":
		return Failure{429, CodeProviderRateLimited, ClassRateLimit, "provider " + provider + " rate limit reached"}
	case "overloaded_error":
		return Failure{statusOverloaded, CodeProviderOverloaded, ClassOverload, "provider " + provider + " is overloaded"}
	}
	return Failure{502, CodeProviderError, ClassUpstream5xx, "provider " + provider + " reported an error during the stream"}
}

func (s *streamState) usageComplete() bool {
	return s.terminal && s.usage.primaryKnown() && s.outputFinal
}
