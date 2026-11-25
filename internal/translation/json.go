package translation

import (
	"bytes"
	"encoding/json"
	"math"
	"slices"
	"strconv"
)

// object is a decoded JSON object whose values stay raw until a reader asks
// for them, so unknown members are noticed without being interpreted.
type object map[string]json.RawMessage

func decodeObject(raw json.RawMessage) (object, bool) {
	if len(raw) == 0 || raw[0] != '{' {
		return nil, false
	}
	var out object
	if json.Unmarshal(raw, &out) != nil {
		return nil, false
	}
	return out, true
}

func decodeArray(raw json.RawMessage) ([]json.RawMessage, bool) {
	if len(raw) == 0 || raw[0] != '[' {
		return nil, false
	}
	var out []json.RawMessage
	if json.Unmarshal(raw, &out) != nil {
		return nil, false
	}
	return out, true
}

// keys returns the member names in a stable order, so the first incompatible
// feature found is the same for the same body.
func (o object) keys() []string {
	out := make([]string, 0, len(o))
	for key := range o {
		out = append(out, key)
	}
	slices.Sort(out)
	return out
}

func isNull(raw json.RawMessage) bool { return string(bytes.TrimSpace(raw)) == "null" }

// present returns a member that exists and, when nullable, is not null.
func (o object) present(key string, nullable bool) (json.RawMessage, bool) {
	raw, ok := o[key]
	if !ok || (nullable && isNull(raw)) {
		return nil, false
	}
	return raw, true
}

func asString(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 || raw[0] != '"' {
		return "", false
	}
	var out string
	return out, json.Unmarshal(raw, &out) == nil
}

func (o object) str(key string) (string, bool) {
	raw, ok := o[key]
	if !ok {
		return "", false
	}
	return asString(raw)
}

func asBool(raw json.RawMessage) (bool, bool) {
	switch string(bytes.TrimSpace(raw)) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}

func asPositive(raw json.RawMessage) (int64, bool) {
	value, err := strconv.ParseInt(string(bytes.TrimSpace(raw)), 10, 64)
	return value, err == nil && value > 0
}

// asNumber keeps the lexeme of a finite number.
func asNumber(raw json.RawMessage) (string, bool) {
	lexeme := string(bytes.TrimSpace(raw))
	value, err := strconv.ParseFloat(lexeme, 64)
	if err != nil || math.IsInf(value, 0) || math.IsNaN(value) {
		return "", false
	}
	return lexeme, true
}

func asStrings(raw json.RawMessage) ([]string, bool) {
	if len(raw) == 0 || raw[0] != '[' {
		return nil, false
	}
	var out []string
	return out, json.Unmarshal(raw, &out) == nil
}

// isObject reports whether raw holds a JSON object.
func isObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{' && json.Valid(trimmed)
}

// marshal encodes without HTML escaping, which would change prompt bytes for no
// reason.
func marshal(value any) ([]byte, error) {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(out.Bytes(), []byte("\n")), nil
}

// number turns a kept lexeme back into a JSON number without reformatting it.
func number(lexeme string) json.RawMessage {
	if lexeme == "" {
		return nil
	}
	return json.RawMessage(lexeme)
}

// compactObject validates tool arguments and returns them compact. An empty
// string is an empty object, as providers send it for calls without
// arguments.
func compactObject(text string) (json.RawMessage, bool) {
	if len(bytes.TrimSpace([]byte(text))) == 0 {
		return json.RawMessage(`{}`), true
	}
	if !isObject(json.RawMessage(text)) {
		return nil, false
	}
	var out bytes.Buffer
	if json.Compact(&out, []byte(text)) != nil {
		return nil, false
	}
	return out.Bytes(), true
}

// compactRaw removes insignificant whitespace from valid JSON and keeps member
// order, so the same schema renders to the same bytes in every protocol.
func compactRaw(raw json.RawMessage) json.RawMessage {
	var out bytes.Buffer
	if json.Compact(&out, raw) != nil {
		return raw
	}
	return out.Bytes()
}
