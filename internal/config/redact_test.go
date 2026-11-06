package config

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

const (
	envCanary    = "canary-env-secret"
	fileCanary   = "canary-file-secret"
	hookCanary   = "canary-hook-token"
	signCanary   = "canary-signing-secret"
	tokenCanary  = "canary-ntfy-token"
	secretCanary = "canary-"
)

const everySecretYAML = `timezone: UTC
providers:
  anthropic:
    api_format: anthropic
    base_url: https://api.anthropic.com
    api_key: ${ENV_KEY}
    models: [model-a]
  openai:
    api_format: openai
    base_url: https://api.openai.com
    api_key_file: file-key.txt
    models: [model-b]
apps:
  bot:
    allowed_models: [model-a]
alerts:
  channels:
    hook:
      type: webhook
      url: ${HOOK_URL}
      signing_secret: ${SIGNING}
    ntfy:
      type: ntfy
      server_url: https://ntfy.example
      topic: alerts
      token: ${NTFY_TOKEN}
`

func everySecretHarness(t *testing.T) (*harness, func(suffix string)) {
	t.Helper()
	vars := func(suffix string) *Environment {
		return NewEnvironment(map[string]string{
			"ANTHROPIC_API_KEY": envCanary + "-initial",
			"ENV_KEY":           envCanary + suffix,
			"HOOK_URL":          "https://hooks.example/" + hookCanary + suffix,
			"SIGNING":           signCanary + suffix,
			"NTFY_TOKEN":        tokenCanary + suffix,
		})
	}
	h := newHarness(t, baseYAML, func(o *ManagerOptions) {
		o.Options.Environment = vars("")
	})
	rotate := func(suffix string) {
		h.manager.opts.Environment = vars(suffix)
		writeFile(t, filepath.Join(h.dir, "file-key.txt"), fileCanary+suffix+"\n")
	}
	rotate("")
	if status := h.save(t, everySecretYAML); status.Result != ResultApplied {
		t.Fatalf("setup reload: %+v", status)
	}
	return h, rotate
}

func TestEventsMaskAllSecretSources(t *testing.T) {
	h, rotate := everySecretHarness(t)
	initial := len(h.sink.snapshot())

	rotate("-rotated")
	if status := h.save(t, everySecretYAML); status.Result != ResultApplied {
		t.Fatalf("rotation: %+v", status)
	}
	events := h.sink.snapshot()[initial:]
	if len(events) != 1 {
		t.Fatalf("events: %+v", events)
	}
	want := map[string]bool{
		"alerts.channels.hook.signing_secret": true,
		"alerts.channels.hook.url":            true,
		"alerts.channels.ntfy.token":          true,
		"providers.anthropic.api_key":         true,
		"providers.openai.api_key":            true,
	}
	if len(events[0].Changes) != len(want) {
		t.Fatalf("changes: %+v", events[0].Changes)
	}
	for _, change := range events[0].Changes {
		if !want[change.Path] || !change.SecretChanged || *change.Old != Redacted || *change.New != Redacted {
			t.Fatalf("unexpected change: %+v", change)
		}
	}
	assertNoCanary(t, h)
}

func TestRejectedSecretNeverAppearsInErrors(t *testing.T) {
	h, _ := everySecretHarness(t)
	const rejected = secretCanary + "rejected-literal"
	h.save(t, replaced(t, everySecretYAML, "${ENV_KEY}", rejected))
	h.save(t, replaced(t, everySecretYAML, "${HOOK_URL}", "https://hooks.example/"+rejected))
	h.save(t, replaced(t, everySecretYAML, "${NTFY_TOKEN}", rejected))
	h.save(t, "providers: ["+rejected)
	if h.manager.Status().Result != ResultRejected {
		t.Fatalf("status: %+v", h.manager.Status())
	}
	assertNoCanary(t, h)
}

// assertNoCanary checks every public surface for any canary value.
func assertNoCanary(t *testing.T, h *harness) {
	t.Helper()
	status, err := json.Marshal(h.manager.Status())
	if err != nil {
		t.Fatal(err)
	}
	events, err := json.Marshal(h.sink.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	surfaces := map[string]string{
		"logs":   h.logs.String(),
		"status": string(status),
		"events": string(events),
		"yaml":   h.manager.Capture().EffectiveYAML(),
	}
	for name, text := range surfaces {
		if strings.Contains(text, secretCanary) {
			t.Errorf("%s exposes a secret: %s", name, text)
		}
	}
}

func TestMapPathsEscapeUntrustedNames(t *testing.T) {
	cases := map[string]string{
		"anthropic":      "providers.anthropic",
		"ollama/qwen2.5": `providers["ollama/qwen2.5"]`,
		"a.b":            `providers["a.b"]`,
		"evil\nkey":      `providers["evil\nkey"]`,
		"\x1b[31mred":    `providers["\x1b[31mred"]`,
	}
	for name, want := range cases {
		if got := joinPath("providers", name); got != want {
			t.Errorf("joinPath(%q) = %s, want %s", name, got, want)
		}
	}
	if joinPath("a", "b.c") == joinPath("a.b", "c") {
		t.Error("distinct names must not produce the same path")
	}
	h := newHarness(t, baseYAML)
	text := replaced(t, baseYAML, "  model-b: {input: 1.00", "  model-b: {input: 2.00")
	text = replaced(t, text, "models: [local/model]", "models: [local/model, local/two]")
	text = replaced(t, text, "pricing:\n", "pricing:\n  local/two: {input: 0, output: 0, cache_write: 0, cache_read: 0}\n")
	if status := h.save(t, text); status.Result != ResultApplied {
		t.Fatalf("status: %+v", status)
	}
	var paths []string
	for _, change := range h.sink.snapshot()[0].Changes {
		paths = append(paths, change.Path)
	}
	joined := strings.Join(paths, " ")
	if !strings.Contains(joined, `pricing["local/two"].input`) || !strings.Contains(joined, "pricing.model-b.input") {
		t.Fatalf("paths: %s", joined)
	}
	for i := 1; i < len(paths); i++ {
		if paths[i-1] > paths[i] {
			t.Fatalf("changes must be ordered by path: %v", paths)
		}
	}
}

func TestEffectiveYAMLContainsDefaultsWithoutSecrets(t *testing.T) {
	h, _ := everySecretHarness(t)
	text := h.manager.Capture().EffectiveYAML()
	for _, want := range []string{
		"port: 8080", "port: 8081", "bind: 127.0.0.1", "max_body_bytes: 10000000",
		"fail_mode: fail_closed", "tls_min_version: \"1.2\"", "max_concurrent: 20",
		"api_key: '[REDACTED]'", "url: '[REDACTED]'", "signing_secret: '[REDACTED]'", "token: '[REDACTED]'",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("effective configuration lacks %q:\n%s", want, text)
		}
	}
	for _, forbidden := range []string{secretCanary, "file-key.txt", "ENV_KEY"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("effective configuration exposes %q", forbidden)
		}
	}
	if h.manager.Capture().EffectiveYAML() != text {
		t.Error("the rendering must be deterministic")
	}
}

func TestStatusJSONShape(t *testing.T) {
	h := newHarness(t, baseYAML)
	initial, err := json.Marshal(h.manager.Status())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(initial), `"errors":[]`) || !strings.Contains(string(initial), `"result":"initial"`) {
		t.Fatalf("initial status: %s", initial)
	}
	h.save(t, replaced(t, baseYAML, "  bot:\n", "  bot:\n    on_limit: sometimes\n"))
	rejected, err := json.Marshal(h.manager.Status())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"active_generation":1`, `"result":"rejected"`, `"code":"invalid_policy"`,
		`"path":"apps.bot.on_limit"`, `"line":16`, `"message":"must be one of block, downgrade, warn"`,
	} {
		if !strings.Contains(string(rejected), want) {
			t.Errorf("status lacks %s: %s", want, rejected)
		}
	}
}

func TestReloadSummaryFormatting(t *testing.T) {
	text := func(value string) *string { return &value }
	got := reloadSummary(3, []Change{
		{Path: "apps.bot-discord.budget.usd_per_day", Old: text("2.000000"), New: text("5.000000")},
		{Path: "apps.bot-discord.max_tokens", Old: text("4096"), New: nil},
	})
	const want = "config reloaded: 3 apps, 2 changes (bot-discord.budget.usd_per_day 2.00 → 5.00, bot-discord.max_tokens 4096 → (none))"
	if got != want {
		t.Fatalf("got %s", got)
	}
	if value := "1.234500"; displayValue(&value) != "1.2345" {
		t.Fatalf("display: %s", displayValue(&value))
	}
}
