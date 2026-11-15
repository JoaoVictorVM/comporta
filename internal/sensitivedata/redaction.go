package sensitivedata

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strconv"
	"strings"
)

const (
	nonceBytes     = 12
	nonceAttempts  = 3
	maxTokenPrefix = len("[SECRET_4096")
)

// ReverseMap restores the placeholders of one request. It lives only for the
// operation that built it and offers no representation of its values.
type ReverseMap struct {
	suffix string
	values map[string]string
}

// Restore replaces, in one pass, every complete placeholder of this map. Restored
// values are never scanned again, so a value that looks like a placeholder stays
// literal; unknown, partial or foreign placeholders stay untouched.
func (m *ReverseMap) Restore(text string) (string, bool) {
	if m == nil || len(m.values) == 0 {
		return text, false
	}
	var out strings.Builder
	last, changed := 0, false
	for from := 0; from < len(text); {
		k := strings.Index(text[from:], m.suffix)
		if k < 0 {
			break
		}
		at := from + k
		end := at + len(m.suffix)
		from = end
		lower := max(last, at-maxTokenPrefix)
		open := strings.LastIndexByte(text[lower:at], '[')
		if open < 0 {
			continue
		}
		open += lower
		value, ok := m.values[text[open:end]]
		if !ok {
			continue
		}
		if !changed {
			out.Grow(len(text))
		}
		out.WriteString(text[last:open])
		out.WriteString(value)
		last, changed = end, true
	}
	if !changed {
		return text, false
	}
	out.WriteString(text[last:])
	return out.String(), true
}

// Len is the number of placeholders the map can restore.
func (m *ReverseMap) Len() int {
	if m == nil {
		return 0
	}
	return len(m.values)
}

// Clear drops every reference to the original values. Go may still hold copies
// elsewhere until they are collected; this only ends this map's ownership.
func (m *ReverseMap) Clear() {
	if m == nil {
		return
	}
	clear(m.values)
	m.values = nil
}

const reverseMapText = "sensitivedata.ReverseMap{redacted}"

func (m *ReverseMap) String() string   { return reverseMapText }
func (m *ReverseMap) GoString() string { return reverseMapText }

// Format writes a constant for every verb.
func (m *ReverseMap) Format(state fmt.State, _ rune) { _, _ = io.WriteString(state, reverseMapText) }

// LogValue writes a constant.
func (m *ReverseMap) LogValue() slog.Value { return slog.StringValue(reverseMapText) }

// MarshalJSON writes a constant.
func (m *ReverseMap) MarshalJSON() ([]byte, error) { return []byte(`"[redacted]"`), nil }

// selectSpans keeps, among overlapping redactions of one field, the longest span;
// ties go to the category of highest precedence. The chosen span replaces the whole
// value it covers, which protects the candidates inside it as well.
func selectSpans(candidates []match) []match {
	slices.SortFunc(candidates, func(a, b match) int {
		if d := (b.end - b.start) - (a.end - a.start); d != 0 {
			return d
		}
		if d := precedence[a.category] - precedence[b.category]; d != 0 {
			return d
		}
		return a.start - b.start
	})
	var chosen []match
	for _, c := range candidates {
		i, _ := slices.BinarySearchFunc(chosen, c.start, func(m match, start int) int { return m.start - start })
		if i > 0 && chosen[i-1].end > c.start {
			continue
		}
		if i < len(chosen) && chosen[i].start < c.end {
			continue
		}
		chosen = slices.Insert(chosen, i, c)
	}
	return chosen
}

func (r *run) newNonce(entropy io.Reader) (string, error) {
	var raw [nonceBytes]byte
	for range nonceAttempts {
		if _, err := io.ReadFull(entropy, raw[:]); err != nil {
			return "", errEntropy
		}
		nonce := hex.EncodeToString(raw[:])
		if !slices.ContainsFunc(r.fields, func(field string) bool { return strings.Contains(field, nonce) }) {
			return nonce, nil
		}
	}
	return "", errCollision
}

// redact builds the rewritten fields. Indexes start at 1 per category, follow field
// and position order, and repeat for an identical value of the same category.
func (r *run) redact(entropy io.Reader) (map[int]string, *ReverseMap, error) {
	perField := map[int][]match{}
	for _, m := range r.matches {
		if r.policy.Mode(m.category) == ModeRedact {
			perField[m.field] = append(perField[m.field], m)
		}
	}
	if len(perField) == 0 {
		return nil, nil, nil
	}
	nonce, err := r.newNonce(entropy)
	if err != nil {
		return nil, nil, err
	}
	type valueKey struct {
		category Category
		value    string
	}
	tokens := map[valueKey]string{}
	defer clear(tokens)
	var counters [categoryCount]int
	retained := 0
	var reverse *ReverseMap
	if r.policy.Rehydrate {
		reverse = &ReverseMap{suffix: "_" + nonce + "]", values: map[string]string{}}
	}
	rewritten := make(map[int]string, len(perField))
	for field := range len(r.fields) {
		candidates, ok := perField[field]
		if !ok {
			continue
		}
		text := r.fields[field]
		var out bytes.Buffer
		last := 0
		for _, m := range selectSpans(candidates) {
			value := text[m.start:m.end]
			key := valueKey{m.category, value}
			token, seen := tokens[key]
			if !seen {
				retained += len(value)
				if retained > r.limits.MaxMapBytes {
					reverse.Clear()
					return nil, nil, errCapacity
				}
				counters[m.category]++
				token = "[" + strings.ToUpper(m.category.String()) + "_" + strconv.Itoa(counters[m.category]) + "_" + nonce + "]"
				tokens[key] = token
				if reverse != nil {
					reverse.values[token] = value
				}
			}
			out.WriteString(text[last:m.start])
			out.WriteString(token)
			last = m.end
		}
		out.WriteString(text[last:])
		rewritten[field] = out.String()
	}
	return rewritten, reverse, nil
}
