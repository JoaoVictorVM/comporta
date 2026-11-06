package config

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const literalCanary = "sk-literal-canary-1234"

func secretOptions(vars map[string]string) Options {
	return Options{SystemTimezone: "UTC", ResolveSecrets: true, Environment: NewEnvironment(vars)}
}

func TestLiteralAPIKeyRejectedEverywhere(t *testing.T) {
	literal := replaced(t, baseYAML, "${ANTHROPIC_API_KEY}", literalCanary)
	cases := map[string]struct {
		text string
		path string
	}{
		"provider":     {literal, "providers.anthropic.api_key"},
		"partial":      {replaced(t, baseYAML, "${ANTHROPIC_API_KEY}", "prefix-${ANTHROPIC_API_KEY}"), "providers.anthropic.api_key"},
		"shell syntax": {replaced(t, baseYAML, "${ANTHROPIC_API_KEY}", "${ANTHROPIC_API_KEY:-x}"), "providers.anthropic.api_key"},
		"number":       {replaced(t, baseYAML, "${ANTHROPIC_API_KEY}", "12345"), "providers.anthropic.api_key"},
		"empty":        {replaced(t, baseYAML, "${ANTHROPIC_API_KEY}", `""`), "providers.anthropic.api_key"},
		"unknown spot": {baseYAML + "surprise:\n  api_key: " + literalCanary + "\n", "surprise.api_key"},
		"in app":       {replaced(t, baseYAML, "  bot:\n", "  bot:\n    api_key: "+literalCanary+"\n"), "apps.bot.api_key"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			diags := requireDiagnostic(t, tc.text, CodeLiteralSecret, tc.path)
			for _, diagnostic := range diags {
				if strings.Contains(diagnostic.String(), literalCanary) {
					t.Fatalf("diagnostic exposes the literal: %s", diagnostic)
				}
			}
		})
	}

	_, diags := loadText(t, literal, structuralOptions())
	const documented = "providers.anthropic.api_key contains a literal secret; use ${ANTHROPIC_API_KEY} or api_key_file"
	if len(diags) != 1 || diags[0].String() != documented {
		t.Fatalf("expected only the documented message, got %+v", diags)
	}
}

func TestMissingEnvAbortsStartup(t *testing.T) {
	const documented = "providers.anthropic.api_key: environment variable ANTHROPIC_API_KEY is not set"
	for name, vars := range map[string]map[string]string{"absent": {}, "empty": {"ANTHROPIC_API_KEY": ""}} {
		t.Run(name, func(t *testing.T) {
			_, diags := loadText(t, baseYAML, secretOptions(vars))
			if len(diags) != 1 || diags[0].String() != documented {
				t.Fatalf("expected the documented message, got %+v", diags)
			}
		})
	}
	candidate, diags := loadText(t, baseYAML, secretOptions(map[string]string{"ANTHROPIC_API_KEY": "value"}))
	if candidate == nil {
		t.Fatalf("resolved configuration must load: %+v", diags)
	}
	if got := candidate.secrets["providers.anthropic.api_key"].Reveal(); got != "value" {
		t.Fatalf("resolved secret: %q", got)
	}
}

func TestStructuralModeDoesNotResolveSecrets(t *testing.T) {
	text := replaced(t, baseYAML, "api_key: ${ANTHROPIC_API_KEY}", "api_key_file: /does/not/exist")
	if candidate := requireValid(t, text); len(candidate.secrets) != 0 {
		t.Fatal("structural mode must not read secrets")
	}
}

func TestSecretSourcesExclusive(t *testing.T) {
	text := replaced(t, baseYAML, "api_key: ${ANTHROPIC_API_KEY}", "api_key: ${ANTHROPIC_API_KEY}\n    api_key_file: /run/secrets/key")
	requireDiagnostic(t, text, CodeInvalidValue, "providers.anthropic")
}

func TestLocalProviderMayOmitKey(t *testing.T) {
	cfg := requireValid(t, baseYAML).cfg
	if !cfg.Providers["local"].Local || cfg.Providers["local"].APIKey.IsSet() {
		t.Fatalf("local provider: %+v", cfg.Providers["local"])
	}
	remote := replaced(t, baseYAML, "    api_key: ${ANTHROPIC_API_KEY}\n", "")
	requireDiagnostic(t, remote, CodeInvalidValue, "providers.anthropic")
	localLiteral := replaced(t, baseYAML, "base_url: http://localhost:11434", "base_url: http://localhost:11434\n    api_key: "+literalCanary)
	requireDiagnostic(t, localLiteral, CodeLiteralSecret, "providers.local.api_key")
}

func fileKeyYAML(path string) string {
	return "timezone: UTC\nproviders:\n  p:\n    api_format: openai\n    base_url: https://x.example\n    api_key_file: " + path + "\n    models: [m]\n"
}

func TestSecretFileContent(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"value":       "value",
		"value\n":     "value",
		"value\r\n":   "value",
		"value\n\n":   "value\n",
		"value\r":     "value\r",
		" value \n":   " value ",
		"with\ninner": "with\ninner",
	}
	for content, want := range cases {
		path := filepath.Join(dir, "secret")
		writeFile(t, path, content)
		candidate, err := LoadBytes([]byte(fileKeyYAML(filepath.ToSlash(path))), filepath.Join(dir, "comporta.yaml"), secretOptions(nil))
		if err != nil {
			t.Fatalf("%q: %v", content, err)
		}
		if got := candidate.secrets["providers.p.api_key"].Reveal(); got != want {
			t.Errorf("content %q resolved to %q, want %q", content, got, want)
		}
	}
}

func TestSecretFileRelativeToConfig(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "secrets", "key"), "value\n")
	candidate, err := LoadBytes([]byte(fileKeyYAML("secrets/key")), filepath.Join(dir, "comporta.yaml"), secretOptions(nil))
	if err != nil {
		t.Fatal(err)
	}
	if candidate.secrets["providers.p.api_key"].Reveal() != "value" {
		t.Fatal("relative secret path must be anchored at the configuration directory")
	}
}

// deniedFS simulates an unreadable secret: portable permission bits cannot do
// this on Windows or when tests run as root.
type deniedFS struct{}

func (deniedFS) Open(string) (io.ReadCloser, error) { return nil, fs.ErrPermission }
func (deniedFS) Stat(string) (fs.FileInfo, error)   { return nil, fs.ErrPermission }

func TestUnreadableAndOversizedSecretFile(t *testing.T) {
	opts := secretOptions(nil)
	opts.FS = deniedFS{}
	_, diags := loadText(t, fileKeyYAML("/run/secrets/anthropic"), opts)
	const documented = "api_key_file /run/secrets/anthropic: permission denied"
	if len(diags) != 1 || diags[0].String() != documented || diags[0].Code != CodeSecretFileUnreadable {
		t.Fatalf("expected the documented message, got %+v", diags)
	}

	dir := t.TempDir()
	cases := map[string]struct {
		content string
		reason  string
	}{
		"oversized": {strings.Repeat("x", maxSecretFileBytes+1), "64 KiB"},
		"empty":     {"", "empty"},
		"only eol":  {"\n", "empty"},
		"nul":       {"a\x00b", "NUL"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, "secret")
			writeFile(t, path, tc.content)
			_, diags := loadText(t, fileKeyYAML(filepath.ToSlash(path)), secretOptions(nil))
			if len(diags) != 1 || !strings.Contains(diags[0].Message, tc.reason) {
				t.Fatalf("expected reason %q, got %+v", tc.reason, diags)
			}
		})
	}
	exact := filepath.Join(dir, "exact")
	writeFile(t, exact, strings.Repeat("x", maxSecretFileBytes))
	if _, diags := loadText(t, fileKeyYAML(filepath.ToSlash(exact)), secretOptions(nil)); diags != nil {
		t.Fatalf("file at the exact limit must load: %+v", diags)
	}

	_, diags = loadText(t, fileKeyYAML(filepath.ToSlash(dir)), secretOptions(nil))
	if len(diags) != 1 || !strings.Contains(diags[0].Message, "not a regular file") {
		t.Fatalf("a directory must be refused: %+v", diags)
	}
	_, diags = loadText(t, fileKeyYAML(filepath.ToSlash(filepath.Join(dir, "absent"))), secretOptions(nil))
	if len(diags) != 1 || !strings.Contains(diags[0].Message, "no such file") {
		t.Fatalf("an absent file must be classified: %+v", diags)
	}
}

func TestResolvedAlertURLValidatedWithoutExposure(t *testing.T) {
	const token = "canary-webhook-token"
	text := baseYAML + "alerts:\n  channels:\n    ops:\n      type: slack\n      url: ${HOOK}\n"
	_, diags := loadText(t, text, secretOptions(map[string]string{"ANTHROPIC_API_KEY": "k", "HOOK": "http://hooks.example/" + token}))
	if !hasDiagnostic(diags, CodeInvalidValue, "alerts.channels.ops.url") {
		t.Fatalf("plain http to a remote host must be refused: %+v", diags)
	}
	for _, diagnostic := range diags {
		if strings.Contains(diagnostic.String(), token) {
			t.Fatalf("diagnostic exposes the URL: %s", diagnostic)
		}
	}
	if _, diags := loadText(t, text, secretOptions(map[string]string{"ANTHROPIC_API_KEY": "k", "HOOK": "https://hooks.example/" + token + "?a=1"})); diags != nil {
		t.Fatalf("https webhook must load: %+v", diags)
	}
}

func TestSecretNeverRendersItsValue(t *testing.T) {
	const canary = "canary-secret-value"
	secret := Secret{value: canary}
	rendered := []string{
		fmt.Sprint(secret),
		fmt.Sprintf("%v %+v %#v %s %q %x %d", secret, secret, secret, secret, secret, secret, secret),
		fmt.Sprintf("%v", struct{ S Secret }{secret}),
	}
	data, err := json.Marshal(map[string]Secret{"key": secret})
	if err != nil {
		t.Fatal(err)
	}
	rendered = append(rendered, string(data))
	var logged strings.Builder
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("event", "secret", secret)
	rendered = append(rendered, logged.String())
	for _, text := range rendered {
		if strings.Contains(text, canary) {
			t.Fatalf("secret value exposed: %s", text)
		}
	}
	if secret.Reveal() != canary {
		t.Fatal("Reveal must return the value")
	}
}

func TestReloadRereadsSecretFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "comporta.yaml")
	writeFile(t, path, fileKeyYAML("key.txt"))
	writeFile(t, filepath.Join(dir, "key.txt"), "first\n")
	manager, err := NewManager(ManagerOptions{
		Path:    path,
		Options: secretOptions(nil),
		inspect: func(string) (bool, error) { return false, nil },
	})
	if err != nil {
		t.Fatal(err)
	}

	writeFile(t, filepath.Join(dir, "key.txt"), "second\n")
	if status := manager.Reload(context.Background()); status.Result != ResultApplied {
		t.Fatalf("status: %+v", status)
	}
	if key, _ := manager.Capture().ProviderAPIKey("p"); key.Reveal() != "second" {
		t.Fatal("a reload must reread the secret file")
	}

	if err := os.Remove(filepath.Join(dir, "key.txt")); err != nil {
		t.Fatal(err)
	}
	status := manager.Reload(context.Background())
	if status.Result != ResultRejected || !hasDiagnostic(status.Errors, CodeSecretFileUnreadable, "providers.p.api_key_file") {
		t.Fatalf("a secret file that vanished must reject the reload: %+v", status)
	}
	if key, _ := manager.Capture().ProviderAPIKey("p"); key.Reveal() != "second" {
		t.Fatal("the previous secret must stay active")
	}
}
