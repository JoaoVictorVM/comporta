package config

import (
	"maps"
	"net/netip"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// validateConfig applies relational rules and compiles derived values. It
// returns the non-fatal warnings; errors go to the decoder's list.
func validateConfig(cfg *Config, d *decoder, baseDir string, opts Options) []Diagnostic {
	warnings := &diagnosticList{}

	if !filepath.IsAbs(cfg.DataDir) {
		cfg.DataDir = filepath.Join(baseDir, cfg.DataDir)
	}
	cfg.DataDir = filepath.Clean(cfg.DataDir)

	validateTimezone(cfg, d, opts)
	validateListeners(&cfg.Listeners, d)
	owners := validateProviders(cfg, d)
	validatePricing(cfg, owners, d)
	for _, name := range slices.Sorted(maps.Keys(cfg.Apps)) {
		app := cfg.Apps[name]
		validateApp(&app, joinPath("apps", name), cfg, owners, d, opts, warnings)
		cfg.Apps[name] = app
	}
	validateAlerts(cfg, d)
	validateTelemetry(&cfg.Telemetry, d)
	return warnings.sorted()
}

func validateTimezone(cfg *Config, d *decoder, opts Options) {
	if cfg.Timezone == "" {
		if _, declared := d.positions["timezone"]; declared {
			return
		}
		cfg.Timezone = opts.SystemTimezone
		if cfg.Timezone == "" {
			d.reportAt(CodeInvalidValue, "timezone", "system timezone has no IANA identifier; set timezone explicitly")
			return
		}
	}
	if !validTimezone(cfg.Timezone) {
		d.reportAt(CodeInvalidValue, "timezone", "must be a valid IANA timezone name")
	}
}

// validTimezone excludes the names time.LoadLocation accepts that do not
// identify a calendar: "" and "Local" depend on the host.
func validTimezone(name string) bool {
	if name == "" || name == "Local" {
		return false
	}
	_, err := time.LoadLocation(name)
	return err == nil
}

func validateListeners(listeners *Listeners, d *decoder) {
	proxy := normalizeBind(&listeners.Proxy, "listeners.proxy.bind", d)
	admin := normalizeBind(&listeners.Admin, "listeners.admin.bind", d)
	if proxy.IsValid() && admin.IsValid() && listeners.Proxy.Port == listeners.Admin.Port {
		if proxy == admin || proxy.IsUnspecified() || admin.IsUnspecified() {
			d.reportAt(CodeInvalidValue, "listeners.admin.port", "proxy and admin listeners must not share a socket")
		}
	}

	base := []string{"localhost", "127.0.0.1", "[::1]"}
	hosts := slices.Clone(base)
	for i, host := range listeners.AllowedHosts {
		normalized := strings.ToLower(host)
		if !validAllowedHost(normalized) {
			d.reportAt(CodeInvalidValue, indexPath("listeners.allowed_hosts", i), "must be a bare host name, without wildcards, URLs, ports or paths")
			continue
		}
		if !slices.Contains(hosts, normalized) {
			hosts = append(hosts, normalized)
		}
	}
	listeners.AllowedHosts = hosts
}

func normalizeBind(listener *Listener, path string, d *decoder) netip.Addr {
	addr, err := netip.ParseAddr(listener.Bind)
	if err != nil || addr.Zone() != "" {
		d.reportAt(CodeInvalidValue, path, "must be an IP address")
		return netip.Addr{}
	}
	addr = addr.Unmap()
	listener.Bind = addr.String()
	return addr
}

func validAllowedHost(host string) bool {
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		addr, err := netip.ParseAddr(host[1 : len(host)-1])
		return err == nil && addr.Is6()
	}
	return !strings.ContainsAny(host, "*/:\\?#@ \t")
}

// validateProviders returns the owner of each declared model.
func validateProviders(cfg *Config, d *decoder) map[string]string {
	owners := map[string]string{}
	if len(cfg.Providers) == 0 {
		if _, declared := d.positions["providers"]; declared {
			d.reportAt(CodeInvalidValue, "providers", "at least one provider is required")
		} else {
			d.diags.add(Diagnostic{Code: CodeInvalidValue, Path: "providers", Message: "at least one provider is required"})
		}
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Providers)) {
		provider := cfg.Providers[name]
		path := joinPath("providers", name)
		if provider.BaseURL != "" {
			local, problem := checkEndpoint(provider.BaseURL, false)
			if problem != "" {
				d.reportAt(CodeInvalidValue, joinPath(path, "base_url"), problem)
			} else {
				provider.Local = local
				if !local && !provider.APIKey.IsSet() && !hasSecretField(d, path) {
					d.reportAt(CodeInvalidValue, path, "api_key or api_key_file is required for a remote provider")
				}
			}
		}
		for i, model := range provider.Models {
			if owner, taken := owners[model]; taken {
				d.reportAt(CodeInvalidValue, indexPath(joinPath(path, "models"), i), "model "+quoteText(model)+" is already declared by provider "+owner)
				continue
			}
			owners[model] = name
		}
		cfg.Providers[name] = provider
	}
	return owners
}

// hasSecretField tells a rejected key apart from an absent one, so a refused
// literal is not reported a second time as missing.
func hasSecretField(d *decoder, path string) bool {
	_, env := d.positions[joinPath(path, "api_key")]
	_, file := d.positions[joinPath(path, "api_key_file")]
	return env || file
}

// checkEndpoint validates an endpoint without DNS or network access, and reports
// whether it targets a literal local or private host. Plain HTTP to a remote name
// is refused even if that name might resolve to a private address. Messages never
// quote the URL, which may carry a token.
func checkEndpoint(raw string, allowQuery bool) (local bool, problem string) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Opaque != "" || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return false, "must be an absolute http or https URL"
	}
	if parsed.User != nil {
		return false, "must not contain credentials"
	}
	if parsed.Fragment != "" || (!allowQuery && parsed.RawQuery != "") {
		return false, "must not contain a query or fragment"
	}
	if ambiguousPath(parsed.EscapedPath()) {
		return false, "must not contain dot segments, backslashes or encoded separators in the path"
	}
	local = isLocalHost(parsed.Hostname())
	if parsed.Scheme == "http" && !local {
		return false, "plain http is only allowed for localhost, loopback or private IP literals"
	}
	return local, ""
}

// ambiguousPath refuses a path that an intermediary could read differently than the
// gateway does, since request paths are appended to it.
func ambiguousPath(path string) bool {
	lower := strings.ToLower(path)
	if strings.Contains(lower, "%2f") || strings.Contains(lower, "%5c") || strings.Contains(lower, "\\") {
		return true
	}
	for segment := range strings.SplitSeq(strings.ReplaceAll(lower, "%2e", "."), "/") {
		if segment == "." || segment == ".." {
			return true
		}
	}
	return false
}

func isLocalHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	return addr.IsLoopback() || addr.IsPrivate()
}

func validatePricing(cfg *Config, owners map[string]string, d *decoder) {
	for _, model := range slices.Sorted(maps.Keys(cfg.Pricing)) {
		if _, declared := owners[model]; !declared {
			d.reportAt(CodeInvalidReference, joinPath("pricing", model), "model "+quoteText(model)+" is not declared by any provider")
		}
	}
}

func validateApp(app *App, path string, cfg *Config, owners map[string]string, d *decoder, opts Options, warnings *diagnosticList) {
	if app.Timezone == "" {
		if _, declared := d.positions[joinPath(path, "timezone")]; !declared {
			app.Timezone = cfg.Timezone
		}
	} else if !validTimezone(app.Timezone) {
		d.reportAt(CodeInvalidValue, joinPath(path, "timezone"), "must be a valid IANA timezone name")
	}
	if app.FailMode == "" {
		app.FailMode = cfg.FailMode
	}

	allowedPath := joinPath(path, "allowed_models")
	for i, model := range app.AllowedModels {
		if _, declared := owners[model]; !declared {
			d.reportAt(CodeInvalidReference, indexPath(allowedPath, i), "model "+quoteText(model)+" is not declared by any provider")
		}
	}

	validateDowngrade(app, path, cfg, owners, d, opts)
	validateFallback(app, path, cfg, owners, d, warnings)

	derive := app.OnLimit == OnLimitDowngrade
	validateHardCap(app.Budget, &app.HardCap, joinPath(path, "hard_cap"), derive, d)
	validateHardCap(app.PerUser.Budget, &app.PerUser.HardCap, joinPath(joinPath(path, "per_user"), "hard_cap"), derive, d)
}

func validateDowngrade(app *App, path string, cfg *Config, owners map[string]string, d *decoder, opts Options) {
	targetPath := joinPath(path, "downgrade_to")
	if app.OnLimit == OnLimitDowngrade {
		if app.DowngradeTo == "" {
			if _, declared := d.positions[targetPath]; !declared {
				d.reportAt(CodeInvalidReference, joinPath(path, "on_limit"), "downgrade requires downgrade_to")
			}
		}
		if app.Budget.isZero() && app.PerUser.Budget.isZero() {
			d.reportAt(CodeInvalidValue, joinPath(path, "on_limit"), "downgrade requires a budget on the app or per user")
		}
	}
	if app.DowngradeTo == "" {
		return
	}
	target := quoteText(app.DowngradeTo)
	owner, declared := owners[app.DowngradeTo]
	if !declared {
		d.reportAt(CodeInvalidReference, targetPath, "model "+target+" is not declared by any provider")
		return
	}
	if !slices.Contains(app.AllowedModels, app.DowngradeTo) {
		d.reportAt(CodeInvalidReference, targetPath, "model "+target+" is not in allowed_models")
	}
	if app.OnLimit != OnLimitDowngrade {
		return
	}
	if _, overridden := cfg.Pricing[app.DowngradeTo]; overridden || cfg.Providers[owner].Local {
		return
	}
	switch {
	case opts.Catalog == nil:
		d.reportAt(CodeInvalidReference, targetPath, "price of model "+target+" cannot be verified; add a pricing override")
	case !opts.Catalog.HasPrice(app.DowngradeTo):
		d.reportAt(CodeInvalidReference, targetPath, "model "+target+" has no known price")
	}
}

func validateFallback(app *App, path string, cfg *Config, owners map[string]string, d *decoder, warnings *diagnosticList) {
	chainPath := joinPath(path, "fallback")
	if len(app.Fallback) > 4 {
		d.reportAt(CodeInvalidValue, chainPath, "must have at most 4 entries")
	}
	formats := map[APIFormat]bool{}
	for _, model := range app.AllowedModels {
		if owner, declared := owners[model]; declared {
			formats[cfg.Providers[owner].APIFormat] = true
		}
	}
	for i, model := range app.Fallback {
		owner, declared := owners[model]
		switch {
		case !declared:
			d.reportAt(CodeInvalidReference, indexPath(chainPath, i), "model "+quoteText(model)+" is not declared by any provider")
		case !slices.Contains(app.AllowedModels, model):
			d.reportAt(CodeInvalidReference, indexPath(chainPath, i), "model "+quoteText(model)+" is not in allowed_models")
		default:
			formats[cfg.Providers[owner].APIFormat] = true
		}
	}
	if len(app.Fallback) > 0 && len(formats) > 1 {
		position := d.positions[chainPath]
		warnings.add(Diagnostic{
			Code: CodeFallbackMixedFormat, Path: chainPath, Line: position.Line, Column: position.Column,
			Message: "fallback chain mixes API formats; entries in another format are not eligible until cross-provider translation exists",
		})
	}
}

func validateHardCap(budget Budget, hardCap *Budget, path string, derive bool, d *decoder) {
	checkInt := func(key string, budgetValue int64, capValue *int64) {
		capPath := joinPath(path, key)
		switch {
		case *capValue > 0 && budgetValue == 0:
			d.reportAt(CodeInvalidValue, capPath, "has no matching budget")
		case *capValue > 0 && *capValue < budgetValue:
			d.reportAt(CodeInvalidValue, capPath, "must be greater than or equal to the budget")
		case *capValue == 0 && budgetValue > 0 && derive:
			derived, ok := downgradeHardCap(budgetValue)
			if !ok {
				d.diags.add(Diagnostic{Code: CodeInvalidValue, Path: capPath, Message: "derived hard cap exceeds the supported range"})
				return
			}
			*capValue = derived
		}
	}
	usdPerDay, usdPerMonth := int64(hardCap.USDPerDay), int64(hardCap.USDPerMonth)
	checkInt("usd_per_day", int64(budget.USDPerDay), &usdPerDay)
	checkInt("usd_per_month", int64(budget.USDPerMonth), &usdPerMonth)
	hardCap.USDPerDay, hardCap.USDPerMonth = Microusd(usdPerDay), Microusd(usdPerMonth)
	checkInt("tokens_per_day", budget.TokensPerDay, &hardCap.TokensPerDay)
	checkInt("tokens_per_month", budget.TokensPerMonth, &hardCap.TokensPerMonth)
}

func validateAlerts(cfg *Config, d *decoder) {
	for _, name := range slices.Sorted(maps.Keys(cfg.Alerts.Channels)) {
		channel := cfg.Alerts.Channels[name]
		if channel.Type != AlertNtfy || channel.ServerURL == "" {
			continue
		}
		path := joinPath(joinPath("alerts.channels", name), "server_url")
		if _, problem := checkEndpoint(channel.ServerURL, false); problem != "" {
			d.reportAt(CodeInvalidValue, path, problem)
		}
	}
}

func validateTelemetry(telemetry *Telemetry, d *decoder) {
	const path = "telemetry.otel_endpoint"
	if telemetry.OTelEndpoint != "" {
		parsed, err := url.Parse(telemetry.OTelEndpoint)
		switch {
		case err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https"):
			d.reportAt(CodeInvalidValue, path, "must be an absolute http or https URL")
		case parsed.User != nil:
			d.reportAt(CodeInvalidValue, path, "must not contain credentials")
		case parsed.RawQuery != "" || parsed.Fragment != "":
			d.reportAt(CodeInvalidValue, path, "must not contain a query or fragment")
		}
	}
	if telemetry.OTelEnabled && telemetry.OTelEndpoint == "" {
		if _, declared := d.positions[path]; !declared {
			d.reportAt(CodeInvalidValue, "telemetry.otel_enabled", "otel_endpoint is required when OpenTelemetry is enabled")
		}
	}
}
