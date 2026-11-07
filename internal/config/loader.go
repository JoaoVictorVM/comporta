package config

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"go.yaml.in/yaml/v3"
)

// FileSystem is the read-only file access the loader needs.
type FileSystem interface {
	Open(name string) (io.ReadCloser, error)
	Stat(name string) (fs.FileInfo, error)
}

type osFileSystem struct{}

func (osFileSystem) Open(name string) (io.ReadCloser, error) { return os.Open(name) }
func (osFileSystem) Stat(name string) (fs.FileInfo, error)   { return os.Stat(name) }

// PriceCatalog reports whether a model has a built-in price. Overrides in the
// configuration are consulted first.
type PriceCatalog interface {
	HasPrice(model string) bool
}

// Options controls one load. The zero value performs structural validation only.
type Options struct {
	// ResolveSecrets also reads environment references and secret files.
	ResolveSecrets bool
	Environment    *Environment
	// SystemTimezone is the default timezone. Empty means the system has no
	// usable IANA identifier, which makes an explicit timezone mandatory.
	SystemTimezone string
	Catalog        PriceCatalog
	FS             FileSystem
}

func (o Options) fileSystem() FileSystem {
	if o.FS != nil {
		return o.FS
	}
	return osFileSystem{}
}

// Position locates a value in the source file.
type Position struct {
	Line   int
	Column int
}

// Candidate is a fully validated configuration that has not been published.
type Candidate struct {
	cfg       Config
	secrets   map[string]Secret
	warnings  []Diagnostic
	positions map[string]Position
}

// Warnings returns the non-fatal diagnostics found while loading.
func (c *Candidate) Warnings() []Diagnostic {
	out := make([]Diagnostic, len(c.warnings))
	copy(out, c.warnings)
	return out
}

// DataDir is the absolute data directory.
func (c *Candidate) DataDir() string { return c.cfg.DataDir }

// AppNames lists apps in sorted order.
func (c *Candidate) AppNames() []string { return slices.Sorted(maps.Keys(c.cfg.Apps)) }

// HasApp reports whether an app with exactly this name is defined.
func (c *Candidate) HasApp(name string) bool {
	_, ok := c.cfg.Apps[name]
	return ok
}

// Load reads and validates the file at path. A failure is always an *Error.
func Load(path string, opts Options) (*Candidate, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, singleError(Diagnostic{Code: CodeConfigUnreadable, Message: "configuration path cannot be resolved"})
	}
	data, err := readConfigFile(opts.fileSystem(), abs)
	if err != nil {
		return nil, err
	}
	return LoadBytes(data, abs, opts)
}

func singleError(d Diagnostic) error { return &Error{Diagnostics: []Diagnostic{d}} }

// readConfigFile reads one byte past the limit so an oversized file is detected
// without loading it entirely.
func readConfigFile(fsys FileSystem, path string) ([]byte, error) {
	data, err := readLimited(fsys, path, MaxConfigBytes+1)
	if err := checkConfigRead(data, err); err != nil {
		return nil, err
	}
	return data, nil
}

func checkConfigRead(data []byte, err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return singleError(Diagnostic{Code: CodeConfigMissing, Message: "configuration file not found"})
	case err != nil:
		return singleError(Diagnostic{Code: CodeConfigUnreadable, Message: "configuration file cannot be read: " + classifyIOError(err)})
	case len(data) > MaxConfigBytes:
		return tooLargeError()
	}
	return nil
}

func tooLargeError() error {
	return singleError(Diagnostic{Code: CodeConfigTooLarge, Message: "configuration exceeds " + strconv.Itoa(MaxConfigBytes) + " byte limit"})
}

func readLimited(fsys FileSystem, path string, limit int64) ([]byte, error) {
	file, err := fsys.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	return io.ReadAll(io.LimitReader(file, limit))
}

// classifyIOError reduces an I/O failure to a fixed phrase, so operating system
// text never reaches diagnostics.
func classifyIOError(err error) string {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "no such file"
	case errors.Is(err, fs.ErrPermission):
		return "permission denied"
	default:
		return "read error"
	}
}

// LoadBytes validates configuration bytes as if read from path, whose directory
// anchors relative references. A failure is always an *Error.
func LoadBytes(data []byte, path string, opts Options) (*Candidate, error) {
	if len(data) > MaxConfigBytes {
		return nil, tooLargeError()
	}
	diags := &diagnosticList{}
	root := parseRestricted(data, diags)
	if !diags.empty() {
		return nil, diags.err()
	}

	dec := &decoder{diags: diags, positions: map[string]Position{}}
	if root != nil {
		dec.checkValues(root, "", "")
	}
	cfg := dec.decodeRoot(root)
	baseDir := filepath.Dir(path)
	warnings := validateConfig(&cfg, dec, baseDir, opts)

	var secrets map[string]Secret
	if opts.ResolveSecrets {
		secrets = resolveSecrets(&cfg, dec, baseDir, opts)
	}
	if err := diags.err(); err != nil {
		return nil, err
	}
	return &Candidate{cfg: cfg, secrets: secrets, warnings: warnings, positions: dec.positions}, nil
}

var yamlLinePattern = regexp.MustCompile(`line (\d+)`)

// parseRestricted builds the YAML tree and rejects every feature outside the
// supported format. Parser messages may quote the input, so only the line number
// is taken from them.
func parseRestricted(data []byte, diags *diagnosticList) (root *yaml.Node) {
	defer func() {
		if recover() != nil {
			root = nil
			diags.add(Diagnostic{Code: CodeInvalidYAML, Message: "invalid YAML syntax"})
		}
	}()

	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := decoder.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}
		diags.add(syntaxDiagnostic(err))
		return nil
	}
	var extra yaml.Node
	switch err := decoder.Decode(&extra); {
	case err == nil:
		diags.add(Diagnostic{Code: CodeInvalidYAML, Line: extra.Line, Column: extra.Column, Message: "multiple YAML documents are not supported"})
		return nil
	case !errors.Is(err, io.EOF):
		diags.add(syntaxDiagnostic(err))
		return nil
	}
	if len(doc.Content) == 0 {
		return nil
	}
	root = doc.Content[0]
	checkRestricted(root, "", 0, diags)
	return root
}

func syntaxDiagnostic(err error) Diagnostic {
	d := Diagnostic{Code: CodeInvalidYAML, Message: "invalid YAML syntax"}
	if match := yamlLinePattern.FindStringSubmatch(err.Error()); match != nil {
		if line, convErr := strconv.Atoi(match[1]); convErr == nil {
			d.Line = line
			d.Column = 1
		}
	}
	return d
}

const maxYAMLDepth = 32

func checkRestricted(node *yaml.Node, path string, depth int, diags *diagnosticList) {
	report := func(message string) {
		diags.add(Diagnostic{Code: CodeInvalidYAML, Path: path, Line: node.Line, Column: node.Column, Message: message})
	}
	if depth > maxYAMLDepth {
		report("nesting is too deep")
		return
	}
	switch {
	case node.Kind == yaml.AliasNode:
		report("aliases are not supported")
		return
	case node.Anchor != "":
		report("anchors are not supported")
	case node.Style&yaml.TaggedStyle != 0:
		report("explicit tags are not supported")
	}
	switch node.Kind {
	case yaml.MappingNode:
		seen := make(map[string]bool, len(node.Content)/2)
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			switch {
			case key.Tag == "!!merge":
				diags.add(Diagnostic{Code: CodeInvalidYAML, Path: path, Line: key.Line, Column: key.Column, Message: "merge keys are not supported"})
				continue
			case key.Kind != yaml.ScalarNode || key.Tag != "!!str" || key.Anchor != "" || key.Style&yaml.TaggedStyle != 0:
				diags.add(Diagnostic{Code: CodeInvalidYAML, Path: path, Line: key.Line, Column: key.Column, Message: "mapping keys must be plain strings"})
				continue
			}
			child := joinPath(path, key.Value)
			if seen[key.Value] {
				diags.add(Diagnostic{Code: CodeInvalidYAML, Path: child, Line: key.Line, Column: key.Column, Message: "duplicate key"})
				continue
			}
			seen[key.Value] = true
			checkRestricted(value, child, depth+1, diags)
		}
	case yaml.SequenceNode:
		for i, child := range node.Content {
			checkRestricted(child, indexPath(path, i), depth+1, diags)
		}
	}
}

// decoder converts the restricted tree into typed values, recording a problem
// for each invalid field and keeping the default for it.
type decoder struct {
	diags     *diagnosticList
	positions map[string]Position
}

func (d *decoder) report(code Code, path string, node *yaml.Node, message string) {
	diagnostic := Diagnostic{Code: code, Path: path, Message: message}
	if node != nil {
		diagnostic.Line, diagnostic.Column = node.Line, node.Column
	}
	d.diags.add(diagnostic)
}

// reportAt reports at a previously decoded path, reusing its source position.
func (d *decoder) reportAt(code Code, path, message string) {
	position := d.positions[path]
	d.diags.add(Diagnostic{Code: code, Path: path, Line: position.Line, Column: position.Column, Message: message})
}

func isNull(node *yaml.Node) bool {
	return node.Kind == yaml.ScalarNode && node.Tag == "!!null"
}

// checkValues enforces the rules that apply wherever a field appears, including
// fields the typed contract does not know: a literal under any api_key must be
// refused even when the surrounding field is itself unknown.
func (d *decoder) checkValues(node *yaml.Node, path, parentName string) {
	switch node.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			child := joinPath(path, key.Value)
			if key.Value == "api_key" && value.Kind == yaml.ScalarNode && !isNull(value) {
				if !isEnvReference(value) {
					d.report(CodeLiteralSecret, child, value, literalSecretMessage(parentName, "API_KEY", "api_key_file"))
				}
			}
			d.checkValues(value, child, key.Value)
		}
	case yaml.SequenceNode:
		for i, item := range node.Content {
			d.checkValues(item, indexPath(path, i), "")
		}
	case yaml.ScalarNode:
		if isNull(node) {
			d.report(CodeInvalidValue, path, node, "null is not allowed")
		}
	}
}

func isEnvReference(node *yaml.Node) bool {
	return node.Tag == "!!str" && envRefPattern.MatchString(node.Value)
}

// literalSecretMessage suggests a variable derived from a validated name, never
// from the rejected value.
func literalSecretMessage(name, suffix, fileField string) string {
	variable := suffix
	if namePattern.MatchString(name) {
		variable = strings.ToUpper(strings.ReplaceAll(name, "-", "_")) + "_" + suffix
		if !envRefPattern.MatchString("${" + variable + "}") {
			variable = suffix
		}
	}
	return "contains a literal secret; use ${" + variable + "} or " + fileField
}

type fieldSet map[string]*yaml.Node

// fields validates a mapping against its allowed keys and returns those present.
func (d *decoder) fields(node *yaml.Node, path string, allowed ...string) fieldSet {
	out := fieldSet{}
	if !d.isMapping(node, path) {
		return out
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		child := joinPath(path, key.Value)
		if !containsString(allowed, key.Value) {
			d.report(CodeUnknownField, child, key, "unknown field")
			continue
		}
		d.positions[child] = Position{Line: value.Line, Column: value.Column}
		out[key.Value] = value
	}
	return out
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func (d *decoder) isMapping(node *yaml.Node, path string) bool {
	if isNull(node) {
		return false
	}
	if node.Kind != yaml.MappingNode {
		d.report(CodeInvalidValue, path, node, "must be a mapping")
		return false
	}
	return true
}

// named iterates a mapping whose keys are data, such as provider names.
func (d *decoder) named(node *yaml.Node, path string, visit func(name string, value *yaml.Node, child string)) {
	if !d.isMapping(node, path) {
		return
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		key, value := node.Content[i], node.Content[i+1]
		child := joinPath(path, key.Value)
		d.positions[child] = Position{Line: value.Line, Column: value.Column}
		visit(key.Value, value, child)
	}
}

func (d *decoder) scalar(node *yaml.Node, path, tag, message string) bool {
	if isNull(node) {
		return false
	}
	if node.Kind != yaml.ScalarNode || node.Tag != tag {
		d.report(CodeInvalidValue, path, node, message)
		return false
	}
	return true
}

func (d *decoder) text(node *yaml.Node, path string) (string, bool) {
	if !d.scalar(node, path, "!!str", "must be a string") {
		return "", false
	}
	if node.Value == "" {
		d.report(CodeInvalidValue, path, node, "must not be empty")
		return "", false
	}
	for _, r := range node.Value {
		if unicode.IsControl(r) {
			d.report(CodeInvalidValue, path, node, "must not contain control characters")
			return "", false
		}
	}
	return node.Value, true
}

func (d *decoder) optText(fields fieldSet, key, parent string, target *string) {
	if node, ok := fields[key]; ok {
		if value, valid := d.text(node, joinPath(parent, key)); valid {
			*target = value
		}
	}
}

func (d *decoder) optBool(fields fieldSet, key, parent string, target *bool) {
	node, ok := fields[key]
	if !ok {
		return
	}
	if d.scalar(node, joinPath(parent, key), "!!bool", "must be true or false") {
		var value bool
		if err := node.Decode(&value); err == nil {
			*target = value
		}
	}
}

var decimalIntegerPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)$`)

func (d *decoder) integer(node *yaml.Node, path string, minimum, maximum int64) (int64, bool) {
	if !d.scalar(node, path, "!!int", "must be a decimal integer") {
		return 0, false
	}
	if !decimalIntegerPattern.MatchString(node.Value) {
		d.report(CodeInvalidValue, path, node, "must be a non-negative decimal integer")
		return 0, false
	}
	value, err := strconv.ParseInt(node.Value, 10, 64)
	if err != nil {
		d.report(CodeInvalidValue, path, node, "exceeds the supported integer range")
		return 0, false
	}
	if value < minimum || value > maximum {
		message := "must be between " + strconv.FormatInt(minimum, 10) + " and " + strconv.FormatInt(maximum, 10)
		if maximum == math.MaxInt64 {
			message = "must be greater than zero"
		}
		d.report(CodeInvalidValue, path, node, message)
		return 0, false
	}
	return value, true
}

func (d *decoder) optPositive(fields fieldSet, key, parent string, target *int64) {
	d.optRange(fields, key, parent, 1, math.MaxInt64, target)
}

func (d *decoder) optRange(fields fieldSet, key, parent string, minimum, maximum int64, target *int64) {
	if node, ok := fields[key]; ok {
		if value, valid := d.integer(node, joinPath(parent, key), minimum, maximum); valid {
			*target = value
		}
	}
}

func (d *decoder) optDuration(fields fieldSet, key, parent string, target *time.Duration) {
	node, ok := fields[key]
	if !ok {
		return
	}
	path := joinPath(parent, key)
	if !d.scalar(node, path, "!!str", "must be a duration string such as 30s") {
		return
	}
	value, err := time.ParseDuration(node.Value)
	if err != nil {
		d.report(CodeInvalidValue, path, node, "must be a duration string such as 30s")
		return
	}
	if value <= 0 {
		d.report(CodeInvalidValue, path, node, "must be greater than zero")
		return
	}
	*target = value
}

// optMoney reads the decimal lexeme directly. YAML typing is checked only to
// refuse quoted strings; the value itself never passes through a float.
func (d *decoder) optMoney(fields fieldSet, key, parent string, allowZero bool, target *Microusd) {
	node, ok := fields[key]
	if !ok {
		return
	}
	path := joinPath(parent, key)
	if isNull(node) {
		return
	}
	if node.Kind != yaml.ScalarNode || (node.Tag != "!!int" && node.Tag != "!!float") {
		d.report(CodeInvalidValue, path, node, errMoneyFormat.Error())
		return
	}
	value, err := ParseMoney(node.Value)
	if err != nil {
		d.report(CodeInvalidValue, path, node, err.Error())
		return
	}
	if value == 0 && !allowZero {
		d.report(CodeInvalidValue, path, node, "must be greater than zero")
		return
	}
	*target = value
}

func optEnum[T ~string](d *decoder, fields fieldSet, key, parent string, code Code, target *T, allowed ...T) {
	node, ok := fields[key]
	if !ok {
		return
	}
	path := joinPath(parent, key)
	names := make([]string, len(allowed))
	for i, value := range allowed {
		names[i] = string(value)
	}
	message := "must be one of " + strings.Join(names, ", ")
	if !d.scalar(node, path, "!!str", message) {
		return
	}
	for _, value := range allowed {
		if node.Value == string(value) {
			*target = value
			return
		}
	}
	d.report(code, path, node, message)
}

// stringList rejects empty entries, control characters and duplicates.
func (d *decoder) stringList(node *yaml.Node, path string) []string {
	if isNull(node) {
		return nil
	}
	if node.Kind != yaml.SequenceNode {
		d.report(CodeInvalidValue, path, node, "must be a list")
		return nil
	}
	out := make([]string, 0, len(node.Content))
	seen := make(map[string]bool, len(node.Content))
	for i, item := range node.Content {
		itemPath := indexPath(path, i)
		d.positions[itemPath] = Position{Line: item.Line, Column: item.Column}
		value, ok := d.text(item, itemPath)
		if !ok {
			continue
		}
		if seen[value] {
			d.report(CodeInvalidValue, itemPath, item, "duplicate entry "+quoteText(value))
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

// secretRef reads an exclusive pair of secret source fields. Whether the field
// was present is returned separately, because a rejected literal must not also
// be reported as a missing secret.
func (d *decoder) secretRef(fields fieldSet, parent, name, envKey, fileKey string) (SecretRef, bool) {
	var ref SecretRef
	envNode, hasEnv := fields[envKey]
	fileNode, hasFile := fields[fileKey]
	if hasEnv && hasFile {
		d.report(CodeInvalidValue, parent, envNode, "use only one of "+envKey+" or "+fileKey)
		return ref, true
	}
	if hasEnv {
		path := joinPath(parent, envKey)
		switch {
		case isNull(envNode):
		case envNode.Kind != yaml.ScalarNode:
			d.report(CodeInvalidValue, path, envNode, "must be a string")
		case isEnvReference(envNode):
			ref.Env = envRefPattern.FindStringSubmatch(envNode.Value)[1]
		case envKey != "api_key":
			d.report(CodeLiteralSecret, path, envNode, literalSecretMessage(name, strings.ToUpper(envKey), fileKey))
		}
	}
	if hasFile {
		if value, ok := d.text(fileNode, joinPath(parent, fileKey)); ok {
			ref.File = value
		}
	}
	return ref, hasEnv || hasFile
}

func (d *decoder) decodeRoot(root *yaml.Node) Config {
	cfg := defaultConfig()
	if root == nil {
		return cfg
	}
	fields := d.fields(root, "", "data_dir", "timezone", "fail_mode", "providers", "apps", "listeners", "pricing", "cache", "alerts", "telemetry", "storage")
	d.optText(fields, "data_dir", "", &cfg.DataDir)
	d.optText(fields, "timezone", "", &cfg.Timezone)
	optEnum(d, fields, "fail_mode", "", CodeInvalidValue, &cfg.FailMode, FailClosed, FailOpen)
	if node, ok := fields["listeners"]; ok {
		d.decodeListeners(node, &cfg.Listeners)
	}
	if node, ok := fields["providers"]; ok {
		d.named(node, "providers", func(name string, value *yaml.Node, path string) {
			d.checkName(name, path, value)
			cfg.Providers[name] = d.decodeProvider(value, path, name)
		})
	}
	if node, ok := fields["apps"]; ok {
		d.named(node, "apps", func(name string, value *yaml.Node, path string) {
			d.checkName(name, path, value)
			cfg.Apps[name] = d.decodeApp(value, path)
		})
	}
	if node, ok := fields["pricing"]; ok {
		d.named(node, "pricing", func(name string, value *yaml.Node, path string) {
			cfg.Pricing[name] = d.decodePrice(value, path)
		})
	}
	if node, ok := fields["storage"]; ok {
		storage := d.fields(node, "storage", "retention_days")
		d.optRange(storage, "retention_days", "storage", 1, 365, &cfg.Storage.RetentionDays)
	}
	if node, ok := fields["cache"]; ok {
		cache := d.fields(node, "cache", "max_bytes")
		d.optPositive(cache, "max_bytes", "cache", &cfg.Cache.MaxBytes)
	}
	if node, ok := fields["alerts"]; ok {
		d.decodeAlerts(node, &cfg.Alerts)
	}
	if node, ok := fields["telemetry"]; ok {
		telemetry := d.fields(node, "telemetry", "metrics_enabled", "otel_enabled", "otel_endpoint")
		d.optBool(telemetry, "metrics_enabled", "telemetry", &cfg.Telemetry.MetricsEnabled)
		d.optBool(telemetry, "otel_enabled", "telemetry", &cfg.Telemetry.OTelEnabled)
		d.optText(telemetry, "otel_endpoint", "telemetry", &cfg.Telemetry.OTelEndpoint)
	}
	return cfg
}

func (d *decoder) checkName(name, path string, node *yaml.Node) {
	if !namePattern.MatchString(name) {
		d.report(CodeInvalidValue, path, node, "name must match "+namePattern.String())
	}
}

func (d *decoder) decodeListeners(node *yaml.Node, out *Listeners) {
	const parent = "listeners"
	fields := d.fields(node, parent, "proxy", "admin", "allowed_hosts", "max_body_bytes",
		"read_header_timeout", "non_streaming_timeout", "streaming_idle_timeout", "upstream_header_timeout")
	decodeListener := func(key string, target *Listener) {
		value, ok := fields[key]
		if !ok {
			return
		}
		path := joinPath(parent, key)
		listener := d.fields(value, path, "bind", "port")
		d.optText(listener, "bind", path, &target.Bind)
		d.optRange(listener, "port", path, 1, 65535, &target.Port)
	}
	decodeListener("proxy", &out.Proxy)
	decodeListener("admin", &out.Admin)
	if value, ok := fields["allowed_hosts"]; ok {
		out.AllowedHosts = d.stringList(value, joinPath(parent, "allowed_hosts"))
	}
	d.optPositive(fields, "max_body_bytes", parent, &out.MaxBodyBytes)
	d.optDuration(fields, "read_header_timeout", parent, &out.ReadHeaderTimeout)
	d.optDuration(fields, "non_streaming_timeout", parent, &out.NonStreamingTimeout)
	d.optDuration(fields, "streaming_idle_timeout", parent, &out.StreamingIdleTimeout)
	d.optDuration(fields, "upstream_header_timeout", parent, &out.UpstreamHeaderTimeout)
}

func (d *decoder) decodeProvider(node *yaml.Node, path, name string) Provider {
	out := Provider{TLSMinVersion: "1.2"}
	fields := d.fields(node, path, "api_format", "base_url", "api_key", "api_key_file", "models", "tls_min_version")
	if !d.requireFields(node, fields, path, "api_format", "base_url", "models") {
		return out
	}
	optEnum(d, fields, "api_format", path, CodeInvalidValue, &out.APIFormat, FormatAnthropic, FormatOpenAI)
	d.optText(fields, "base_url", path, &out.BaseURL)
	out.Models = d.stringList(fields["models"], joinPath(path, "models"))
	if len(fields["models"].Content) == 0 && !isNull(fields["models"]) {
		d.report(CodeInvalidValue, joinPath(path, "models"), fields["models"], "must not be empty")
	}
	optEnum(d, fields, "tls_min_version", path, CodeInvalidValue, &out.TLSMinVersion, "1.2", "1.3")
	out.APIKey, _ = d.secretRef(fields, path, name, "api_key", "api_key_file")
	return out
}

// requireFields reports each absent mandatory field, returning whether all are
// present. A mapping of the wrong kind has already been reported.
func (d *decoder) requireFields(node *yaml.Node, fields fieldSet, path string, required ...string) bool {
	if node.Kind != yaml.MappingNode {
		return false
	}
	complete := true
	for _, key := range required {
		if _, ok := fields[key]; !ok {
			d.report(CodeInvalidValue, joinPath(path, key), node, "is required")
			complete = false
		}
	}
	return complete
}

func (d *decoder) decodeLimits(node *yaml.Node, path string, out *Limits) {
	fields := d.fields(node, path, "requests_per_minute", "tokens_per_minute", "max_concurrent")
	d.optPositive(fields, "requests_per_minute", path, &out.RequestsPerMinute)
	d.optPositive(fields, "tokens_per_minute", path, &out.TokensPerMinute)
	d.optPositive(fields, "max_concurrent", path, &out.MaxConcurrent)
}

func (d *decoder) decodeBudget(node *yaml.Node, path string, out *Budget) {
	fields := d.fields(node, path, "usd_per_day", "usd_per_month", "tokens_per_day", "tokens_per_month")
	d.optMoney(fields, "usd_per_day", path, false, &out.USDPerDay)
	d.optMoney(fields, "usd_per_month", path, false, &out.USDPerMonth)
	d.optPositive(fields, "tokens_per_day", path, &out.TokensPerDay)
	d.optPositive(fields, "tokens_per_month", path, &out.TokensPerMonth)
}

func (d *decoder) decodeApp(node *yaml.Node, path string) App {
	out := defaultApp()
	fields := d.fields(node, path, "require_user", "allowed_models", "max_tokens", "on_limit", "downgrade_to",
		"timezone", "fail_mode", "limits", "budget", "hard_cap", "per_user", "loop_guard", "fallback", "cache", "detection")
	if d.requireFields(node, fields, path, "allowed_models") {
		out.AllowedModels = d.stringList(fields["allowed_models"], joinPath(path, "allowed_models"))
		if len(fields["allowed_models"].Content) == 0 && !isNull(fields["allowed_models"]) {
			d.report(CodeInvalidValue, joinPath(path, "allowed_models"), fields["allowed_models"], "must not be empty")
		}
	}
	d.optBool(fields, "require_user", path, &out.RequireUser)
	d.optPositive(fields, "max_tokens", path, &out.MaxTokens)
	optEnum(d, fields, "on_limit", path, CodeInvalidPolicy, &out.OnLimit, OnLimitBlock, OnLimitDowngrade, OnLimitWarn)
	d.optText(fields, "downgrade_to", path, &out.DowngradeTo)
	d.optText(fields, "timezone", path, &out.Timezone)
	optEnum(d, fields, "fail_mode", path, CodeInvalidValue, &out.FailMode, FailClosed, FailOpen)
	if value, ok := fields["limits"]; ok {
		d.decodeLimits(value, joinPath(path, "limits"), &out.Limits)
	}
	if value, ok := fields["budget"]; ok {
		d.decodeBudget(value, joinPath(path, "budget"), &out.Budget)
	}
	if value, ok := fields["hard_cap"]; ok {
		d.decodeBudget(value, joinPath(path, "hard_cap"), &out.HardCap)
	}
	if value, ok := fields["per_user"]; ok {
		perUserPath := joinPath(path, "per_user")
		perUser := d.fields(value, perUserPath, "limits", "budget", "hard_cap")
		if limits, ok := perUser["limits"]; ok {
			d.decodeLimits(limits, joinPath(perUserPath, "limits"), &out.PerUser.Limits)
		}
		if budget, ok := perUser["budget"]; ok {
			d.decodeBudget(budget, joinPath(perUserPath, "budget"), &out.PerUser.Budget)
		}
		if hardCap, ok := perUser["hard_cap"]; ok {
			d.decodeBudget(hardCap, joinPath(perUserPath, "hard_cap"), &out.PerUser.HardCap)
		}
	}
	if value, ok := fields["loop_guard"]; ok {
		loopPath := joinPath(path, "loop_guard")
		loop := d.fields(value, loopPath, "enabled", "similar_requests", "similar_window", "requests_per_minute", "cooldown")
		d.optBool(loop, "enabled", loopPath, &out.LoopGuard.Enabled)
		d.optPositive(loop, "similar_requests", loopPath, &out.LoopGuard.SimilarRequests)
		d.optDuration(loop, "similar_window", loopPath, &out.LoopGuard.SimilarWindow)
		d.optPositive(loop, "requests_per_minute", loopPath, &out.LoopGuard.RequestsPerMinute)
		d.optDuration(loop, "cooldown", loopPath, &out.LoopGuard.Cooldown)
	}
	if value, ok := fields["fallback"]; ok {
		out.Fallback = d.stringList(value, joinPath(path, "fallback"))
	}
	if value, ok := fields["cache"]; ok {
		cachePath := joinPath(path, "cache")
		cache := d.fields(value, cachePath, "enabled", "ttl_seconds", "shared_across_users")
		d.optBool(cache, "enabled", cachePath, &out.Cache.Enabled)
		d.optRange(cache, "ttl_seconds", cachePath, 1, 3600, &out.Cache.TTLSeconds)
		d.optBool(cache, "shared_across_users", cachePath, &out.Cache.SharedAcrossUsers)
	}
	if value, ok := fields["detection"]; ok {
		d.decodeDetection(value, joinPath(path, "detection"), &out.Detection)
	}
	return out
}

func (d *decoder) decodeDetection(node *yaml.Node, path string, out *Detection) {
	fields := d.fields(node, path, "cpf", "cnpj", "card", "email", "phone", "pix", "secret", "rehydrate")
	modes := map[string]*DetectionMode{
		"cpf": &out.CPF, "cnpj": &out.CNPJ, "card": &out.Card, "email": &out.Email,
		"phone": &out.Phone, "pix": &out.Pix, "secret": &out.Secret,
	}
	for key, target := range modes {
		optEnum(d, fields, key, path, CodeInvalidValue, target, DetectionOff, DetectionDetect, DetectionRedact, DetectionBlock)
	}
	d.optBool(fields, "rehydrate", path, &out.Rehydrate)
}

func (d *decoder) decodePrice(node *yaml.Node, path string) Price {
	var out Price
	fields := d.fields(node, path, "input", "output", "cache_write", "cache_read")
	d.requireFields(node, fields, path, "input", "output", "cache_write", "cache_read")
	d.optMoney(fields, "input", path, true, &out.Input)
	d.optMoney(fields, "output", path, true, &out.Output)
	d.optMoney(fields, "cache_write", path, true, &out.CacheWrite)
	d.optMoney(fields, "cache_read", path, true, &out.CacheRead)
	return out
}

func (d *decoder) decodeAlerts(node *yaml.Node, out *Alerts) {
	const parent = "alerts"
	fields := d.fields(node, parent, "channels", "thresholds", "anomaly_multiplier", "anomaly_min_usd")
	if value, ok := fields["channels"]; ok {
		d.named(value, joinPath(parent, "channels"), func(name string, channel *yaml.Node, path string) {
			d.checkName(name, path, channel)
			out.Channels[name] = d.decodeChannel(channel, path, name)
		})
	}
	if value, ok := fields["thresholds"]; ok {
		out.Thresholds = d.decodeThresholds(value, joinPath(parent, "thresholds"))
	}
	d.optPositive(fields, "anomaly_multiplier", parent, &out.AnomalyMultiplier)
	d.optMoney(fields, "anomaly_min_usd", parent, true, &out.AnomalyMinUSD)
}

func (d *decoder) decodeThresholds(node *yaml.Node, path string) []int64 {
	if isNull(node) {
		return nil
	}
	if node.Kind != yaml.SequenceNode {
		d.report(CodeInvalidValue, path, node, "must be a list")
		return nil
	}
	out := make([]int64, 0, len(node.Content))
	for i, item := range node.Content {
		itemPath := indexPath(path, i)
		value, ok := d.integer(item, itemPath, 1, 100)
		if !ok {
			continue
		}
		if len(out) > 0 && value <= out[len(out)-1] {
			d.report(CodeInvalidValue, itemPath, item, "thresholds must be strictly increasing")
			continue
		}
		out = append(out, value)
	}
	return out
}

func (d *decoder) decodeChannel(node *yaml.Node, path, name string) AlertChannel {
	var out AlertChannel
	if !d.isMapping(node, path) {
		return out
	}
	typeNode := mappingValue(node, "type")
	if typeNode == nil {
		d.report(CodeInvalidValue, joinPath(path, "type"), node, "is required")
		return out
	}
	typeFields := fieldSet{"type": typeNode}
	d.positions[joinPath(path, "type")] = Position{Line: typeNode.Line, Column: typeNode.Column}
	optEnum(d, typeFields, "type", path, CodeInvalidValue, &out.Type, AlertWebhook, AlertDiscord, AlertSlack, AlertNtfy)

	switch out.Type {
	case AlertWebhook, AlertDiscord, AlertSlack:
		allowed := []string{"type", "url", "url_file"}
		if out.Type == AlertWebhook {
			allowed = append(allowed, "signing_secret", "signing_secret_file")
		}
		fields := d.fields(node, path, allowed...)
		var has bool
		if out.URL, has = d.secretRef(fields, path, name, "url", "url_file"); !has {
			d.report(CodeInvalidValue, joinPath(path, "url"), node, "url or url_file is required")
		}
		if out.Type == AlertWebhook {
			if out.SigningSecret, has = d.secretRef(fields, path, name, "signing_secret", "signing_secret_file"); !has {
				d.report(CodeInvalidValue, joinPath(path, "signing_secret"), node, "signing_secret or signing_secret_file is required")
			}
		}
	case AlertNtfy:
		fields := d.fields(node, path, "type", "server_url", "topic", "token", "token_file")
		d.requireFields(node, fields, path, "server_url", "topic")
		d.optText(fields, "server_url", path, &out.ServerURL)
		d.optText(fields, "topic", path, &out.Topic)
		out.Token, _ = d.secretRef(fields, path, name, "token", "token_file")
	}
	return out
}

func mappingValue(node *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(node.Content); i += 2 {
		if node.Content[i].Value == key {
			return node.Content[i+1]
		}
	}
	return nil
}
