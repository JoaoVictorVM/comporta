package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"unicode/utf8"
)

const maxJSONDepth = 128

// Span is a half-open byte range of the original document.
type Span struct {
	Start, End int
}

// Member is one key of an object, with its value located in the original bytes.
type Member struct {
	Key   string
	Value Span
}

// Document is a validated JSON object indexed over its original bytes. It is never
// deserialized and serialized again: forwarding must keep every byte the gateway
// has no reason to change, including escapes, spacing and number spelling.
type Document struct {
	data []byte
	root Span
}

// syntaxError carries only an offset, so reporting it cannot echo the body.
type syntaxError struct {
	offset int64
}

func (e *syntaxError) Error() string { return fmt.Sprintf("invalid JSON at byte %d", e.offset) }

var (
	errNotObject = errors.New("request body must be a JSON object")
	errTooDeep   = errors.New("JSON nesting exceeds 128 levels")
	errDuplicate = errors.New("JSON contains duplicate keys")
	errBadUTF8   = errors.New("JSON contains invalid UTF-8")
)

// ParseDocument validates the whole document. Duplicate keys are refused at every
// level, even inside unknown objects, because the gateway and the provider could
// otherwise read different values from the same bytes.
func ParseDocument(data []byte) (*Document, error) {
	var raw json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) {
			return nil, &syntaxError{offset: syntax.Offset}
		}
		return nil, &syntaxError{offset: int64(len(data))}
	}
	if !utf8.Valid(data) {
		return nil, errBadUTF8
	}
	start := skipSpace(data, 0)
	if data[start] != '{' {
		return nil, errNotObject
	}
	end, err := checkValue(data, start, 1)
	if err != nil {
		return nil, err
	}
	return &Document{data: data, root: Span{start, end}}, nil
}

// Root is the span of the top-level object.
func (d *Document) Root() Span { return d.root }

// Bytes returns the original bytes of a span.
func (d *Document) Bytes(span Span) []byte { return d.data[span.Start:span.End] }

// Kind returns the first byte of a value, which identifies its JSON type.
func (d *Document) Kind(span Span) byte { return d.data[span.Start] }

// Members lists the members of an object span in document order.
func (d *Document) Members(span Span) []Member {
	var members []Member
	i := skipSpace(d.data, span.Start+1)
	for d.data[i] != '}' {
		keyEnd := skipString(d.data, i)
		key := decodeString(d.data[i:keyEnd])
		i = skipSpace(d.data, keyEnd)
		i = skipSpace(d.data, i+1)
		valueEnd := skipValue(d.data, i)
		members = append(members, Member{Key: key, Value: Span{i, valueEnd}})
		i = skipSpace(d.data, valueEnd)
		if d.data[i] == ',' {
			i = skipSpace(d.data, i+1)
		}
	}
	return members
}

// Elements lists the values of an array span.
func (d *Document) Elements(span Span) []Span {
	var elements []Span
	i := skipSpace(d.data, span.Start+1)
	for d.data[i] != ']' {
		end := skipValue(d.data, i)
		elements = append(elements, Span{i, end})
		i = skipSpace(d.data, end)
		if d.data[i] == ',' {
			i = skipSpace(d.data, i+1)
		}
	}
	return elements
}

// Member finds a key directly inside an object span. Keys are compared decoded, so
// an escaped spelling of a key is the same key.
func (d *Document) Member(object Span, key string) (Span, bool) {
	for _, member := range d.Members(object) {
		if member.Key == key {
			return member.Value, true
		}
	}
	return Span{}, false
}

// Path walks nested objects from the root.
func (d *Document) Path(keys ...string) (Span, bool) {
	current := d.root
	for _, key := range keys {
		if d.Kind(current) != '{' {
			return Span{}, false
		}
		next, ok := d.Member(current, key)
		if !ok {
			return Span{}, false
		}
		current = next
	}
	return current, true
}

// String decodes a string value.
func (d *Document) String(span Span) (string, bool) {
	if d.Kind(span) != '"' {
		return "", false
	}
	return decodeString(d.Bytes(span)), true
}

// Patch replaces a byte range. An insertion is a patch with an empty range.
type Patch struct {
	At   Span
	Text []byte
	// transportOnly marks a change that does not alter what the client asked for. It
	// is a property of the patch and not of its position, because two insertions at
	// the end of the same object share a position.
	transportOnly bool
}

// SetMember builds the patch that sets a key of an object: it replaces only the
// value when the key exists, or inserts a new member otherwise, leaving every other
// byte of the object untouched.
func (d *Document) SetMember(object Span, key string, value []byte) (Patch, error) {
	if d.Kind(object) != '{' {
		return Patch{}, errNotObject
	}
	members := d.Members(object)
	for _, member := range members {
		if member.Key == key {
			return Patch{At: member.Value, Text: value}, nil
		}
	}
	encodedKey, err := json.Marshal(key)
	if err != nil {
		return Patch{}, err
	}
	text := append(append(slices.Clone(encodedKey), ':'), value...)
	if len(members) == 0 {
		return Patch{At: Span{object.Start + 1, object.Start + 1}, Text: text}, nil
	}
	end := members[len(members)-1].Value.End
	return Patch{At: Span{end, end}, Text: append([]byte{','}, text...)}, nil
}

// Apply returns the document with the patches applied. Patches must not overlap;
// insertions at the same offset keep the order given. With no patches it returns
// the original slice, so an untouched body is forwarded as is.
func (d *Document) Apply(patches []Patch) []byte {
	if len(patches) == 0 {
		return d.data
	}
	ordered := slices.Clone(patches)
	slices.SortStableFunc(ordered, func(a, b Patch) int { return a.At.Start - b.At.Start })
	size := len(d.data)
	for _, patch := range ordered {
		size += len(patch.Text) - (patch.At.End - patch.At.Start)
	}
	out := make([]byte, 0, size)
	position := 0
	for _, patch := range ordered {
		out = append(out, d.data[position:patch.At.Start]...)
		out = append(out, patch.Text...)
		position = patch.At.End
	}
	return append(out, d.data[position:]...)
}

// The helpers below walk bytes json.Unmarshal already validated, so they do not
// repeat syntax checks.

func skipSpace(data []byte, i int) int {
	for i < len(data) && (data[i] == ' ' || data[i] == '\t' || data[i] == '\n' || data[i] == '\r') {
		i++
	}
	return i
}

func skipString(data []byte, i int) int {
	i++
	for data[i] != '"' {
		if data[i] == '\\' {
			i++
		}
		i++
	}
	return i + 1
}

func skipValue(data []byte, i int) int {
	switch data[i] {
	case '"':
		return skipString(data, i)
	case '{', '[':
		depth := 0
		for {
			switch data[i] {
			case '"':
				i = skipString(data, i)
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
			i++
		}
	}
	for i < len(data) && !isValueEnd(data[i]) {
		i++
	}
	return i
}

func isValueEnd(c byte) bool {
	return c == ',' || c == '}' || c == ']' || c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

func decodeString(raw []byte) string {
	var decoded string
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return ""
	}
	return decoded
}

func checkValue(data []byte, i, depth int) (int, error) {
	switch data[i] {
	case '{':
		return checkObject(data, i, depth)
	case '[':
		return checkArray(data, i, depth)
	}
	return skipValue(data, i), nil
}

func checkObject(data []byte, i, depth int) (int, error) {
	if depth > maxJSONDepth {
		return 0, errTooDeep
	}
	var seen map[string]struct{}
	i = skipSpace(data, i+1)
	for data[i] != '}' {
		keyEnd := skipString(data, i)
		key := decodeString(data[i:keyEnd])
		if _, duplicate := seen[key]; duplicate {
			return 0, errDuplicate
		}
		if seen == nil {
			seen = make(map[string]struct{})
		}
		seen[key] = struct{}{}
		i = skipSpace(data, keyEnd)
		i = skipSpace(data, i+1)
		end, err := checkValue(data, i, depth+1)
		if err != nil {
			return 0, err
		}
		i = skipSpace(data, end)
		if data[i] == ',' {
			i = skipSpace(data, i+1)
		}
	}
	return i + 1, nil
}

func checkArray(data []byte, i, depth int) (int, error) {
	if depth > maxJSONDepth {
		return 0, errTooDeep
	}
	i = skipSpace(data, i+1)
	for data[i] != ']' {
		end, err := checkValue(data, i, depth+1)
		if err != nil {
			return 0, err
		}
		i = skipSpace(data, end)
		if data[i] == ',' {
			i = skipSpace(data, i+1)
		}
	}
	return i + 1, nil
}

// jsonDepthExceeds reports whether a valid document nests deeper than the limit,
// for payloads that are only inspected and need no key checks.
func jsonDepthExceeds(data []byte, limit int) bool {
	depth := 0
	for i := 0; i < len(data); i++ {
		switch data[i] {
		case '"':
			i = skipString(data, i) - 1
		case '{', '[':
			depth++
			if depth > limit {
				return true
			}
		case '}', ']':
			depth--
		}
	}
	return false
}
