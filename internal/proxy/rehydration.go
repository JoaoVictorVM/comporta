package proxy

import (
	"errors"
	"net/http"

	"github.com/JoaoVictorVM/comporta/internal/auth"
	"github.com/JoaoVictorVM/comporta/internal/sensitivedata"
)

var errRehydration = errors.New("response rehydration failed")

func rehydrationFailure() Failure {
	return Failure{http.StatusBadGateway, CodeRehydrationFailed, ClassProtocol, "response rehydration failed"}
}

// rehydrator restores the placeholders of one request in the known text fields of
// a successful JSON response. Usage, IDs, tool names, the model, finish reasons and
// unknown fields are never read, and the raw JSON is never searched.
type rehydrator struct {
	reverse *sensitivedata.ReverseMap
	patches []Patch
}

// rehydrateResponse returns the body with the restored values, or the original
// slice when nothing changed. The result is bounded by the same limit as any
// response the gateway buffers.
func rehydrateResponse(format auth.Format, body []byte, reverse *sensitivedata.ReverseMap, limit int) ([]byte, error) {
	document, err := ParseDocument(body)
	if err != nil {
		return nil, errRehydration
	}
	r := &rehydrator{reverse: reverse}
	if format == auth.FormatAnthropic {
		err = r.anthropic(document)
	} else {
		err = r.openAI(document)
	}
	if err != nil {
		return nil, errRehydration
	}
	if len(r.patches) == 0 {
		return body, nil
	}
	size := len(body)
	for _, patch := range r.patches {
		size += len(patch.Text) - (patch.At.End - patch.At.Start)
		if size > limit {
			return nil, errRehydration
		}
	}
	return document.Apply(r.patches), nil
}

func (r *rehydrator) text(d *Document, span Span, out *[]Patch) error {
	value, ok := d.String(span)
	if !ok {
		return nil
	}
	restored, changed := r.reverse.Restore(value)
	if !changed {
		return nil
	}
	encoded, err := encodeString(restored)
	if err != nil {
		return err
	}
	*out = append(*out, Patch{At: span, Text: encoded})
	return nil
}

func (r *rehydrator) values(d *Document, span Span, out *[]Patch) error {
	switch d.Kind(span) {
	case '"':
		return r.text(d, span, out)
	case '{':
		for _, member := range d.Members(span) {
			if err := r.values(d, member.Value, out); err != nil {
				return err
			}
		}
	case '[':
		for _, element := range d.Elements(span) {
			if err := r.values(d, element, out); err != nil {
				return err
			}
		}
	}
	return nil
}

// arguments restores a tool argument string. JSON arguments are restored string by
// string, so a value with quotes or line breaks keeps the inner JSON valid.
func (r *rehydrator) arguments(d *Document, span Span) error {
	value, ok := d.String(span)
	if !ok {
		return nil
	}
	if kind := firstNonSpace(value); kind == '{' || kind == '[' || kind == '"' {
		wrapped := embeddedPrefix + value + embeddedSuffix
		if inner, err := ParseDocument([]byte(wrapped)); err == nil {
			member, _ := inner.Member(inner.Root(), "_")
			var patches []Patch
			if err := r.values(inner, member, &patches); err != nil {
				return err
			}
			if len(patches) == 0 {
				return nil
			}
			rebuilt := inner.Apply(patches)
			encoded, err := encodeString(string(rebuilt[len(embeddedPrefix) : len(rebuilt)-len(embeddedSuffix)]))
			if err != nil {
				return err
			}
			r.patches = append(r.patches, Patch{At: span, Text: encoded})
			return nil
		}
	}
	return r.text(d, span, &r.patches)
}

func (r *rehydrator) anthropic(d *Document) error {
	content, ok := d.Path("content")
	if !ok || d.Kind(content) != '[' {
		return nil
	}
	for _, block := range d.Elements(content) {
		var err error
		switch blockType(d, block) {
		case "text":
			if text, found := d.Member(block, "text"); found {
				err = r.text(d, text, &r.patches)
			}
		case "tool_use":
			if input, found := d.Member(block, "input"); found {
				err = r.values(d, input, &r.patches)
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (r *rehydrator) openAI(d *Document) error {
	choices, ok := d.Path("choices")
	if !ok || d.Kind(choices) != '[' {
		return nil
	}
	for _, choice := range d.Elements(choices) {
		if d.Kind(choice) != '{' {
			continue
		}
		message, found := d.Member(choice, "message")
		if !found || d.Kind(message) != '{' {
			continue
		}
		if err := r.openAIMessage(d, message); err != nil {
			return err
		}
	}
	return nil
}

func (r *rehydrator) openAIMessage(d *Document, message Span) error {
	for _, member := range d.Members(message) {
		switch member.Key {
		case "content":
			if d.Kind(member.Value) == '"' {
				if err := r.text(d, member.Value, &r.patches); err != nil {
					return err
				}
				continue
			}
			if d.Kind(member.Value) != '[' {
				continue
			}
			for _, part := range d.Elements(member.Value) {
				if blockType(d, part) != "text" {
					continue
				}
				if text, found := d.Member(part, "text"); found {
					if err := r.text(d, text, &r.patches); err != nil {
						return err
					}
				}
			}
		case "function_call":
			if d.Kind(member.Value) != '{' {
				continue
			}
			if arguments, found := d.Member(member.Value, "arguments"); found {
				if err := r.arguments(d, arguments); err != nil {
					return err
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
				function, found := d.Member(call, "function")
				if !found || d.Kind(function) != '{' {
					continue
				}
				if arguments, has := d.Member(function, "arguments"); has {
					if err := r.arguments(d, arguments); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}
