package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/keys"
)

var (
	printedKey = regexp.MustCompile(`cmp_[0-9A-Za-z]{43}`)
	printedID  = regexp.MustCompile(`k_[0-9a-f]{32}`)
)

func keysConfig(t *testing.T) (path, dataDir string) {
	t.Helper()
	path = writeConfig(t, strings.Replace(validConfig, "  bot:\n", "  bot-discord:\n    allowed_models: [model-a]\n  bot:\n", 1))
	return path, filepath.Join(filepath.Dir(path), "data")
}

func TestCreatePrintsKeyOnlyOnceAfterCommit(t *testing.T) {
	path, dataDir := keysConfig(t)
	code, stdout, stderr := run(t, "keys", "create", "bot-discord", "--config", path)
	if code != 0 || stderr != "" {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 {
		t.Fatalf("output %q", stdout)
	}
	key := printedKey.FindString(lines[0])
	id := printedID.FindString(lines[1])
	if lines[0] != "Key for bot-discord (shown only once): "+key || key == "" {
		t.Fatalf("first line %q", lines[0])
	}
	if lines[1] != "Store it in the app's environment. Key ID: "+id || id == "" {
		t.Fatalf("second line %q", lines[1])
	}

	stored, err := os.ReadFile(filepath.Join(dataDir, "keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(stored, []byte(key)) {
		t.Fatal("the key reached the disk")
	}
	if !bytes.Contains(stored, []byte(id)) {
		t.Fatal("the commit must happen before the key is printed")
	}

	_, listed, _ := run(t, "keys", "list", "--config", path)
	if strings.Contains(listed, key) {
		t.Fatal("the key can only be shown by its creation")
	}
}

func TestCreateFailureShowsNoKey(t *testing.T) {
	path, dataDir := keysConfig(t)
	if code, _, _ := run(t, "keys", "create", "bot", "--config", path); code != 0 {
		t.Fatal("setup")
	}
	if err := os.WriteFile(filepath.Join(dataDir, "keys.json"), []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := run(t, "keys", "create", "bot", "--config", path)
	if code != 1 || stdout != "" {
		t.Fatalf("exit %d, stdout %q", code, stdout)
	}
	if want := "keys.json is unreadable; restore from backup or recreate keys"; !strings.Contains(stderr, want) {
		t.Fatalf("stderr %q", stderr)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("closed") }

func TestPrintFailureAfterCommitExplainsRecovery(t *testing.T) {
	path, _ := keysConfig(t)
	var stderr bytes.Buffer
	code := Execute(context.Background(), []string{"keys", "create", "bot", "--config", path}, failingWriter{}, &stderr)
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stderr.String(), "revoke key ID k_") || !strings.Contains(stderr.String(), "create another") {
		t.Fatalf("stderr %q", stderr.String())
	}
	if printedKey.MatchString(stderr.String()) {
		t.Fatal("the fallback must not print the key")
	}
	_, listed, _ := run(t, "keys", "list", "--config", path)
	if strings.Count(listed, "k_") != 1 {
		t.Fatalf("creation must not be retried: %s", listed)
	}
}

func TestKeysListNeverRevealsSecrets(t *testing.T) {
	path, dataDir := keysConfig(t)
	code, listed, _ := run(t, "keys", "list", "--config", path)
	if code != 0 || !strings.HasPrefix(listed, "KEY ID") {
		t.Fatalf("empty list: exit %d, %q", code, listed)
	}
	if _, err := os.Stat(dataDir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("list must write nothing")
	}

	_, created, _ := run(t, "keys", "create", "bot", "--config", path)
	key := printedKey.FindString(created)
	id := printedID.FindString(created)
	_, _, _ = run(t, "keys", "create", "bot-discord", "--config", path)
	if code, stdout, stderr := run(t, "keys", "revoke", id, "--config", path); code != 0 || !strings.Contains(stdout, "revoked") {
		t.Fatalf("revoke: %d %q %q", code, stdout, stderr)
	}

	code, listed, stderr := run(t, "keys", "list", "--config", path)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	for _, column := range []string{"KEY ID", "APP", "STATUS", "CREATED AT", "LAST USED AT", "active", "revoked", "never"} {
		if !strings.Contains(listed, column) {
			t.Fatalf("missing %q in %s", column, listed)
		}
	}
	document, err := os.ReadFile(filepath.Join(dataDir, "keys.json"))
	if err != nil {
		t.Fatal(err)
	}
	digest := regexp.MustCompile(`[0-9a-f]{64}`).FindString(string(document))
	if digest == "" {
		t.Fatal("no digest stored")
	}
	if strings.Contains(listed, key) || strings.Contains(listed, key[4:12]) || strings.Contains(listed, digest) {
		t.Fatalf("listing exposes a secret: %s", listed)
	}
	lines := strings.Split(strings.TrimSpace(listed), "\n")
	if !strings.Contains(lines[1], " bot ") || !strings.Contains(lines[2], "bot-discord") {
		t.Fatalf("rows must be ordered by app: %s", listed)
	}
}

func TestRevokeIsIdempotentAndUnknownIDFails(t *testing.T) {
	path, _ := keysConfig(t)
	if code, _, stderr := run(t, "keys", "revoke", "k_00000000000000000000000000000000", "--config", path); code != 1 || !strings.Contains(stderr, "key ID not found") {
		t.Fatalf("missing store: %d %q", code, stderr)
	}
	_, created, _ := run(t, "keys", "create", "bot", "--config", path)
	id := printedID.FindString(created)
	if code, _, _ := run(t, "keys", "revoke", id, "--config", path); code != 0 {
		t.Fatal("first revoke")
	}
	if code, stdout, _ := run(t, "keys", "revoke", id, "--config", path); code != 0 || !strings.Contains(stdout, "already revoked") {
		t.Fatalf("second revoke: %d %q", code, stdout)
	}
	code, _, stderr := run(t, "keys", "revoke", "unknown\x1b[31m", "--config", path)
	if code != 1 || !strings.Contains(stderr, "key ID not found") || strings.Contains(stderr, "\x1b") {
		t.Fatalf("%d %q", code, stderr)
	}
}

func TestThirdKeyIsRefusedWithDocumentedMessage(t *testing.T) {
	path, _ := keysConfig(t)
	for _, verb := range []string{"create", "rotate"} {
		if code, _, stderr := run(t, "keys", verb, "bot-discord", "--config", path); code != 0 {
			t.Fatalf("%s: %q", verb, stderr)
		}
	}
	code, stdout, stderr := run(t, "keys", "create", "bot-discord", "--config", path)
	if code != 1 || stdout != "" {
		t.Fatalf("exit %d, stdout %q", code, stdout)
	}
	if want := "bot-discord already has 2 active keys; revoke one before rotating\n"; stderr != want {
		t.Fatalf("stderr %q", stderr)
	}
}

func TestUnknownAppHasSuggestion(t *testing.T) {
	path, _ := keysConfig(t)
	cases := map[string]string{
		"bot-dicord":   `app "bot-dicord" is not defined in comporta.yaml (did you mean "bot-discord"?)`,
		"bo":           `app "bo" is not defined in comporta.yaml (did you mean "bot"?)`,
		"far-away-app": `app "far-away-app" is not defined in comporta.yaml`,
		"bad\x1b[2Jx":  `app "bad\x1b[2Jx" is not defined in comporta.yaml`,
	}
	for input, want := range cases {
		t.Run(want, func(t *testing.T) {
			code, stdout, stderr := run(t, "keys", "create", input, "--config", path)
			if code != 1 || stdout != "" {
				t.Fatalf("exit %d, stdout %q", code, stdout)
			}
			if strings.TrimSpace(stderr) != want {
				t.Fatalf("stderr %q", stderr)
			}
			if strings.Contains(stderr, "\x1b") {
				t.Fatal("control sequences must be escaped")
			}
		})
	}
	if got := suggestApp("ab", []string{"aa", "ac"}); got != "aa" {
		t.Fatalf("ties are lexical, got %q", got)
	}
}

func TestKeyCreationAcceptsOnlyConfiguredApps(t *testing.T) {
	config := strings.Replace(validConfig, "timezone: UTC\n", "timezone: UTC\ndata_dir: ./relative/state\n", 1)
	path := writeConfig(t, config)
	if code, _, stderr := run(t, "keys", "create", "bot", "--config", path); code != 0 {
		t.Fatalf("configured app: %q", stderr)
	}
	stored := filepath.Join(filepath.Dir(path), "relative", "state", "keys.json")
	if _, err := os.Stat(stored); err != nil {
		t.Fatalf("data_dir must resolve from the configuration file: %v", err)
	}
	if code, _, _ := run(t, "keys", "create", "not-configured", "--config", path); code != 1 {
		t.Fatal("an unknown app must be refused")
	}
	if code, _, stderr := run(t, "keys", "create", "BOT", "--config", path); code != 1 {
		t.Fatalf("names are exact: %d %q", code, stderr)
	}
}

func TestKeysCommandsDoNotResolveProviderSecrets(t *testing.T) {
	config := strings.ReplaceAll(validConfig, "COMPORTA_TEST_KEY", "COMPORTA_TEST_KEY_NEVER_SET")
	path := writeConfig(t, config)
	dotenv := filepath.Join(filepath.Dir(path), ".env")
	if err := os.WriteFile(dotenv, []byte("this is not a valid dotenv line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := run(t, "keys", "create", "bot", "--config", path)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	id := printedID.FindString(stdout)
	for _, args := range [][]string{{"list"}, {"rotate", "bot"}, {"revoke", id}} {
		if code, _, stderr := run(t, append(append([]string{"keys"}, args...), "--config", path)...); code != 0 {
			t.Fatalf("%v: %q", args, stderr)
		}
	}
}

func TestKeysCommandsReportInvalidConfiguration(t *testing.T) {
	path := writeConfig(t, "providers: {}\n")
	code, stdout, stderr := run(t, "keys", "list", "--config", path)
	if code != 1 || stdout != "" || stderr == "" {
		t.Fatalf("%d %q %q", code, stdout, stderr)
	}
}

func TestKeysCommandUsageErrors(t *testing.T) {
	path, _ := keysConfig(t)
	for _, args := range [][]string{{"keys", "create"}, {"keys", "revoke"}, {"keys", "list", "extra"}} {
		if code, _, _ := run(t, append(args, "--config", path)...); code != 1 {
			t.Fatalf("%v must fail", args)
		}
	}
}

func TestIOErrorMessageNamesPermissionOnly(t *testing.T) {
	if got := ioErrorMessage(keys.ErrStoreIO); got != keys.ErrStoreIO.Error() {
		t.Fatalf("got %q", got)
	}
}
