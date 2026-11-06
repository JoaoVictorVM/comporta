package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const baseYAML = `timezone: UTC
providers:
  anthropic:
    api_format: anthropic
    base_url: https://api.anthropic.com
    api_key: ${ANTHROPIC_API_KEY}
    models: [model-a, model-b]
  local:
    api_format: openai
    base_url: http://localhost:11434
    models: [local/model]
pricing:
  model-b: {input: 1.00, output: 5.00, cache_write: 1.25, cache_read: 0.10}
apps:
  bot:
    allowed_models: [model-a, model-b]
    budget: {usd_per_day: 2.00}
`

const testConfigPath = "/virtual/comporta.yaml"

func structuralOptions() Options { return Options{SystemTimezone: "UTC"} }

func loadText(t *testing.T, text string, opts Options) (*Candidate, []Diagnostic) {
	t.Helper()
	candidate, err := LoadBytes([]byte(text), testConfigPath, opts)
	if err == nil {
		return candidate, nil
	}
	var failure *Error
	if !errors.As(err, &failure) {
		t.Fatalf("load returned a non-diagnostic error: %v", err)
	}
	return nil, failure.Diagnostics
}

func hasDiagnostic(diags []Diagnostic, code Code, path string) bool {
	for _, diagnostic := range diags {
		if diagnostic.Code == code && (path == "" || diagnostic.Path == path) {
			return true
		}
	}
	return false
}

func requireDiagnostic(t *testing.T, text string, code Code, path string) []Diagnostic {
	t.Helper()
	_, diags := loadText(t, text, structuralOptions())
	if !hasDiagnostic(diags, code, path) {
		t.Fatalf("expected %s at %q, got %+v", code, path, diags)
	}
	return diags
}

func requireValid(t *testing.T, text string) *Candidate {
	t.Helper()
	candidate, diags := loadText(t, text, structuralOptions())
	if candidate == nil {
		t.Fatalf("expected a valid configuration, got %+v", diags)
	}
	return candidate
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// replaced fails the test when the edit does not apply, so a fixture change can
// never silently turn a negative test into a positive one.
func replaced(t *testing.T, text, old, new string) string {
	t.Helper()
	if !strings.Contains(text, old) {
		t.Fatalf("fixture does not contain %q", old)
	}
	return strings.Replace(text, old, new, 1)
}
