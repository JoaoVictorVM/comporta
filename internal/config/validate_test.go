package config

import (
	"strings"
	"testing"
)

const providerBlock = `    api_format: openai
    models: [m]
`

func withProviderURL(url string) string {
	return "timezone: UTC\nproviders:\n  p:\n    api_key: ${K}\n" + providerBlock + "    base_url: " + url + "\n"
}

func TestAppNameAndModelReferences(t *testing.T) {
	requireDiagnostic(t, replaced(t, baseYAML, "  bot:\n", "  Bad_Name:\n"), CodeInvalidValue, "apps.Bad_Name")
	long := strings.Repeat("a", 41)
	requireDiagnostic(t, replaced(t, baseYAML, "  bot:\n", "  "+long+":\n"), CodeInvalidValue, "apps."+long)
	requireDiagnostic(t, replaced(t, baseYAML, "allowed_models: [model-a, model-b]", "allowed_models: [model-a, ghost]"), CodeInvalidReference, "apps.bot.allowed_models[1]")
	requireDiagnostic(t, replaced(t, baseYAML, "allowed_models: [model-a, model-b]", "allowed_models: []"), CodeInvalidValue, "apps.bot.allowed_models")
	requireDiagnostic(t, replaced(t, baseYAML, "allowed_models: [model-a, model-b]", "allowed_models: [model-a, model-a]"), CodeInvalidValue, "apps.bot.allowed_models[1]")
	requireDiagnostic(t, replaced(t, baseYAML, "models: [local/model]", "models: [model-a]"), CodeInvalidValue, "providers.local.models[0]")
	requireDiagnostic(t, baseYAML+"fail_mode: maybe\n", CodeInvalidValue, "fail_mode")
	requireDiagnostic(t, "timezone: UTC\nproviders: {}\n", CodeInvalidValue, "providers")
	requireDiagnostic(t, "timezone: UTC\n", CodeInvalidValue, "providers")
	requireDiagnostic(t, "", CodeInvalidValue, "providers")
	requireDiagnostic(t, replaced(t, baseYAML, "  model-b: {input", "  ghost: {input"), CodeInvalidReference, `pricing.ghost`)
	requireDiagnostic(t, replaced(t, baseYAML, "cache_read: 0.10", "cache_read: 0.10, extra: 1"), CodeUnknownField, "pricing.model-b.extra")
	requireDiagnostic(t, replaced(t, baseYAML, ", cache_read: 0.10", ""), CodeInvalidValue, "pricing.model-b.cache_read")
}

func TestLocalModelAliasIsQuotedInPaths(t *testing.T) {
	text := baseYAML + "  other:\n    allowed_models: [local/model]\n    max_tokens: 0\n"
	requireDiagnostic(t, text, CodeInvalidValue, "apps.other.max_tokens")
	candidate := requireValid(t, replaced(t, baseYAML, "pricing:\n", "pricing:\n  local/model: {input: 0, output: 0, cache_write: 0, cache_read: 0}\n"))
	if _, ok := candidate.cfg.Pricing["local/model"]; !ok {
		t.Fatal("price of a model alias must be kept")
	}
}

func TestProviderURLRules(t *testing.T) {
	valid := []string{
		"https://api.example.com",
		"https://api.example.com/v1",
		"http://localhost:11434",
		"http://127.0.0.1:8000",
		"http://10.0.0.5",
		"http://192.168.1.10:8000",
		"http://[::1]:8000",
	}
	for _, url := range valid {
		requireValid(t, withProviderURL(url))
	}
	invalid := []string{
		"ftp://api.example.com",
		"api.example.com",
		"http://api.example.com",
		"http://localhost.example.com",
		"https://user:pass@api.example.com",
		"https://api.example.com?token=1",
		"https://api.example.com#frag",
		"http://8.8.8.8",
	}
	for _, url := range invalid {
		requireDiagnostic(t, withProviderURL(url), CodeInvalidValue, "providers.p.base_url")
	}
	requireDiagnostic(t, "timezone: UTC\nproviders:\n  bad_name:\n    api_key: ${K}\n"+providerBlock+"    base_url: https://x.example\n", CodeInvalidValue, "providers.bad_name")
	requireDiagnostic(t, replaced(t, withProviderURL("https://x.example"), "api_format: openai", "api_format: gemini"), CodeInvalidValue, "providers.p.api_format")
	requireDiagnostic(t, withProviderURL("https://x.example")+"    tls_min_version: \"1.1\"\n", CodeInvalidValue, "providers.p.tls_min_version")
	requireDiagnostic(t, withProviderURL("https://x.example")+"    tls_min_version: 1.2\n", CodeInvalidValue, "providers.p.tls_min_version")
}

func TestTimezoneValidation(t *testing.T) {
	requireValid(t, replaced(t, baseYAML, "timezone: UTC", "timezone: America/Sao_Paulo"))
	for _, name := range []string{"Mars/Olympus", "Local", "../../etc/passwd"} {
		requireDiagnostic(t, replaced(t, baseYAML, "timezone: UTC", "timezone: "+name), CodeInvalidValue, "timezone")
	}
	requireDiagnostic(t, replaced(t, baseYAML, "  bot:\n", "  bot:\n    timezone: Mars/Olympus\n"), CodeInvalidValue, "apps.bot.timezone")

	withoutRoot := replaced(t, baseYAML, "timezone: UTC\n", "")
	candidate := requireValid(t, withoutRoot)
	if candidate.cfg.Timezone != "UTC" {
		t.Fatalf("system timezone must be the default, got %q", candidate.cfg.Timezone)
	}
	_, diags := loadText(t, withoutRoot, Options{})
	if !hasDiagnostic(diags, CodeInvalidValue, "timezone") {
		t.Fatalf("an unknown system timezone must require an explicit one, got %+v", diags)
	}
}

func TestPositiveLimits(t *testing.T) {
	cases := map[string]string{
		"apps.bot.max_tokens":                     "    max_tokens: 0\n",
		"apps.bot.limits.requests_per_minute":     "    limits: {requests_per_minute: -1}\n",
		"apps.bot.limits.tokens_per_minute":       "    limits: {tokens_per_minute: 0}\n",
		"apps.bot.limits.max_concurrent":          "    limits: {max_concurrent: 1.5}\n",
		"apps.bot.budget.tokens_per_day":          "    budget: {tokens_per_day: 0}\n",
		"apps.bot.budget.usd_per_day":             "    budget: {usd_per_day: 0}\n",
		"apps.bot.loop_guard.similar_requests":    "    loop_guard: {similar_requests: 0}\n",
		"apps.bot.loop_guard.cooldown":            "    loop_guard: {cooldown: 0s}\n",
		"apps.bot.loop_guard.similar_window":      "    loop_guard: {similar_window: soon}\n",
		"apps.bot.cache.ttl_seconds":              "    cache: {ttl_seconds: 3601}\n",
		"apps.bot.per_user.limits.max_concurrent": "    per_user: {limits: {max_concurrent: 0}}\n",
		"apps.bot.max_tokens ":                    "    max_tokens: 9223372036854775808\n",
	}
	base := replaced(t, baseYAML, "    budget: {usd_per_day: 2.00}\n", "")
	for path, line := range cases {
		t.Run(path, func(t *testing.T) {
			requireDiagnostic(t, base+line, CodeInvalidValue, strings.TrimSpace(path))
		})
	}
}

func TestListenerRules(t *testing.T) {
	requireDiagnostic(t, baseYAML+"listeners: {proxy: {port: 70000}}\n", CodeInvalidValue, "listeners.proxy.port")
	requireDiagnostic(t, baseYAML+"listeners: {proxy: {bind: localhost}}\n", CodeInvalidValue, "listeners.proxy.bind")
	requireDiagnostic(t, baseYAML+"listeners: {proxy: {port: 9000}, admin: {port: 9000}}\n", CodeInvalidValue, "listeners.admin.port")
	requireDiagnostic(t, baseYAML+"listeners: {proxy: {bind: 0.0.0.0, port: 9000}, admin: {port: 9000}}\n", CodeInvalidValue, "listeners.admin.port")
	requireValid(t, baseYAML+"listeners: {proxy: {bind: 127.0.0.1, port: 9000}, admin: {bind: 127.0.0.2, port: 9000}}\n")
	for _, host := range []string{"*.example.com", "https://x.example", "x.example/path", "x.example:80"} {
		requireDiagnostic(t, baseYAML+"listeners: {allowed_hosts: [\""+host+"\"]}\n", CodeInvalidValue, "listeners.allowed_hosts[0]")
	}
	cfg := requireValid(t, baseYAML+"listeners: {allowed_hosts: [Gateway.Internal, localhost]}\n").cfg
	got := strings.Join(cfg.Listeners.AllowedHosts, ",")
	if got != "localhost,127.0.0.1,[::1],gateway.internal" {
		t.Fatalf("allowed hosts: %s", got)
	}
}

func TestDowngradeRequiresAllowedPricedModel(t *testing.T) {
	policy := func(lines string) string {
		return replaced(t, baseYAML, "  bot:\n", "  bot:\n"+lines)
	}
	requireValid(t, policy("    on_limit: downgrade\n    downgrade_to: model-b\n"))
	requireDiagnostic(t, policy("    on_limit: sometimes\n"), CodeInvalidPolicy, "apps.bot.on_limit")
	requireDiagnostic(t, policy("    on_limit: downgrade\n"), CodeInvalidReference, "apps.bot.on_limit")
	requireDiagnostic(t, policy("    on_limit: downgrade\n    downgrade_to: ghost\n"), CodeInvalidReference, "apps.bot.downgrade_to")
	requireDiagnostic(t, policy("    on_limit: downgrade\n    downgrade_to: local/model\n"), CodeInvalidReference, "apps.bot.downgrade_to")
	requireDiagnostic(t, policy("    on_limit: downgrade\n    downgrade_to: model-a\n"), CodeInvalidReference, "apps.bot.downgrade_to")

	noBudget := replaced(t, baseYAML, "    budget: {usd_per_day: 2.00}\n", "    on_limit: downgrade\n    downgrade_to: model-b\n")
	requireDiagnostic(t, noBudget, CodeInvalidValue, "apps.bot.on_limit")

	localTarget := replaced(t, baseYAML, "allowed_models: [model-a, model-b]", "allowed_models: [model-a, local/model]\n    on_limit: downgrade\n    downgrade_to: local/model")
	requireValid(t, localTarget)

	catalogTarget := policy("    on_limit: downgrade\n    downgrade_to: model-a\n")
	if _, diags := loadText(t, catalogTarget, Options{SystemTimezone: "UTC", Catalog: fakeCatalog{"model-a": true}}); diags != nil {
		t.Fatalf("the catalog must satisfy the price check: %+v", diags)
	}
	_, diags := loadText(t, catalogTarget, Options{SystemTimezone: "UTC", Catalog: fakeCatalog{}})
	if !hasDiagnostic(diags, CodeInvalidReference, "apps.bot.downgrade_to") {
		t.Fatalf("a catalog miss must be rejected: %+v", diags)
	}
}

type fakeCatalog map[string]bool

func (c fakeCatalog) HasPrice(model string) bool { return c[model] }

func TestFallbackRules(t *testing.T) {
	chain := func(entries string) string {
		return replaced(t, baseYAML, "  bot:\n", "  bot:\n    fallback: ["+entries+"]\n")
	}
	if candidate := requireValid(t, chain("model-b")); len(candidate.Warnings()) != 0 {
		t.Fatalf("unexpected warnings: %+v", candidate.Warnings())
	}
	requireDiagnostic(t, chain("ghost"), CodeInvalidReference, "apps.bot.fallback[0]")
	requireDiagnostic(t, chain("local/model"), CodeInvalidReference, "apps.bot.fallback[0]")
	requireDiagnostic(t, chain("model-b, model-b"), CodeInvalidValue, "apps.bot.fallback[1]")

	six := strings.Replace(baseYAML, "models: [model-a, model-b]", "models: [model-a, model-b, c, d, e]", 1)
	six = strings.Replace(six, "allowed_models: [model-a, model-b]", "allowed_models: [model-a, model-b, c, d, e]\n    fallback: [model-a, model-b, c, d, e]", 1)
	requireDiagnostic(t, six, CodeInvalidValue, "apps.bot.fallback")

	mixed := replaced(t, baseYAML, "allowed_models: [model-a, model-b]", "allowed_models: [model-a, local/model]\n    fallback: [local/model]")
	warnings := requireValid(t, mixed).Warnings()
	if !hasDiagnostic(warnings, CodeFallbackMixedFormat, "apps.bot.fallback") {
		t.Fatalf("mixed formats must warn without removing entries: %+v", warnings)
	}
}

func TestAlertAndTelemetryRules(t *testing.T) {
	channel := func(body string) string {
		return baseYAML + "alerts:\n  channels:\n    ops:\n" + body
	}
	requireValid(t, channel("      type: slack\n      url: ${SLACK_URL}\n"))
	requireValid(t, channel("      type: webhook\n      url_file: u\n      signing_secret: ${S}\n"))
	requireValid(t, channel("      type: ntfy\n      server_url: https://ntfy.example\n      topic: t\n"))
	requireDiagnostic(t, channel("      type: pager\n"), CodeInvalidValue, "alerts.channels.ops.type")
	requireDiagnostic(t, channel("      url: ${X}\n"), CodeInvalidValue, "alerts.channels.ops.type")
	requireDiagnostic(t, channel("      type: slack\n"), CodeInvalidValue, "alerts.channels.ops.url")
	requireDiagnostic(t, channel("      type: slack\n      url: https://hooks.example/token\n"), CodeLiteralSecret, "alerts.channels.ops.url")
	requireDiagnostic(t, channel("      type: slack\n      url: ${A}\n      url_file: f\n"), CodeInvalidValue, "alerts.channels.ops")
	requireDiagnostic(t, channel("      type: slack\n      url: ${A}\n      signing_secret: ${B}\n"), CodeUnknownField, "alerts.channels.ops.signing_secret")
	requireDiagnostic(t, channel("      type: webhook\n      url: ${A}\n"), CodeInvalidValue, "alerts.channels.ops.signing_secret")
	requireDiagnostic(t, channel("      type: ntfy\n      server_url: http://ntfy.example\n      topic: t\n"), CodeInvalidValue, "alerts.channels.ops.server_url")
	requireDiagnostic(t, channel("      type: ntfy\n      server_url: https://u:p@ntfy.example\n      topic: t\n"), CodeInvalidValue, "alerts.channels.ops.server_url")
	requireDiagnostic(t, channel("      type: ntfy\n      server_url: https://ntfy.example\n"), CodeInvalidValue, "alerts.channels.ops.topic")
	requireDiagnostic(t, channel("      type: ntfy\n      server_url: https://ntfy.example\n      topic: t\n      token: literal\n"), CodeLiteralSecret, "alerts.channels.ops.token")

	requireDiagnostic(t, baseYAML+"alerts: {thresholds: [80, 50]}\n", CodeInvalidValue, "alerts.thresholds[1]")
	requireDiagnostic(t, baseYAML+"alerts: {thresholds: [50, 50]}\n", CodeInvalidValue, "alerts.thresholds[1]")
	requireDiagnostic(t, baseYAML+"alerts: {thresholds: [101]}\n", CodeInvalidValue, "alerts.thresholds[0]")
	requireDiagnostic(t, baseYAML+"telemetry: {otel_enabled: true}\n", CodeInvalidValue, "telemetry.otel_enabled")
	requireDiagnostic(t, baseYAML+"telemetry: {otel_endpoint: \"https://u:p@otel.example\"}\n", CodeInvalidValue, "telemetry.otel_endpoint")
	requireDiagnostic(t, baseYAML+"telemetry: {otel_endpoint: \"https://otel.example?x=1\"}\n", CodeInvalidValue, "telemetry.otel_endpoint")
	requireDiagnostic(t, replaced(t, baseYAML, "  bot:\n", "  bot:\n    detection: {cpf: maybe}\n"), CodeInvalidValue, "apps.bot.detection.cpf")
}

func TestDiagnosticPositions(t *testing.T) {
	diags := requireDiagnostic(t, replaced(t, baseYAML, "  bot:\n", "  bot:\n    on_limit: sometimes\n"), CodeInvalidPolicy, "apps.bot.on_limit")
	if diags[0].Line != 16 || diags[0].Column == 0 {
		t.Fatalf("unexpected position: %+v", diags[0])
	}
	if got := diags[0].String(); got != "apps.bot.on_limit: must be one of block, downgrade, warn" {
		t.Fatalf("documented message changed: %s", got)
	}
}
