package config

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadSizeBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "comporta.yaml")
	padded := func(size int) []byte {
		data := bytes.Repeat([]byte("#"), size)
		copy(data, baseYAML)
		data[len(baseYAML)] = '\n'
		return data
	}
	if err := os.WriteFile(path, padded(MaxConfigBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path, structuralOptions()); err != nil {
		t.Fatalf("file at the exact limit must load: %v", err)
	}
	if err := os.WriteFile(path, padded(MaxConfigBytes+1), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path, structuralOptions())
	if err == nil || !strings.Contains(err.Error(), "configuration exceeds 1000000 byte limit") {
		t.Fatalf("oversized file must be rejected with the documented message: %v", err)
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "absent.yaml"), structuralOptions())
	failure, ok := err.(*Error)
	if !ok || !hasDiagnostic(failure.Diagnostics, CodeConfigMissing, "") {
		t.Fatalf("expected config_missing, got %v", err)
	}
}

func TestRejectDuplicateAndUnknownKeys(t *testing.T) {
	requireDiagnostic(t, baseYAML+"timezone: UTC\n", CodeInvalidYAML, "timezone")
	requireDiagnostic(t, baseYAML+"surprise: 1\n", CodeUnknownField, "surprise")
	requireDiagnostic(t, replaced(t, baseYAML, "  bot:\n", "  bot:\n    surprise: 1\n"), CodeUnknownField, "apps.bot.surprise")
	requireDiagnostic(t, baseYAML+"123: 1\n", CodeInvalidYAML, "")
}

func TestRejectAliasesAndMultipleDocuments(t *testing.T) {
	cases := map[string]string{
		"anchor and alias": baseYAML + "listeners:\n  proxy: &p {port: 9000}\n  admin: *p\n",
		"merge key":        baseYAML + "listeners:\n  proxy:\n    <<: {port: 9000}\n",
		"multiple docs":    baseYAML + "---\ntimezone: UTC\n",
		"custom tag":       replaced(t, baseYAML, "timezone: UTC", "timezone: !custom UTC"),
		"explicit tag":     replaced(t, baseYAML, "timezone: UTC", "timezone: !!str UTC"),
		"syntax error":     baseYAML + "listeners: [\n",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			requireDiagnostic(t, text, CodeInvalidYAML, "")
		})
	}
}

func TestRejectExcessiveNesting(t *testing.T) {
	var builder strings.Builder
	builder.WriteString(baseYAML + "deep: ")
	for range 100 {
		builder.WriteString("[")
	}
	for range 100 {
		builder.WriteString("]")
	}
	builder.WriteString("\n")
	requireDiagnostic(t, builder.String(), CodeInvalidYAML, "")
}

func TestNullRejected(t *testing.T) {
	requireDiagnostic(t, replaced(t, baseYAML, "timezone: UTC", "timezone: null"), CodeInvalidValue, "timezone")
	requireDiagnostic(t, replaced(t, baseYAML, "timezone: UTC", "timezone:"), CodeInvalidValue, "timezone")
}

func TestDiagnosticDoesNotEchoYAML(t *testing.T) {
	const canary = "canary-value-7f3a"
	cases := []string{
		"timezone: UTC\nproviders: [" + canary + "\n",
		"timezone: " + canary + "\nproviders: {}\n",
		baseYAML + "fail_mode: " + canary + "\n",
		baseYAML + "unknown: " + canary + "\n",
		replaced(t, baseYAML, "budget: {usd_per_day: 2.00}", "budget: {usd_per_day: "+canary+"}"),
		replaced(t, baseYAML, "api_key: ${ANTHROPIC_API_KEY}", "api_key: "+canary),
		replaced(t, baseYAML, "base_url: https://api.anthropic.com", "base_url: https://"+canary+":x@host"),
		"\"" + canary,
	}
	for i, text := range cases {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			_, diags := loadText(t, text, structuralOptions())
			if len(diags) == 0 {
				t.Fatal("fixture must be invalid")
			}
			for _, diagnostic := range diags {
				if strings.Contains(diagnostic.String(), canary) {
					t.Fatalf("diagnostic echoes input: %s", diagnostic)
				}
			}
		})
	}
}

func TestPathsEscapeUntrustedKeys(t *testing.T) {
	_, diags := loadText(t, "\"evil\\nkey.with.dots\": 1\n"+baseYAML, structuralOptions())
	for _, diagnostic := range diags {
		if strings.ContainsAny(diagnostic.String(), "\n\r") {
			t.Fatalf("diagnostic allows log injection: %q", diagnostic)
		}
	}
	if !hasDiagnostic(diags, CodeUnknownField, `["evil\nkey.with.dots"]`) {
		t.Fatalf("expected a quoted path, got %+v", diags)
	}
}

func TestDiagnosticsAreBoundedAndOrdered(t *testing.T) {
	var builder strings.Builder
	builder.WriteString(baseYAML)
	for i := range 150 {
		fmt.Fprintf(&builder, "unknown-%03d: 1\n", i)
	}
	_, diags := loadText(t, builder.String(), structuralOptions())
	if len(diags) != maxDiagnostics+1 || diags[maxDiagnostics].Code != CodeDiagnosticsTruncated {
		t.Fatalf("expected a truncated list, got %d entries", len(diags))
	}
	for i := 1; i < maxDiagnostics; i++ {
		if diags[i-1].Line > diags[i].Line {
			t.Fatalf("diagnostics out of order at %d", i)
		}
	}
}

func TestDiagnosticJSONUsesNullForUnknownPositions(t *testing.T) {
	data, err := Diagnostic{Code: CodeConfigMissing, Message: "gone"}.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"line":null`) || !strings.Contains(string(data), `"column":null`) {
		t.Fatalf("unexpected JSON: %s", data)
	}
}

func TestDefaultsApplied(t *testing.T) {
	cfg := requireValid(t, baseYAML).cfg
	app := cfg.Apps["bot"]
	switch {
	case cfg.Listeners.Proxy != Listener{Bind: "127.0.0.1", Port: 8080}:
		t.Errorf("proxy default: %+v", cfg.Listeners.Proxy)
	case cfg.Listeners.Admin != Listener{Bind: "127.0.0.1", Port: 8081}:
		t.Errorf("admin default: %+v", cfg.Listeners.Admin)
	case cfg.FailMode != FailClosed || app.FailMode != FailClosed:
		t.Errorf("fail mode default: %s %s", cfg.FailMode, app.FailMode)
	case app.Timezone != "UTC":
		t.Errorf("app timezone inherits the root: %s", app.Timezone)
	case app.Limits.MaxConcurrent != 20 || app.PerUser.Limits.MaxConcurrent != 4:
		t.Errorf("concurrency defaults: %+v", app)
	case app.MaxTokens != 4096 || app.OnLimit != OnLimitBlock:
		t.Errorf("app defaults: %+v", app)
	case !app.LoopGuard.Enabled || app.Cache.TTLSeconds != 600 || app.Detection.CPF != DetectionOff:
		t.Errorf("mechanism defaults: %+v", app)
	case cfg.Providers["anthropic"].TLSMinVersion != "1.2":
		t.Errorf("tls default: %+v", cfg.Providers["anthropic"])
	case !filepath.IsAbs(cfg.DataDir) && filepath.Base(cfg.DataDir) != "data":
		t.Errorf("data dir default: %s", cfg.DataDir)
	}
}

func TestCandidateStructuralQueries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "comporta.yaml")
	text := `timezone: UTC
data_dir: ./state
providers:
  anthropic:
    api_format: anthropic
    base_url: https://api.anthropic.com
    api_key: ${COMPORTA_UNSET_FOR_THIS_TEST}
    models: [model-a]
apps:
  zeta:
    allowed_models: [model-a]
  alpha:
    allowed_models: [model-a]
`
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, err := Load(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := candidate.DataDir(), filepath.Join(filepath.Dir(path), "state"); got != want {
		t.Fatalf("data dir %q, want %q", got, want)
	}
	if got := candidate.AppNames(); len(got) != 2 || got[0] != "alpha" || got[1] != "zeta" {
		t.Fatalf("apps %v", got)
	}
	if !candidate.HasApp("alpha") || candidate.HasApp("Alpha") || candidate.HasApp("missing") {
		t.Fatal("app lookup must be exact")
	}
}
