package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validConfig = `timezone: UTC
providers:
  anthropic:
    api_format: anthropic
    base_url: https://api.anthropic.com
    api_key: ${COMPORTA_TEST_KEY}
    models: [model-a]
apps:
  bot:
    allowed_models: [model-a]
`

func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = Execute(context.Background(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "comporta.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestValidateExamplesExitZeroWithoutSecrets(t *testing.T) {
	examples, err := filepath.Glob("../../examples/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(examples) < 2 {
		t.Fatalf("expected the example files, found %v", examples)
	}
	for _, path := range examples {
		t.Run(filepath.Base(path), func(t *testing.T) {
			code, stdout, stderr := run(t, "config", "validate", path)
			if code != 0 || !strings.Contains(stdout, "configuration valid") {
				t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
			}
		})
	}
}

func TestUnknownPolicyExitsOne(t *testing.T) {
	path := writeConfig(t, strings.Replace(validConfig, "  bot:\n", "  bot:\n    on_limit: sometimes\n", 1))
	code, stdout, stderr := run(t, "config", "validate", path)
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	if stdout != "" {
		t.Fatalf("errors belong on stderr, stdout: %q", stdout)
	}
	if !strings.Contains(stderr, "apps.bot.on_limit: must be one of block, downgrade, warn") {
		t.Fatalf("stderr: %q", stderr)
	}
	if strings.Contains(stderr, "sometimes") {
		t.Fatalf("diagnostic echoes the rejected value: %q", stderr)
	}
}

func TestLiteralSecretOutputIsSanitized(t *testing.T) {
	const canary = "sk-cli-canary"
	path := writeConfig(t, strings.Replace(validConfig, "${COMPORTA_TEST_KEY}", canary, 1))
	code, stdout, stderr := run(t, "config", "validate", path)
	if code != 1 || strings.Contains(stdout+stderr, canary) {
		t.Fatalf("exit %d, output %q %q", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "providers.anthropic.api_key contains a literal secret; use ${ANTHROPIC_API_KEY} or api_key_file") {
		t.Fatalf("stderr: %q", stderr)
	}
}

func TestResolveSecretsMode(t *testing.T) {
	path := writeConfig(t, validConfig)
	t.Setenv("COMPORTA_TEST_KEY", "")

	if code, _, stderr := run(t, "config", "validate", path); code != 0 {
		t.Fatalf("structural mode must not need the secret: %d %q", code, stderr)
	}
	code, _, stderr := run(t, "config", "validate", path, "--resolve-secrets")
	if code != 1 || !strings.Contains(stderr, "environment variable COMPORTA_TEST_KEY is not set") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	t.Setenv("COMPORTA_TEST_KEY", "value")
	if code, _, stderr := run(t, "config", "validate", path, "--resolve-secrets"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
}

func TestResolveSecretsReadsDotenvBesideConfig(t *testing.T) {
	path := writeConfig(t, strings.Replace(validConfig, "COMPORTA_TEST_KEY", "COMPORTA_TEST_DOTENV_KEY", 1))
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), ".env"), []byte("COMPORTA_TEST_DOTENV_KEY=value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := run(t, "config", "validate", path, "--resolve-secrets"); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
}

func TestConfigPathPrecedence(t *testing.T) {
	valid := writeConfig(t, validConfig)
	invalid := writeConfig(t, "providers: {}\n")

	if code, _, stderr := run(t, "config", "validate", "--config", valid); code != 0 {
		t.Fatalf("--config must be used: %d %q", code, stderr)
	}
	if code, _, stderr := run(t, "config", "validate", valid, "--config", valid); code != 0 {
		t.Fatalf("same path twice is allowed: %d %q", code, stderr)
	}
	code, _, stderr := run(t, "config", "validate", valid, "--config", invalid)
	if code != 1 || !strings.Contains(stderr, "different files") {
		t.Fatalf("conflicting paths: exit %d, stderr %q", code, stderr)
	}

	dir := filepath.Dir(valid)
	previous, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(previous) })
	if code, _, stderr := run(t, "config", "validate"); code != 0 {
		t.Fatalf("default path must be comporta.yaml: %d %q", code, stderr)
	}
}

func TestMissingFileAndUsageErrorsExitOne(t *testing.T) {
	code, _, stderr := run(t, "config", "validate", filepath.Join(t.TempDir(), "absent.yaml"))
	if code != 1 || !strings.Contains(stderr, "configuration file not found") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	if code, _, _ := run(t, "config", "validate", "a", "b"); code != 1 {
		t.Fatalf("too many arguments must exit 1, got %d", code)
	}
	if code, _, _ := run(t, "unknown-command"); code != 1 {
		t.Fatalf("unknown command must exit 1, got %d", code)
	}
}
