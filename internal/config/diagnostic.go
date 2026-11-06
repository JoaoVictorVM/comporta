package config

import (
	"cmp"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Code is a stable, machine-readable diagnostic category.
type Code string

const (
	CodeLiteralSecret        Code = "literal_secret"
	CodeMissingEnvironment   Code = "missing_environment"
	CodeSecretFileUnreadable Code = "secret_file_unreadable"
	CodeInvalidPolicy        Code = "invalid_policy"
	CodeRestartRequired      Code = "restart_required"
	CodeConfigMissing        Code = "config_missing"
	CodeConfigUnreadable     Code = "config_unreadable"
	CodeConfigTooLarge       Code = "config_too_large"
	CodeUnknownField         Code = "unknown_field"
	CodeInvalidYAML          Code = "invalid_yaml"
	CodeInvalidReference     Code = "invalid_reference"
	CodeInvalidValue         Code = "invalid_value"
	CodeInvalidEnvironment   Code = "invalid_environment"
	CodePermissionWarning    Code = "permission_warning"
	CodeFallbackMixedFormat  Code = "fallback_mixed_format"
	CodeDiagnosticsTruncated Code = "diagnostics_truncated"
)

const maxDiagnostics = 100

// Diagnostic describes one validation problem. It never carries configuration
// values or raw parser output: Path escapes untrusted names and Message comes
// from a fixed vocabulary. Line and Column are 1-based, and 0 means unavailable.
type Diagnostic struct {
	Code    Code
	Path    string
	Line    int
	Column  int
	Message string
}

// MarshalJSON renders unavailable positions as null.
func (d Diagnostic) MarshalJSON() ([]byte, error) {
	out := struct {
		Code    Code   `json:"code"`
		Path    string `json:"path"`
		Line    *int   `json:"line"`
		Column  *int   `json:"column"`
		Message string `json:"message"`
	}{Code: d.Code, Path: d.Path, Message: d.Message}
	if d.Line > 0 {
		out.Line = &d.Line
		out.Column = &d.Column
	}
	return json.Marshal(out)
}

// String renders the documented operator-facing text of the diagnostic.
func (d Diagnostic) String() string {
	switch {
	case d.Path == "" || d.Code == CodeSecretFileUnreadable:
		return d.Message
	case d.Code == CodeLiteralSecret:
		return d.Path + " " + d.Message
	default:
		return d.Path + ": " + d.Message
	}
}

// Error aggregates the diagnostics that made a configuration unusable.
type Error struct {
	Diagnostics []Diagnostic
}

func (e *Error) Error() string {
	lines := make([]string, len(e.Diagnostics))
	for i, diagnostic := range e.Diagnostics {
		lines[i] = diagnostic.String()
	}
	return strings.Join(lines, "; ")
}

// diagnosticList collects problems and produces a reproducible, bounded result.
type diagnosticList struct {
	items []Diagnostic
}

func (l *diagnosticList) add(d Diagnostic) { l.items = append(l.items, d) }

func (l *diagnosticList) empty() bool { return len(l.items) == 0 }

func (l *diagnosticList) sorted() []Diagnostic {
	items := slices.Clone(l.items)
	slices.SortStableFunc(items, func(a, b Diagnostic) int {
		return cmp.Or(
			cmp.Compare(positionRank(a.Line), positionRank(b.Line)),
			cmp.Compare(a.Column, b.Column),
			cmp.Compare(a.Path, b.Path),
			cmp.Compare(a.Code, b.Code),
		)
	})
	items = slices.CompactFunc(items, func(a, b Diagnostic) bool { return a == b })
	if len(items) > maxDiagnostics {
		omitted := len(items) - maxDiagnostics
		items = append(items[:maxDiagnostics:maxDiagnostics], Diagnostic{
			Code:    CodeDiagnosticsTruncated,
			Message: fmt.Sprintf("%d additional diagnostics omitted", omitted),
		})
	}
	return items
}

// positionRank orders diagnostics without a position after positioned ones.
func positionRank(line int) int {
	if line == 0 {
		return int(^uint(0) >> 1)
	}
	return line
}

func (l *diagnosticList) err() error {
	if l.empty() {
		return nil
	}
	return &Error{Diagnostics: l.sorted()}
}

var plainPathSegment = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

const maxPathNameBytes = 256

// joinPath appends a map key to a dotted path. Names outside the plain alphabet
// are quoted so that dots, newlines and control characters in untrusted keys can
// neither forge another path nor inject log lines.
func joinPath(parent, key string) string {
	if plainPathSegment.MatchString(key) {
		if parent == "" {
			return key
		}
		return parent + "." + key
	}
	return parent + "[" + quoteText(key) + "]"
}

func indexPath(parent string, index int) string {
	return parent + "[" + strconv.Itoa(index) + "]"
}

// quoteText escapes untrusted text for diagnostics, bounding its size.
func quoteText(text string) string {
	if len(text) > maxPathNameBytes {
		text = strings.ToValidUTF8(text[:maxPathNameBytes], "") + "..."
	}
	return strconv.QuoteToASCII(text)
}
