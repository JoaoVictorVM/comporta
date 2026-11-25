// Package translation converts conversational requests, responses and event
// streams between Anthropic Messages, OpenAI Chat Completions and the
// conversational subset of the OpenAI Responses API. It works on bytes that
// were already validated and sanitized by the proxy and never depends on HTTP,
// storage or configuration. Every structure that may hold content keeps it
// out of fmt, JSON and slog output.
package translation

import (
	"encoding/json"
	"fmt"
	"log/slog"
)

// WireFormat is a conversational protocol on the wire.
type WireFormat string

const (
	AnthropicMessages WireFormat = "anthropic_messages"
	OpenAIChat        WireFormat = "openai_chat"
	OpenAIResponses   WireFormat = "openai_responses"
)

// Valid reports whether the format belongs to the closed set.
func (w WireFormat) Valid() bool {
	switch w {
	case AnthropicMessages, OpenAIChat, OpenAIResponses:
		return true
	}
	return false
}

// Feature is the closed reason why a request cannot be represented in another
// protocol. It is safe to persist, log and show to the client.
type Feature string

const (
	FeatureProviderBeta     Feature = "provider_beta"
	FeatureUnknownField     Feature = "unknown_field"
	FeatureUnsupportedRole  Feature = "unsupported_role"
	FeatureUnsupportedBlock Feature = "unsupported_content"
	FeatureImageSource      Feature = "image_source"
	FeatureToolType         Feature = "tool_type"
	FeatureToolChoice       Feature = "tool_choice"
	FeatureToolResult       Feature = "tool_result"
	FeatureStructuredOutput Feature = "structured_output"
	FeatureReasoning        Feature = "reasoning"
	FeatureStatefulResponse Feature = "stateful_response"
	FeatureAudio            Feature = "audio"
	FeatureFile             Feature = "file"
	FeatureResponseShape    Feature = "response_shape"
)

// Features lists every feature code, in a stable order.
func Features() []Feature {
	return []Feature{FeatureProviderBeta, FeatureUnknownField, FeatureUnsupportedRole, FeatureUnsupportedBlock, FeatureImageSource,
		FeatureToolType, FeatureToolChoice, FeatureToolResult, FeatureStructuredOutput, FeatureReasoning, FeatureStatefulResponse,
		FeatureAudio, FeatureFile, FeatureResponseShape}
}

// Valid reports whether the feature belongs to the closed set.
func (f Feature) Valid() bool {
	for _, known := range Features() {
		if f == known {
			return true
		}
	}
	return false
}

// Role is the speaker of a conversation turn.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// BlockKind is the closed set of content the representation carries.
type BlockKind int

const (
	BlockText BlockKind = iota + 1
	BlockImage
	BlockToolCall
	BlockToolResult
)

// Image is either a remote URL or inline base64 data. The gateway never
// fetches nor decodes it.
type Image struct {
	URL       string
	MediaType string
	Data      string
}

func (i Image) inline() bool { return i.URL == "" }

// dataURL renders inline data as a data URL, which is how OpenAI carries it.
func (i Image) dataURL() string {
	if !i.inline() {
		return i.URL
	}
	return "data:" + i.MediaType + ";base64," + i.Data
}

// Block is one piece of content of a turn.
type Block struct {
	Kind  BlockKind
	Text  string
	Image Image
	// ToolCallID correlates a call with its result. ToolName and Arguments
	// describe a call; Arguments is a JSON object.
	ToolCallID string
	ToolName   string
	Arguments  json.RawMessage
	// Result holds the text and image blocks of a tool result.
	Result  []Block
	IsError bool
}

// Turn is one message of the conversation.
type Turn struct {
	Role   Role
	Blocks []Block
}

// Tool is a client-side function the model may call.
type Tool struct {
	Name        string
	Description string
	Schema      json.RawMessage
	Strict      *bool
}

// ChoiceMode is how the model must use tools.
type ChoiceMode int

const (
	ChoiceUnset ChoiceMode = iota
	ChoiceAuto
	ChoiceNone
	ChoiceRequired
	ChoiceNamed
)

// ToolChoice is the tool policy of a request.
type ToolChoice struct {
	Mode ChoiceMode
	Name string
}

// DefaultMaxOutputTokens is sent to a provider that requires an output limit
// when the client gave none and no app cap applied.
const DefaultMaxOutputTokens = 4096

// Request is the canonical, in-memory form of a conversational request. It
// exists for one logical request and is never persisted.
type Request struct {
	Source            WireFormat
	Model             string
	Instructions      []string
	Turns             []Turn
	Tools             []Tool
	ToolChoice        ToolChoice
	ParallelToolCalls *bool
	MaxOutputTokens   int64
	// Temperature and TopP keep the client's number lexeme; empty is absent.
	Temperature   string
	TopP          string
	StopSequences []string
	Stream        bool
	// IncludeUsage is what an OpenAI Chat client asked for its stream.
	IncludeUsage bool
	// User is the pseudonym the proxy already wrote into the body.
	User string
	// blocked is the first feature without an equivalent in other protocols.
	blocked Feature
}

// Blocked returns the first feature that keeps this request from being sent
// in another protocol, or an empty value.
func (r *Request) Blocked() Feature { return r.blocked }

func (r *Request) block(feature Feature) {
	if r.blocked == "" {
		r.blocked = feature
	}
}

func (r *Request) summary() string {
	return fmt.Sprintf("{source:%s turns:%d tools:%d}", r.Source, len(r.Turns), len(r.Tools))
}

// Format writes counters only, for every verb.
func (r Request) Format(state fmt.State, _ rune) { _, _ = fmt.Fprint(state, r.summary()) }

// MarshalJSON writes counters only.
func (r Request) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Source WireFormat `json:"source"`
		Turns  int        `json:"turns"`
		Tools  int        `json:"tools"`
	}{r.Source, len(r.Turns), len(r.Tools)})
}

// LogValue writes counters only.
func (r Request) LogValue() slog.Value {
	return slog.GroupValue(slog.String("source", string(r.Source)), slog.Int("turns", len(r.Turns)), slog.Int("tools", len(r.Tools)))
}

// Format writes the kind only, for every verb.
func (b Block) Format(state fmt.State, _ rune) { _, _ = fmt.Fprintf(state, "{block:%d}", b.Kind) }

// MarshalJSON writes the kind only.
func (b Block) MarshalJSON() ([]byte, error) { return json.Marshal(struct{ Kind BlockKind }{b.Kind}) }

// LogValue writes the kind only.
func (b Block) LogValue() slog.Value { return slog.IntValue(int(b.Kind)) }

// StopReason is the normalized end of a response.
type StopReason int

const (
	StopUnknown StopReason = iota
	StopEnd
	StopMaxTokens
	StopToolUse
	StopSequence
	StopContentFilter
)

// Usage holds counters in the disjoint form the proxy settles with: Input
// excludes cache reads and writes. An unknown counter stays unknown.
type Usage struct {
	Input, Output, CacheRead, CacheWrite Counter
}

// Counter is a token count that may not have been reported.
type Counter struct {
	Value int64
	Known bool
}

// Known builds a reported counter.
func Known(value int64) Counter { return Counter{Value: value, Known: true} }

// promptTotal is what OpenAI reports as prompt tokens: every input token,
// cached or not.
func (u Usage) promptTotal() int64 {
	return u.Input.Value + u.CacheRead.Value + u.CacheWrite.Value
}

// Response is the canonical form of one complete answer. It exists for one
// attempt and is never persisted.
type Response struct {
	Model        string
	Blocks       []Block
	Stop         StopReason
	StopSequence string
}

// Format writes counters only, for every verb.
func (r Response) Format(state fmt.State, _ rune) {
	_, _ = fmt.Fprintf(state, "{blocks:%d stop:%d}", len(r.Blocks), r.Stop)
}

// MarshalJSON writes counters only.
func (r Response) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Blocks int `json:"blocks"`
		Stop   int `json:"stop"`
	}{len(r.Blocks), int(r.Stop)})
}

// LogValue writes counters only.
func (r Response) LogValue() slog.Value {
	return slog.GroupValue(slog.Int("blocks", len(r.Blocks)), slog.Int("stop", int(r.Stop)))
}

// MalformedError is a request that is invalid in its own protocol. Its message
// names a field from fixed vocabulary, never a value.
type MalformedError struct{ Message string }

func (e *MalformedError) Error() string { return e.Message }

func malformed(message string) error { return &MalformedError{Message: message} }

// ErrResponse is a response that cannot be represented in the client protocol
// or is invalid in its own. It never carries provider bytes.
type ErrResponse struct{ reason string }

func (e *ErrResponse) Error() string { return "response translation failed: " + e.reason }

func badResponse(reason string) error { return &ErrResponse{reason: reason} }
