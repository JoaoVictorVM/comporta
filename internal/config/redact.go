package config

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// publicTree is the single source of every public representation, so masking
// happens in one place. Secret fields appear under their reference name whatever
// the source, which keeps file paths out and makes a change of source show up as
// a change of the same key.
func publicTree(cfg *Config) map[string]any {
	providers := map[string]any{}
	for name, provider := range cfg.Providers {
		entry := map[string]any{
			"api_format":      string(provider.APIFormat),
			"base_url":        provider.BaseURL,
			"models":          stringsToAny(provider.Models),
			"tls_min_version": provider.TLSMinVersion,
		}
		addSecret(entry, "api_key", provider.APIKey)
		providers[name] = entry
	}
	apps := map[string]any{}
	for name, app := range cfg.Apps {
		apps[name] = publicApp(app)
	}
	pricing := map[string]any{}
	for model, price := range cfg.Pricing {
		pricing[model] = map[string]any{
			"input":       price.Input.String(),
			"output":      price.Output.String(),
			"cache_write": price.CacheWrite.String(),
			"cache_read":  price.CacheRead.String(),
		}
	}
	channels := map[string]any{}
	for name, channel := range cfg.Alerts.Channels {
		entry := map[string]any{"type": string(channel.Type)}
		addSecret(entry, "url", channel.URL)
		addSecret(entry, "signing_secret", channel.SigningSecret)
		addSecret(entry, "token", channel.Token)
		addText(entry, "server_url", channel.ServerURL)
		addText(entry, "topic", channel.Topic)
		channels[name] = entry
	}
	thresholds := make([]any, len(cfg.Alerts.Thresholds))
	for i, threshold := range cfg.Alerts.Thresholds {
		thresholds[i] = threshold
	}
	telemetry := map[string]any{
		"metrics_enabled": cfg.Telemetry.MetricsEnabled,
		"otel_enabled":    cfg.Telemetry.OTelEnabled,
	}
	addText(telemetry, "otel_endpoint", cfg.Telemetry.OTelEndpoint)

	return map[string]any{
		"data_dir":  cfg.DataDir,
		"timezone":  cfg.Timezone,
		"fail_mode": string(cfg.FailMode),
		"providers": providers,
		"apps":      apps,
		"pricing":   pricing,
		"listeners": map[string]any{
			"proxy":                   publicListener(cfg.Listeners.Proxy),
			"admin":                   publicListener(cfg.Listeners.Admin),
			"allowed_hosts":           stringsToAny(cfg.Listeners.AllowedHosts),
			"max_body_bytes":          cfg.Listeners.MaxBodyBytes,
			"read_header_timeout":     cfg.Listeners.ReadHeaderTimeout.String(),
			"non_streaming_timeout":   cfg.Listeners.NonStreamingTimeout.String(),
			"streaming_idle_timeout":  cfg.Listeners.StreamingIdleTimeout.String(),
			"upstream_header_timeout": cfg.Listeners.UpstreamHeaderTimeout.String(),
		},
		"cache": map[string]any{"max_bytes": cfg.Cache.MaxBytes},
		"alerts": map[string]any{
			"channels":           channels,
			"thresholds":         thresholds,
			"anomaly_multiplier": cfg.Alerts.AnomalyMultiplier,
			"anomaly_min_usd":    cfg.Alerts.AnomalyMinUSD.String(),
		},
		"telemetry": telemetry,
	}
}

func publicListener(listener Listener) map[string]any {
	return map[string]any{"bind": listener.Bind, "port": listener.Port}
}

func publicApp(app App) map[string]any {
	detection := app.Detection
	entry := map[string]any{
		"require_user":   app.RequireUser,
		"allowed_models": stringsToAny(app.AllowedModels),
		"max_tokens":     app.MaxTokens,
		"on_limit":       string(app.OnLimit),
		"timezone":       app.Timezone,
		"fail_mode":      string(app.FailMode),
		"limits":         publicLimits(app.Limits),
		"loop_guard": map[string]any{
			"enabled":             app.LoopGuard.Enabled,
			"similar_requests":    app.LoopGuard.SimilarRequests,
			"similar_window":      app.LoopGuard.SimilarWindow.String(),
			"requests_per_minute": app.LoopGuard.RequestsPerMinute,
			"cooldown":            app.LoopGuard.Cooldown.String(),
		},
		"fallback": stringsToAny(app.Fallback),
		"cache": map[string]any{
			"enabled":             app.Cache.Enabled,
			"ttl_seconds":         app.Cache.TTLSeconds,
			"shared_across_users": app.Cache.SharedAcrossUsers,
		},
		"detection": map[string]any{
			"cpf": string(detection.CPF), "cnpj": string(detection.CNPJ), "card": string(detection.Card),
			"email": string(detection.Email), "phone": string(detection.Phone), "pix": string(detection.Pix),
			"secret": string(detection.Secret), "rehydrate": detection.Rehydrate,
		},
	}
	addText(entry, "downgrade_to", app.DowngradeTo)
	perUser := map[string]any{"limits": publicLimits(app.PerUser.Limits)}
	addBudget(perUser, "budget", app.PerUser.Budget)
	addBudget(perUser, "hard_cap", app.PerUser.HardCap)
	entry["per_user"] = perUser
	addBudget(entry, "budget", app.Budget)
	addBudget(entry, "hard_cap", app.HardCap)
	return entry
}

func publicLimits(limits Limits) map[string]any {
	out := map[string]any{"max_concurrent": limits.MaxConcurrent}
	if limits.RequestsPerMinute > 0 {
		out["requests_per_minute"] = limits.RequestsPerMinute
	}
	if limits.TokensPerMinute > 0 {
		out["tokens_per_minute"] = limits.TokensPerMinute
	}
	return out
}

func addBudget(parent map[string]any, key string, budget Budget) {
	if budget.isZero() {
		return
	}
	out := map[string]any{}
	if budget.USDPerDay > 0 {
		out["usd_per_day"] = budget.USDPerDay.String()
	}
	if budget.USDPerMonth > 0 {
		out["usd_per_month"] = budget.USDPerMonth.String()
	}
	if budget.TokensPerDay > 0 {
		out["tokens_per_day"] = budget.TokensPerDay
	}
	if budget.TokensPerMonth > 0 {
		out["tokens_per_month"] = budget.TokensPerMonth
	}
	parent[key] = out
}

func addSecret(parent map[string]any, key string, ref SecretRef) {
	if ref.IsSet() {
		parent[key] = Redacted
	}
}

func addText(parent map[string]any, key, value string) {
	if value != "" {
		parent[key] = value
	}
}

func stringsToAny(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}

// flattenTree returns the leaves keyed by path. A list is one leaf because its
// order is meaningful and a change to it reads better as a whole.
func flattenTree(tree map[string]any) map[string]string {
	leaves := map[string]string{}
	var walk func(path string, node any)
	walk = func(path string, node any) {
		switch value := node.(type) {
		case map[string]any:
			for key, child := range value {
				walk(joinPath(path, key), child)
			}
		case []any:
			encoded, err := json.Marshal(value)
			if err != nil {
				encoded = []byte(`"[unrenderable]"`)
			}
			leaves[path] = string(encoded)
		case int64:
			leaves[path] = strconv.FormatInt(value, 10)
		default:
			leaves[path] = fmt.Sprint(value)
		}
	}
	walk("", tree)
	return leaves
}

// renderTree produces deterministic text: the YAML encoder sorts mapping keys.
func renderTree(tree map[string]any) string {
	out, err := yaml.Marshal(tree)
	if err != nil {
		return ""
	}
	return string(out)
}

// EventType names a configuration notification.
type EventType string

const (
	EventConfigChanged      EventType = "config_changed"
	EventConfigReloadFailed EventType = "config_reload_failed"
)

// Change is one changed leaf, with secret values masked. A nil side means the key
// was absent on that side.
type Change struct {
	Path          string  `json:"path"`
	Old           *string `json:"old"`
	New           *string `json:"new"`
	SecretChanged bool    `json:"secret_changed,omitempty"`
}

// diffSnapshots compares two generations leaf by leaf, ordered by path. Secrets
// are compared by value because their public projection is always the same mask.
func diffSnapshots(previous, next *Snapshot) []Change {
	paths := slices.Collect(maps.Keys(previous.leaves))
	for path := range next.leaves {
		if _, known := previous.leaves[path]; !known {
			paths = append(paths, path)
		}
	}
	slices.Sort(paths)

	var changes []Change
	for _, path := range paths {
		oldValue, hadOld := previous.leaves[path]
		newValue, hasNew := next.leaves[path]
		oldSecret, oldIsSecret := previous.secrets[path]
		newSecret, newIsSecret := next.secrets[path]
		secretChanged := (oldIsSecret || newIsSecret) && oldSecret.value != newSecret.value
		if hadOld == hasNew && oldValue == newValue && !secretChanged {
			continue
		}
		change := Change{Path: path, SecretChanged: secretChanged}
		if hadOld {
			change.Old = &oldValue
		}
		if hasNew {
			change.New = &newValue
		}
		changes = append(changes, change)
	}
	return changes
}

const maxLoggedChanges = 10

// reloadSummary renders the documented log line, for example
// config reloaded: 3 apps, 2 changes (bot-discord.budget.usd_per_day 2.00 → 5.00).
func reloadSummary(apps int, changes []Change) string {
	parts := make([]string, 0, maxLoggedChanges+1)
	for i, change := range changes {
		if i == maxLoggedChanges {
			parts = append(parts, fmt.Sprintf("+%d more", len(changes)-maxLoggedChanges))
			break
		}
		parts = append(parts, strings.TrimPrefix(change.Path, "apps.")+" "+displayValue(change.Old)+" → "+displayValue(change.New))
	}
	return fmt.Sprintf("config reloaded: %s, %s (%s)", counted(apps, "app"), counted(len(changes), "change"), strings.Join(parts, ", "))
}

func counted(count int, noun string) string {
	if count == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(count) + " " + noun + "s"
}

var fullPrecisionMoney = regexp.MustCompile(`^[0-9]+\.[0-9]{6}$`)

// displayValue shortens money for reading in logs, keeping two decimal places at
// least. Structured events keep the full precision.
func displayValue(value *string) string {
	if value == nil {
		return "(none)"
	}
	if !fullPrecisionMoney.MatchString(*value) {
		return *value
	}
	whole, fraction, _ := strings.Cut(*value, ".")
	fraction = strings.TrimRight(fraction, "0")
	for len(fraction) < 2 {
		fraction += "0"
	}
	return whole + "." + fraction
}
