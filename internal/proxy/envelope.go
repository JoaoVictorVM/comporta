package proxy

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/JoaoVictorVM/comporta/internal/auth"
	"github.com/JoaoVictorVM/comporta/internal/keys"
	"github.com/JoaoVictorVM/comporta/internal/useridentity"
)

// Operation distinguishes generation, which consumes tokens, from the advisory
// token count.
type Operation string

const (
	OperationGeneration  Operation = "generation"
	OperationCountTokens Operation = "count_tokens"
)

// UserSource tells where the raw user identifier was read from.
type UserSource string

const (
	UserSourceNone     UserSource = ""
	UserSourceHeader   UserSource = "header"
	UserSourceMetadata UserSource = "metadata.user_id"
	UserSourceField    UserSource = "user"
)

const headerComportaUser = "X-Comporta-User"

// Optional is a value whose absence differs from its zero value, such as a
// temperature of 0 or a token counter a provider did not report.
type Optional[T any] struct {
	Value T
	Known bool
}

func known[T any](value T) Optional[T] { return Optional[T]{Value: value, Known: true} }

// Envelope holds what the pipeline stages need to know about one request. The raw
// user identifier stays unexported and reachable only through RawUser until identity
// is resolved, when it is dropped. Every generic representation (fmt, slog, JSON) shows
// approved metadata only, which leaves out the pseudonym as well.
type Envelope struct {
	RequestID         string
	AppName           string
	KeyID             string
	ConfigGeneration  int64
	APIFormat         auth.Format
	Endpoint          string
	Operation         Operation
	ModelRequested    string
	ModelEffective    string
	MaxTokens         Optional[int64]
	OutputLimitField  string
	Stream            bool
	Temperature       Optional[string]
	UserSource        UserSource
	Identity          useridentity.Identity
	LastMessagesHash  Optional[[32]byte]
	RequestHash       [32]byte
	BodySizeOriginal  int64
	BodySizeEffective int64
	ImageCount        int64
	ClientAskedUsage  bool
	rawUser           string
}

// RawUser returns the identifier sent by the client, until identity is resolved. It is
// sensitive and must not reach a log, an event or a persisted record.
func (e *Envelope) RawUser() (string, UserSource, bool) {
	if e.UserSource == UserSourceNone || e.Identity.Resolved() {
		return "", e.UserSource, false
	}
	return e.rawUser, e.UserSource, true
}

type publicEnvelope struct {
	RequestID string `json:"request_id"`
	AppName   string `json:"app_name"`
	KeyID     string `json:"key_id"`
	Endpoint  string `json:"endpoint"`
	Model     string `json:"model"`
	Stream    bool   `json:"stream"`
}

func (e Envelope) public() publicEnvelope {
	return publicEnvelope{e.RequestID, e.AppName, e.KeyID, e.Endpoint, e.ModelEffective, e.Stream}
}

// MarshalJSON writes approved metadata only.
func (e Envelope) MarshalJSON() ([]byte, error) { return json.Marshal(e.public()) }

// Format writes approved metadata only, for every verb.
func (e Envelope) Format(state fmt.State, _ rune) {
	_, _ = fmt.Fprintf(state, "{request_id:%s app:%s endpoint:%s}", e.RequestID, e.AppName, e.Endpoint)
}

// LogValue writes approved metadata only.
func (e Envelope) LogValue() slog.Value {
	return slog.GroupValue(slog.String("request_id", e.RequestID), slog.String("endpoint", e.Endpoint))
}

func newRequestID() string {
	var id [16]byte
	_, _ = rand.Read(id[:])
	return hex.EncodeToString(id[:])
}

// checkRequestHeaders applies the media rules before any body byte is read.
func checkRequestHeaders(r *http.Request) (Failure, bool) {
	unsupported := func(message string) (Failure, bool) {
		return Failure{http.StatusUnsupportedMediaType, CodeUnsupportedMediaType, ClassInvalidRequest, message}, false
	}
	types := r.Header.Values("Content-Type")
	if len(types) != 1 {
		return unsupported("Content-Type must be application/json")
	}
	mediaType, _, err := mime.ParseMediaType(types[0])
	if err != nil || mediaType != "application/json" {
		return unsupported("Content-Type must be application/json")
	}
	for _, encoding := range r.Header.Values("Content-Encoding") {
		if !strings.EqualFold(strings.TrimSpace(encoding), "identity") {
			return unsupported("Content-Encoding is not supported; send an uncompressed body")
		}
	}
	return Failure{}, true
}

// prepared is the outcome of reading a request: its envelope, the parsed document
// and the patched body that goes upstream.
type prepared struct {
	envelope *Envelope
	document *Document
	patches  []Patch
}

func (p *prepared) effectiveBody() []byte { return p.document.Apply(p.patches) }

// parseRequest validates the fields the gateway depends on and builds the envelope.
// Rules about what the fields may contain semantically stay with the provider.
func parseRequest(route Route, r *http.Request, body []byte, identity keys.Identity, generation int64) (*prepared, *Failure) {
	reject := func(message string) (*prepared, *Failure) {
		failure := invalidRequest(message)
		return nil, &failure
	}
	document, err := ParseDocument(body)
	if err != nil {
		return reject(err.Error())
	}
	format := route.format(r)
	envelope := &Envelope{
		RequestID:        newRequestID(),
		AppName:          identity.AppName(),
		KeyID:            identity.KeyID(),
		ConfigGeneration: generation,
		APIFormat:        format,
		Endpoint:         route.path(),
		Operation:        OperationGeneration,
		BodySizeOriginal: int64(len(body)),
	}
	if route == RouteCountTokens {
		envelope.Operation = OperationCountTokens
	}

	model, ok := document.Path("model")
	if !ok {
		return reject("model is required")
	}
	name, ok := document.String(model)
	if !ok || name == "" {
		return reject("model must be a non-empty string")
	}
	envelope.ModelRequested, envelope.ModelEffective = name, name

	messages, ok := document.Path("messages")
	if !ok || document.Kind(messages) != '[' {
		return reject("messages must be an array")
	}

	lenient := format == auth.FormatOpenAI
	if message := readStream(document, route, envelope, lenient); message != "" {
		return reject(message)
	}
	if message := readOutputLimit(document, route, envelope, lenient); message != "" {
		return reject(message)
	}
	if message := readTemperature(document, envelope, lenient); message != "" {
		return reject(message)
	}
	if message := readUser(document, r, envelope); message != "" {
		return reject(message)
	}

	envelope.ImageCount = countImages(document, messages, format)
	if digest, ok := lastMessagesHash(document, messages, format); ok {
		envelope.LastMessagesHash = known(digest)
	}

	result := &prepared{envelope: envelope, document: document}
	if message := applyUsagePatch(result); message != "" {
		return reject(message)
	}
	result.refreshHashes()
	return result, nil
}

// refreshHashes recomputes what depends on the effective body. It must run again
// after any stage adds a patch. The gateway's own usage request is left out of the
// request hash: it does not change what the client asked for.
func (p *prepared) refreshHashes() {
	semantic := make([]Patch, 0, len(p.patches))
	for _, patch := range p.patches {
		if !patch.transportOnly {
			semantic = append(semantic, patch)
		}
	}
	p.envelope.RequestHash = sha256.Sum256(p.document.Apply(semantic))
	p.envelope.BodySizeEffective = int64(len(p.effectiveBody()))
}

func isNull(document *Document, span Span) bool { return document.Kind(span) == 'n' }

// present treats an explicit null as absent only where the SDK is allowed to send
// it: the OpenAI API documents these fields as nullable, the Anthropic API does not.
func present(document *Document, lenient bool, keys ...string) (Span, bool) {
	span, ok := document.Path(keys...)
	if !ok || (lenient && isNull(document, span)) {
		return Span{}, false
	}
	return span, true
}

func readStream(document *Document, route Route, envelope *Envelope, lenient bool) string {
	span, ok := present(document, lenient, "stream")
	if !ok {
		return ""
	}
	switch string(document.Bytes(span)) {
	case "true":
		if route == RouteCountTokens {
			return "stream is not supported on count_tokens"
		}
		envelope.Stream = true
	case "false":
	default:
		return "stream must be a boolean"
	}
	return ""
}

func parsePositive(document *Document, span Span) (int64, bool) {
	value, err := strconv.ParseInt(string(document.Bytes(span)), 10, 64)
	return value, err == nil && value > 0
}

func readOutputLimit(document *Document, route Route, envelope *Envelope, lenient bool) string {
	switch route {
	case RouteMessages:
		span, ok := document.Path("max_tokens")
		if !ok {
			return "max_tokens is required"
		}
		value, valid := parsePositive(document, span)
		if !valid {
			return "max_tokens must be a positive integer"
		}
		envelope.MaxTokens, envelope.OutputLimitField = known(value), "max_tokens"
	case RouteChatCompletions:
		maxTokens, hasMax := present(document, lenient, "max_tokens")
		maxCompletion, hasCompletion := present(document, lenient, "max_completion_tokens")
		if hasMax && hasCompletion {
			return "max_tokens and max_completion_tokens cannot be used together"
		}
		field, span := "max_tokens", maxTokens
		if hasCompletion {
			field, span = "max_completion_tokens", maxCompletion
		}
		if !hasMax && !hasCompletion {
			return ""
		}
		value, valid := parsePositive(document, span)
		if !valid {
			return field + " must be a positive integer"
		}
		envelope.MaxTokens, envelope.OutputLimitField = known(value), field
	}
	return ""
}

func readTemperature(document *Document, envelope *Envelope, lenient bool) string {
	span, ok := present(document, lenient, "temperature")
	if !ok {
		return ""
	}
	lexeme := string(document.Bytes(span))
	value, err := strconv.ParseFloat(lexeme, 64)
	if err != nil || math.IsInf(value, 0) || math.IsNaN(value) {
		return "temperature must be a finite number"
	}
	envelope.Temperature = known(lexeme)
	return ""
}

// readUser follows the precedence header, metadata.user_id, user. Lower-precedence
// fields are still type-checked so that they are never silently reinterpreted.
func readUser(document *Document, r *http.Request, envelope *Envelope) string {
	metadataUser, hasMetadata := document.Path("metadata", "user_id")
	if hasMetadata {
		value, ok := document.String(metadataUser)
		if !ok {
			return "metadata.user_id must be a string"
		}
		envelope.rawUser, envelope.UserSource = value, UserSourceMetadata
	}
	if field, ok := document.Path("user"); ok {
		value, isString := document.String(field)
		if !isString {
			return "user must be a string"
		}
		if !hasMetadata {
			envelope.rawUser, envelope.UserSource = value, UserSourceField
		}
	}
	headers := r.Header.Values(headerComportaUser)
	if len(headers) > 1 {
		return headerComportaUser + " must not be repeated"
	}
	if len(headers) == 1 {
		envelope.rawUser, envelope.UserSource = headers[0], UserSourceHeader
	}
	return ""
}

// applyUsagePatch asks an OpenAI stream to report usage. stream_options is validated
// whenever present, and patched only when the request streams.
func applyUsagePatch(p *prepared) string {
	envelope, document := p.envelope, p.document
	if envelope.APIFormat != auth.FormatOpenAI {
		return ""
	}
	options, hasOptions := document.Path("stream_options")
	if hasOptions && !isNull(document, options) && document.Kind(options) != '{' {
		return "stream_options must be an object or null"
	}
	hasObject := hasOptions && document.Kind(options) == '{'
	if hasObject {
		if flag, ok := document.Member(options, "include_usage"); ok {
			switch string(document.Bytes(flag)) {
			case "true":
				envelope.ClientAskedUsage = true
			case "false":
			default:
				return "stream_options.include_usage must be a boolean"
			}
		}
	}
	if !envelope.Stream || envelope.ClientAskedUsage {
		return ""
	}
	var patch Patch
	var err error
	switch {
	case hasObject:
		patch, err = document.SetMember(options, "include_usage", []byte("true"))
	case hasOptions:
		patch = Patch{At: options, Text: []byte(`{"include_usage":true}`)}
	default:
		patch, err = document.SetMember(document.Root(), "stream_options", []byte(`{"include_usage":true}`))
	}
	if err != nil {
		return "request body could not be prepared"
	}
	patch.transportOnly = true
	p.patches = append(p.patches, patch)
	return ""
}

func eachContentBlock(document *Document, message Span, visit func(block Span, kind string)) {
	if document.Kind(message) != '{' {
		return
	}
	content, ok := document.Member(message, "content")
	if !ok || document.Kind(content) != '[' {
		return
	}
	for _, block := range document.Elements(content) {
		if document.Kind(block) != '{' {
			continue
		}
		kindSpan, ok := document.Member(block, "type")
		if !ok {
			continue
		}
		if kind, isString := document.String(kindSpan); isString {
			visit(block, kind)
		}
	}
}

// countImages looks only at the known locations of image blocks. Unknown blocks
// stay intact and uncounted.
func countImages(document *Document, messages Span, format auth.Format) int64 {
	var count int64
	for _, message := range document.Elements(messages) {
		eachContentBlock(document, message, func(_ Span, kind string) {
			if format == auth.FormatAnthropic && kind == "image" {
				count++
			}
			if format == auth.FormatOpenAI && (kind == "image_url" || kind == "input_image") {
				count++
			}
		})
	}
	return count
}

func roleOf(document *Document, message Span) string {
	if document.Kind(message) != '{' {
		return ""
	}
	span, ok := document.Member(message, "role")
	if !ok {
		return ""
	}
	role, _ := document.String(span)
	return role
}

// lastMessagesHash signs the last user message and the last tool result. Absent
// parts get distinct markers; with neither there is no signature, so unrelated empty
// traffic can never share one.
func lastMessagesHash(document *Document, messages Span, format auth.Format) ([32]byte, bool) {
	var lastUser, lastTool Optional[Span]
	for _, message := range document.Elements(messages) {
		role := roleOf(document, message)
		if role == "user" {
			lastUser = known(message)
		}
		if format == auth.FormatOpenAI && role == "tool" {
			lastTool = known(message)
		}
		if format == auth.FormatAnthropic {
			eachContentBlock(document, message, func(block Span, kind string) {
				if kind == "tool_result" {
					lastTool = known(block)
				}
			})
		}
	}
	if !lastUser.Known && !lastTool.Known {
		return [32]byte{}, false
	}
	digest := sha256.New()
	writeTagged(digest, 'f', []byte(providerFormat(format)))
	for _, part := range []Optional[Span]{lastUser, lastTool} {
		if !part.Known {
			writeTagged(digest, 'a', nil)
			continue
		}
		writeCanonical(digest, document, part.Value)
	}
	return [32]byte(digest.Sum(nil)), true
}

// writeTagged prefixes every piece with a type and a length, which keeps two
// different structures from concatenating into the same input.
func writeTagged(h hash.Hash, tag byte, data []byte) {
	var header [9]byte
	header[0] = tag
	binary.BigEndian.PutUint64(header[1:], uint64(len(data)))
	h.Write(header[:])
	h.Write(data)
}

// writeCanonical sorts object keys so that spelling differences do not change the
// signature, and keeps number lexemes because floats would merge distinct values.
func writeCanonical(h hash.Hash, document *Document, span Span) {
	switch document.Kind(span) {
	case '{':
		members := document.Members(span)
		slices.SortFunc(members, func(a, b Member) int { return strings.Compare(a.Key, b.Key) })
		writeTagged(h, '{', binary.BigEndian.AppendUint64(nil, uint64(len(members))))
		for _, member := range members {
			writeTagged(h, 'k', []byte(member.Key))
			writeCanonical(h, document, member.Value)
		}
	case '[':
		elements := document.Elements(span)
		writeTagged(h, '[', binary.BigEndian.AppendUint64(nil, uint64(len(elements))))
		for _, element := range elements {
			writeCanonical(h, document, element)
		}
	case '"':
		value, _ := document.String(span)
		writeTagged(h, 's', []byte(value))
	default:
		writeTagged(h, 'v', document.Bytes(span))
	}
}
