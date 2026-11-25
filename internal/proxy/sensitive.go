package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/auth"
	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/sensitivedata"
	"github.com/JoaoVictorVM/comporta/internal/translation"
)

// SensitiveCut describes a request refused by the inspection. Summary is set only
// for a refusal by policy, whose counts are complete.
type SensitiveCut struct {
	At      time.Time
	Status  int
	Code    Code
	Class   ErrorClass
	Summary sensitivedata.Summary
}

// SensitiveRecorder makes inspection metadata durable. It receives summaries only,
// never request content.
type SensitiveRecorder interface {
	PublishSensitive(ctx context.Context, e *Envelope, at time.Time, summary sensitivedata.Summary) error
	RecordSensitiveCut(ctx context.Context, snapshot *config.Snapshot, e *Envelope, cut SensitiveCut) error
}

// SensitiveStage inspects request text after BeforeSend and before the loop guard
// and accounting, so a refusal never reserves budget and the reservation is sized
// from the body that is actually sent. Without a Scanner nothing is inspected.
type SensitiveStage struct {
	Scanner  *sensitivedata.Scanner
	Recorder SensitiveRecorder
}

const (
	rehydrationHeader      = "X-Comporta-Rehydration"
	rehydrationUnsupported = "unsupported-for-stream"
	sensitiveRecordTimeout = time.Second
)

func sensitiveBlocked(message string) *Rejection {
	return &Rejection{Status: http.StatusBadRequest, Code: CodeSensitiveDataBlocked, Class: ClassInvalidRequest, Message: message}
}

func sensitiveScanFailed() *Rejection {
	return &Rejection{Status: http.StatusServiceUnavailable, Code: CodeSensitiveScanFailed, Class: ClassInternal, Message: "sensitive data scan failed"}
}

func sensitiveStorageUnavailable() *Rejection {
	return &Rejection{Status: http.StatusServiceUnavailable, Code: CodeUsageStorageUnavailable, Class: ClassInternal, Message: "usage storage unavailable"}
}

// clientLeft is never written: the client is gone.
func clientLeft() *Rejection {
	return &Rejection{Code: CodeClientDisconnected, Class: ClassClientCancel}
}

func modeOf(m config.DetectionMode) sensitivedata.Mode {
	switch m {
	case config.DetectionDetect:
		return sensitivedata.ModeDetect
	case config.DetectionRedact:
		return sensitivedata.ModeRedact
	case config.DetectionBlock:
		return sensitivedata.ModeBlock
	}
	return sensitivedata.ModeOff
}

// sensitivePolicy reads the detection block of an app from the snapshot the
// request captured; a reload only affects requests that capture the new one.
func sensitivePolicy(d config.Detection) sensitivedata.Policy {
	var p sensitivedata.Policy
	p.Set(sensitivedata.CPF, modeOf(d.CPF))
	p.Set(sensitivedata.CNPJ, modeOf(d.CNPJ))
	p.Set(sensitivedata.Card, modeOf(d.Card))
	p.Set(sensitivedata.Email, modeOf(d.Email))
	p.Set(sensitivedata.Phone, modeOf(d.Phone))
	p.Set(sensitivedata.Pix, modeOf(d.Pix))
	p.Set(sensitivedata.Secret, modeOf(d.Secret))
	p.Rehydrate = d.Rehydrate
	return p
}

// sensitiveState is what the inspection leaves to the response: the reverse map
// of this request alone, when it must be restored.
type sensitiveState struct {
	reverse *sensitivedata.ReverseMap
}

func (s *sensitiveState) release() {
	if s != nil {
		s.reverse.Clear()
		s.reverse = nil
	}
}

func (s *sensitiveState) restoring() *sensitivedata.ReverseMap {
	if s == nil || s.reverse.Len() == 0 {
		return nil
	}
	return s.reverse
}

// inspect runs the scan and applies its decision. A nil rejection means the request
// goes on, possibly with new patches and a summary on the envelope.
func (s *SensitiveStage) inspect(ctx context.Context, snapshot *config.Snapshot, request *prepared, w http.ResponseWriter, logger *slog.Logger, now func() time.Time) (*sensitiveState, *Rejection) {
	if s == nil || s.Scanner == nil {
		return nil, nil
	}
	envelope := request.envelope
	app, ok := snapshot.App(envelope.AppName)
	if !ok {
		failure := internalFailure()
		return nil, &Rejection{Status: failure.Status, Code: failure.Code, Class: failure.Class, Message: failure.Message}
	}
	policy := sensitivePolicy(app.Detection)
	if policy.Rehydrate && envelope.Stream {
		w.Header().Set(rehydrationHeader, rehydrationUnsupported)
	}
	if !policy.Active() {
		return nil, nil
	}
	at := now()
	selection := &contentSelection{document: request.document, format: envelope.APIFormat, responses: envelope.Wire == translation.OpenAIResponses}
	outcome, err := s.Scanner.Scan(ctx, policy, selection.run)
	defer outcome.Release()
	if err != nil {
		if errors.Is(err, sensitivedata.ErrCanceled) {
			return nil, clientLeft()
		}
		if !policy.Protective() {
			logger.Warn("sensitive data scan incomplete; request forwarded unchanged", "request_id", envelope.RequestID)
			request.uncacheable = true
			return nil, nil
		}
		return nil, s.cut(ctx, snapshot, envelope, SensitiveCut{At: at, Status: http.StatusServiceUnavailable, Code: CodeSensitiveScanFailed, Class: ClassInternal}, sensitiveScanFailed())
	}
	summary := outcome.Summary
	if outcome.Blocked != nil {
		rejection := sensitiveBlocked(outcome.Blocked.Message())
		return nil, s.cut(ctx, snapshot, envelope, SensitiveCut{At: at, Status: rejection.Status, Code: rejection.Code, Class: rejection.Class, Summary: summary}, rejection)
	}
	if len(summary) > 0 {
		if rejection := s.publish(ctx, envelope, at, summary, policy, logger); rejection != nil {
			return nil, rejection
		}
		envelope.Sensitive = summary.Clone()
	}
	if !outcome.Redacted() {
		return nil, nil
	}
	patches, err := selection.patches(outcome)
	if err != nil || overlaps(request.patches, patches) {
		return nil, sensitiveScanFailed()
	}
	request.patches = append(request.patches, patches...)
	request.refreshHashes()
	request.uncacheable = true
	state := &sensitiveState{}
	if policy.Rehydrate && !envelope.Stream {
		state.reverse = outcome.Map()
		// The map now belongs to the operation; releasing the outcome must not
		// clear it before the response is restored.
		outcome.DetachMap()
	}
	return state, nil
}

// publish makes the event durable before the request goes on. In detect alone a
// failed write only loses the count; with redact or block the request stops.
func (s *SensitiveStage) publish(ctx context.Context, e *Envelope, at time.Time, summary sensitivedata.Summary, policy sensitivedata.Policy, logger *slog.Logger) *Rejection {
	if s.Recorder == nil {
		return nil
	}
	write, cancel := context.WithTimeout(context.WithoutCancel(ctx), sensitiveRecordTimeout)
	defer cancel()
	if err := s.Recorder.PublishSensitive(write, e, at, summary); err != nil {
		if policy.Protective() {
			return sensitiveStorageUnavailable()
		}
		logger.Warn("sensitive data event not persisted; request forwarded", "request_id", e.RequestID)
	}
	return nil
}

// cut records a refusal before answering it. The gate ticket stays open until this
// returns, so a deletion can never overtake the record.
func (s *SensitiveStage) cut(ctx context.Context, snapshot *config.Snapshot, e *Envelope, cut SensitiveCut, answer *Rejection) *Rejection {
	if s.Recorder == nil {
		return answer
	}
	write, cancel := context.WithTimeout(context.WithoutCancel(ctx), sensitiveRecordTimeout)
	defer cancel()
	if err := s.Recorder.RecordSensitiveCut(write, snapshot, e, cut); err != nil {
		return sensitiveStorageUnavailable()
	}
	return answer
}

func overlaps(existing, added []Patch) bool {
	for _, a := range added {
		for _, b := range existing {
			if a.At.Start < b.At.End && b.At.Start < a.At.End {
				return true
			}
			if b.At.Start == b.At.End && b.At.Start > a.At.Start && b.At.Start < a.At.End {
				return true
			}
		}
	}
	return false
}

// textTarget locates one emitted string: in the request document, or inside the
// JSON carried by a tool argument string.
type textTarget struct {
	span     Span
	embedded int
}

// embeddedJSON is a tool argument string that holds JSON. It is parsed wrapped in a
// one-member object, so any JSON value can be walked with the same Document and
// patched without touching the bytes around the changed strings.
type embeddedJSON struct {
	outer    Span
	document *Document
	patches  []Patch
}

const (
	embeddedPrefix = `{"_":`
	embeddedSuffix = `}`
)

// contentSelection walks only the known content fields of each format. Control
// fields (model, roles, IDs, tool names), property names, numbers and binary blocks
// are never emitted.
type contentSelection struct {
	document *Document
	format   auth.Format
	// responses selects the Responses API body, whose content lives in input
	// and instructions.
	responses bool
	emit      sensitivedata.Emit
	targets   []textTarget
	embedded  []*embeddedJSON
}

func (c *contentSelection) run(emit sensitivedata.Emit) error {
	c.emit = emit
	d := c.document
	for _, member := range d.Members(d.Root()) {
		var err error
		switch {
		case c.responses && member.Key == "input":
			err = c.responsesInput(member.Value)
		case c.responses && member.Key == "instructions":
			err = c.text(d, member.Value, -1)
		case c.responses && member.Key == "tools":
			err = c.functions(member.Value)
		case c.responses:
		case member.Key == "messages":
			err = c.messages(member.Value)
		case member.Key == "system" && c.format == auth.FormatAnthropic:
			err = c.textOrBlocks(d, member.Value, -1)
		case member.Key == "tools":
			err = c.tools(member.Value)
		case member.Key == "functions" && c.format == auth.FormatOpenAI:
			err = c.functions(member.Value)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *contentSelection) text(d *Document, span Span, embedded int) error {
	value, ok := d.String(span)
	if !ok || value == "" {
		return nil
	}
	c.targets = append(c.targets, textTarget{span: span, embedded: embedded})
	return c.emit(value)
}

// values emits every string value below a span, recursively, leaving keys alone.
func (c *contentSelection) values(d *Document, span Span, embedded int) error {
	switch d.Kind(span) {
	case '"':
		return c.text(d, span, embedded)
	case '{':
		for _, member := range d.Members(span) {
			if err := c.values(d, member.Value, embedded); err != nil {
				return err
			}
		}
	case '[':
		for _, element := range d.Elements(span) {
			if err := c.values(d, element, embedded); err != nil {
				return err
			}
		}
	}
	return nil
}

// textOrBlocks reads a string or an array of blocks whose text blocks are content.
func (c *contentSelection) textOrBlocks(d *Document, span Span, embedded int) error {
	switch d.Kind(span) {
	case '"':
		return c.text(d, span, embedded)
	case '[':
		for _, block := range d.Elements(span) {
			if blockType(d, block) != "text" {
				continue
			}
			if text, ok := d.Member(block, "text"); ok {
				if err := c.text(d, text, embedded); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// responsesInput reads a Responses input: a string, or items whose content,
// function call arguments and function outputs are text.
func (c *contentSelection) responsesInput(span Span) error {
	d := c.document
	if d.Kind(span) != '[' {
		return c.text(d, span, -1)
	}
	for _, item := range d.Elements(span) {
		if d.Kind(item) != '{' {
			continue
		}
		var err error
		switch itemType(d, item) {
		case "function_call":
			if arguments, ok := d.Member(item, "arguments"); ok {
				err = c.arguments(arguments)
			}
		case "function_call_output":
			if output, ok := d.Member(item, "output"); ok {
				err = c.responsesParts(output)
			}
		default:
			if content, ok := d.Member(item, "content"); ok {
				err = c.responsesParts(content)
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// responsesParts reads a string or parts whose input_text and output_text
// carry text.
func (c *contentSelection) responsesParts(span Span) error {
	d := c.document
	if d.Kind(span) != '[' {
		return c.text(d, span, -1)
	}
	for _, part := range d.Elements(span) {
		switch blockType(d, part) {
		case "input_text", "output_text":
			if text, ok := d.Member(part, "text"); ok {
				if err := c.text(d, text, -1); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func blockType(d *Document, block Span) string {
	if d.Kind(block) != '{' {
		return ""
	}
	span, ok := d.Member(block, "type")
	if !ok {
		return ""
	}
	kind, _ := d.String(span)
	return kind
}

func (c *contentSelection) messages(span Span) error {
	d := c.document
	if d.Kind(span) != '[' {
		return nil
	}
	for _, message := range d.Elements(span) {
		if d.Kind(message) != '{' {
			continue
		}
		var err error
		if c.format == auth.FormatAnthropic {
			err = c.anthropicMessage(message)
		} else {
			err = c.openAIMessage(message)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *contentSelection) anthropicMessage(message Span) error {
	d := c.document
	content, ok := d.Member(message, "content")
	if !ok {
		return nil
	}
	if d.Kind(content) == '"' {
		return c.text(d, content, -1)
	}
	if d.Kind(content) != '[' {
		return nil
	}
	for _, block := range d.Elements(content) {
		var err error
		switch blockType(d, block) {
		case "text":
			if text, ok := d.Member(block, "text"); ok {
				err = c.text(d, text, -1)
			}
		case "tool_use":
			if input, ok := d.Member(block, "input"); ok {
				err = c.values(d, input, -1)
			}
		case "tool_result":
			if result, ok := d.Member(block, "content"); ok {
				err = c.textOrBlocks(d, result, -1)
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *contentSelection) openAIMessage(message Span) error {
	d := c.document
	for _, member := range d.Members(message) {
		var err error
		switch member.Key {
		case "content":
			err = c.textOrBlocks(d, member.Value, -1)
		case "function_call":
			if d.Kind(member.Value) == '{' {
				if arguments, ok := d.Member(member.Value, "arguments"); ok {
					err = c.arguments(arguments)
				}
			}
		case "tool_calls":
			if d.Kind(member.Value) != '[' {
				continue
			}
			for _, call := range d.Elements(member.Value) {
				if d.Kind(call) != '{' {
					continue
				}
				function, ok := d.Member(call, "function")
				if !ok || d.Kind(function) != '{' {
					continue
				}
				if arguments, found := d.Member(function, "arguments"); found {
					if err = c.arguments(arguments); err != nil {
						return err
					}
				}
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// arguments reads a tool argument string. When it holds JSON, its string values are
// inspected one by one and the JSON is rebuilt valid; otherwise it is plain text.
func (c *contentSelection) arguments(span Span) error {
	d := c.document
	value, ok := d.String(span)
	if !ok {
		return nil
	}
	wrapped := make([]byte, 0, len(embeddedPrefix)+len(value)+len(embeddedSuffix))
	wrapped = append(append(append(wrapped, embeddedPrefix...), value...), embeddedSuffix...)
	if inner, err := ParseDocument(wrapped); err == nil {
		if kind := firstNonSpace(value); kind == '{' || kind == '[' || kind == '"' {
			index := len(c.embedded)
			c.embedded = append(c.embedded, &embeddedJSON{outer: span, document: inner})
			member, _ := inner.Member(inner.Root(), "_")
			return c.values(inner, member, index)
		}
	}
	return c.text(d, span, -1)
}

func firstNonSpace(s string) byte {
	for i := range len(s) {
		switch s[i] {
		case ' ', '\t', '\n', '\r':
			continue
		}
		return s[i]
	}
	return 0
}

func (c *contentSelection) tools(span Span) error {
	d := c.document
	if d.Kind(span) != '[' {
		return nil
	}
	for _, tool := range d.Elements(span) {
		if d.Kind(tool) != '{' {
			continue
		}
		definition := tool
		if c.format == auth.FormatOpenAI {
			function, ok := d.Member(tool, "function")
			if !ok || d.Kind(function) != '{' {
				continue
			}
			definition = function
		}
		if err := c.function(definition); err != nil {
			return err
		}
	}
	return nil
}

func (c *contentSelection) functions(span Span) error {
	d := c.document
	if d.Kind(span) != '[' {
		return nil
	}
	for _, function := range d.Elements(span) {
		if d.Kind(function) != '{' {
			continue
		}
		if err := c.function(function); err != nil {
			return err
		}
	}
	return nil
}

// function reads the description and the schema of one tool definition.
func (c *contentSelection) function(definition Span) error {
	d := c.document
	for _, member := range d.Members(definition) {
		var err error
		switch member.Key {
		case "description":
			err = c.text(d, member.Value, -1)
		case "input_schema", "parameters":
			err = c.schema(member.Value)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// schema reads descriptions and the text of enum, default and examples, without
// ever touching property names or the structure.
func (c *contentSelection) schema(span Span) error {
	d := c.document
	switch d.Kind(span) {
	case '{':
		for _, member := range d.Members(span) {
			var err error
			switch {
			case member.Key == "description" && d.Kind(member.Value) == '"':
				err = c.text(d, member.Value, -1)
			case member.Key == "enum" || member.Key == "default" || member.Key == "examples":
				err = c.values(d, member.Value, -1)
			case d.Kind(member.Value) == '{' || d.Kind(member.Value) == '[':
				err = c.schema(member.Value)
			}
			if err != nil {
				return err
			}
		}
	case '[':
		for _, element := range d.Elements(span) {
			if err := c.schema(element); err != nil {
				return err
			}
		}
	}
	return nil
}

// encodeString writes a JSON string without HTML escaping, which the provider
// would read the same but would change bytes for no reason.
func encodeString(value string) ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}

// patches turns the rewritten strings into disjoint patches over the request
// document. Strings inside a tool argument become one patch of that argument.
func (c *contentSelection) patches(outcome *sensitivedata.Outcome) ([]Patch, error) {
	var patches []Patch
	for i, target := range c.targets {
		text, changed := outcome.Rewritten(i)
		if !changed {
			continue
		}
		encoded, err := encodeString(text)
		if err != nil {
			return nil, err
		}
		patch := Patch{At: target.span, Text: encoded}
		if target.embedded < 0 {
			patches = append(patches, patch)
			continue
		}
		e := c.embedded[target.embedded]
		e.patches = append(e.patches, patch)
	}
	for _, e := range c.embedded {
		if len(e.patches) == 0 {
			continue
		}
		rebuilt := e.document.Apply(e.patches)
		inner := rebuilt[len(embeddedPrefix) : len(rebuilt)-len(embeddedSuffix)]
		encoded, err := encodeString(string(inner))
		if err != nil {
			return nil, err
		}
		patches = append(patches, Patch{At: e.outer, Text: encoded})
	}
	return patches, nil
}
