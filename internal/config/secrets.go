package config

import (
	"bytes"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const maxSecretFileBytes = 64 << 10

// Secret holds a resolved value. Every generic way of printing or serializing it
// yields the mask, so only an explicit Reveal call can expose the value.
type Secret struct {
	value string
}

// Reveal returns the secret value. Call it only at the point of use.
func (s Secret) Reveal() string { return s.value }

func (s Secret) String() string   { return Redacted }
func (s Secret) GoString() string { return Redacted }

// Format masks every fmt verb, including %x and %d, which bypass String.
func (s Secret) Format(state fmt.State, _ rune) { _, _ = state.Write([]byte(Redacted)) }

func (s Secret) MarshalText() ([]byte, error) { return []byte(Redacted), nil }
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(`"` + Redacted + `"`), nil }
func (s Secret) LogValue() slog.Value         { return slog.StringValue(Redacted) }

// resolveSecrets reads every referenced secret, keyed by the public field path.
func resolveSecrets(cfg *Config, d *decoder, baseDir string, opts Options) map[string]Secret {
	resolver := secretResolver{
		d: d, baseDir: baseDir, env: opts.Environment, fsys: opts.fileSystem(),
		values: map[string]Secret{},
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Providers)) {
		path := joinPath("providers", name)
		resolver.resolve(path, "api_key", "api_key_file", cfg.Providers[name].APIKey)
	}
	for _, name := range slices.Sorted(maps.Keys(cfg.Alerts.Channels)) {
		path := joinPath("alerts.channels", name)
		channel := cfg.Alerts.Channels[name]
		if secret, ok := resolver.resolve(path, "url", "url_file", channel.URL); ok {
			// The value is itself a secret, so the message never quotes it.
			if _, problem := checkEndpoint(secret.Reveal(), true); problem != "" {
				d.reportAt(CodeInvalidValue, joinPath(path, "url"), "resolved value "+problem)
			}
		}
		resolver.resolve(path, "signing_secret", "signing_secret_file", channel.SigningSecret)
		resolver.resolve(path, "token", "token_file", channel.Token)
	}
	return resolver.values
}

type secretResolver struct {
	d       *decoder
	baseDir string
	env     *Environment
	fsys    FileSystem
	values  map[string]Secret
}

func (r *secretResolver) resolve(parent, envField, fileField string, ref SecretRef) (Secret, bool) {
	var (
		secret Secret
		ok     bool
	)
	switch {
	case ref.Env != "":
		secret, ok = r.fromEnvironment(joinPath(parent, envField), ref.Env)
	case ref.File != "":
		secret, ok = r.fromFile(joinPath(parent, fileField), fileField, ref.File)
	}
	if ok {
		r.values[joinPath(parent, envField)] = secret
	}
	return secret, ok
}

func (r *secretResolver) fromEnvironment(path, name string) (Secret, bool) {
	value, _ := r.env.Lookup(name)
	if value == "" {
		r.d.reportAt(CodeMissingEnvironment, path, "environment variable "+name+" is not set")
		return Secret{}, false
	}
	return Secret{value: value}, true
}

func (r *secretResolver) fromFile(path, field, configured string) (Secret, bool) {
	fail := func(reason string) (Secret, bool) {
		r.d.reportAt(CodeSecretFileUnreadable, path, field+" "+displayPath(configured)+": "+reason)
		return Secret{}, false
	}
	target := configured
	if !filepath.IsAbs(target) {
		target = filepath.Join(r.baseDir, target)
	}
	// Stat follows symlinks on purpose: orchestrators mount secrets through them.
	info, err := r.fsys.Stat(target)
	if err != nil {
		return fail(classifyIOError(err))
	}
	if !info.Mode().IsRegular() {
		return fail("not a regular file")
	}
	data, err := readLimited(r.fsys, target, maxSecretFileBytes+1)
	switch {
	case err != nil:
		return fail(classifyIOError(err))
	case len(data) > maxSecretFileBytes:
		return fail("exceeds the 64 KiB limit")
	case bytes.IndexByte(data, 0) >= 0:
		return fail("contains a NUL byte")
	}
	// Only one final line break is removed: other whitespace may be part of the
	// credential, so a generic trim could silently change it.
	value := strings.TrimSuffix(strings.TrimSuffix(string(data), "\n"), "\r")
	if strings.HasSuffix(string(data), "\r") {
		value = string(data)
	}
	if value == "" {
		return fail("file is empty")
	}
	return Secret{value: value}, true
}

// displayPath keeps plain paths readable and escapes anything else, so a crafted
// path cannot inject log lines.
func displayPath(path string) string {
	for _, r := range path {
		if r < 0x20 || r == 0x7f {
			return strconv.QuoteToASCII(path)
		}
	}
	return path
}
