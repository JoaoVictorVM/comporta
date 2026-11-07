package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func freePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	return listener.Addr().(*net.TCPAddr).Port
}

// listenerConfig uses free ports, since the defaults may be in use on the machine.
func listenerConfig(t *testing.T) (path string, proxyPort, adminPort int) {
	t.Helper()
	proxyPort, adminPort = freePort(t), freePort(t)
	return writeConfig(t, validConfig+fmt.Sprintf("listeners: {proxy: {port: %d}, admin: {port: %d}}\n", proxyPort, adminPort)), proxyPort, adminPort
}

func provision(t *testing.T, path string) {
	t.Helper()
	if code, _, stderr := run(t, "keys", "create", "bot", "--config", path); code != 0 {
		t.Fatalf("provisioning failed: %s", stderr)
	}
}

// breakKeyStore rewrites the file in place, keeping its permissions, so that only
// the content is what makes it unreadable.
func breakKeyStore(t *testing.T, path string) {
	t.Helper()
	store := filepath.Join(filepath.Dir(path), "data", "keys.json")
	file, err := os.OpenFile(store, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString("{broken"); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func startServe(t *testing.T, path string) (logs *lockedBuffer, stop func() int) {
	t.Helper()
	logs = &lockedBuffer{}
	var stdout lockedBuffer
	ctx, cancel := context.WithCancel(context.Background())
	exit := make(chan int, 1)
	go func() { exit <- Execute(ctx, []string{"serve", "--config", path}, &stdout, logs) }()
	var once sync.Once
	code := 0
	stop = func() int {
		once.Do(func() {
			cancel()
			select {
			case code = <-exit:
			case <-time.After(10 * time.Second):
				t.Error("serve did not stop after cancellation")
				code = -1
			}
		})
		return code
	}
	t.Cleanup(func() { stop() })
	return logs, stop
}

func waitForLog(t *testing.T, logs *lockedBuffer, text string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(logs.String(), text) {
		if time.Now().After(deadline) {
			t.Fatalf("%q did not appear in: %s", text, logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestServeRequiresReadableKeyStore(t *testing.T) {
	t.Setenv("COMPORTA_TEST_KEY", "value")

	t.Run("missing", func(t *testing.T) {
		path := writeConfig(t, validConfig)
		code, _, stderr := run(t, "serve", "--config", path)
		if code != 1 || !strings.Contains(stderr, "keys.json does not exist") {
			t.Fatalf("exit %d, stderr %q", code, stderr)
		}
		if _, err := os.Stat(filepath.Join(filepath.Dir(path), "data", "keys.json")); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("serve must not create the store")
		}
	})
	t.Run("corrupt", func(t *testing.T) {
		path := writeConfig(t, validConfig)
		provision(t, path)
		breakKeyStore(t, path)
		code, _, stderr := run(t, "serve", "--config", path)
		if code != 1 || !strings.Contains(stderr, "keys.json is unreadable; restore from backup or recreate keys") {
			t.Fatalf("exit %d, stderr %q", code, stderr)
		}
	})
}

func TestServeRunsBothWatchersAndStops(t *testing.T) {
	t.Setenv("COMPORTA_TEST_KEY", "value")
	path, _, _ := listenerConfig(t)
	provision(t, path)
	logs, stop := startServe(t, path)
	waitForLog(t, logs, "configuration active")

	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(current), "timezone: UTC", "timezone: America/Sao_Paulo", 1)
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForLog(t, logs, "config reloaded")

	breakKeyStore(t, path)
	waitForLog(t, logs, "key store unavailable")

	if code := stop(); code != 0 {
		t.Fatalf("exit %d: %s", code, logs.String())
	}
	waitForLog(t, logs, "shutdown complete")
}

func TestServeSavesNothingWhenNoKeyWasUsed(t *testing.T) {
	t.Setenv("COMPORTA_TEST_KEY", "value")
	path, _, _ := listenerConfig(t)
	provision(t, path)
	store := filepath.Join(filepath.Dir(path), "data", "keys.json")
	before, err := os.ReadFile(store)
	if err != nil {
		t.Fatal(err)
	}
	logs, stop := startServe(t, path)
	waitForLog(t, logs, "configuration active")
	if code := stop(); code != 0 {
		t.Fatalf("exit %d: %s", code, logs.String())
	}
	after, err := os.ReadFile(store)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("serve must not rewrite an unchanged store")
	}
}

func TestServeOpensConfiguredListeners(t *testing.T) {
	t.Setenv("COMPORTA_TEST_KEY", "canary-serve-key")
	path, proxyPort, adminPort := listenerConfig(t)
	provision(t, path)
	logs, stop := startServe(t, path)
	waitForLog(t, logs, "listening")

	health, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/healthz", adminPort))
	if err != nil {
		t.Fatal(err)
	}
	_ = health.Body.Close()
	if health.StatusCode != http.StatusOK {
		t.Fatalf("health status %d", health.StatusCode)
	}
	models, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d/v1/models", proxyPort))
	if err != nil {
		t.Fatal(err)
	}
	_ = models.Body.Close()
	if models.StatusCode != http.StatusUnauthorized {
		t.Fatalf("the proxy must require a key, got %d", models.StatusCode)
	}

	if code := stop(); code != 0 {
		t.Fatalf("exit %d: %s", code, logs.String())
	}
	for _, port := range []int{proxyPort, adminPort} {
		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			t.Fatalf("port %d still bound after shutdown: %v", port, err)
		}
		_ = listener.Close()
	}
	output := logs.String()
	if !strings.Contains(output, `"level":"INFO"`) || !strings.Contains(output, "shutdown complete") {
		t.Fatalf("logs must be JSON and report shutdown: %s", output)
	}
	if strings.Contains(output, "canary-serve-key") {
		t.Fatalf("logs expose a secret: %s", output)
	}
}

func TestServeInvalidStoreOpensNoPorts(t *testing.T) {
	t.Setenv("COMPORTA_TEST_KEY", "value")
	path, proxyPort, adminPort := listenerConfig(t)
	code, _, _ := run(t, "serve", "--config", path)
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	for _, port := range []int{proxyPort, adminPort} {
		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			t.Fatalf("port %d stayed bound: %v", port, err)
		}
		_ = listener.Close()
	}
}

func TestServeBindFailureExitsOne(t *testing.T) {
	t.Setenv("COMPORTA_TEST_KEY", "value")
	path, proxyPort, _ := listenerConfig(t)
	provision(t, path)
	taken, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", proxyPort))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	code, _, stderr := run(t, "serve", "--config", path)
	if code != 1 || !strings.Contains(stderr, "could not open the proxy listener") {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
}

func TestServeInvalidConfigExitsOne(t *testing.T) {
	const literal = "sk-serve-literal-canary"
	cases := map[string]struct {
		config string
		want   string
	}{
		"literal key": {
			strings.Replace(validConfig, "${COMPORTA_TEST_KEY}", literal, 1),
			"providers.anthropic.api_key contains a literal secret; use ${ANTHROPIC_API_KEY} or api_key_file",
		},
		"missing variable": {
			strings.ReplaceAll(validConfig, "COMPORTA_TEST_KEY", "COMPORTA_TEST_UNSET_KEY"),
			"providers.anthropic.api_key: environment variable COMPORTA_TEST_UNSET_KEY is not set",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var stdout, stderr lockedBuffer
			path := writeConfig(t, tc.config)
			code := Execute(context.Background(), []string{"serve", "--config", path}, &stdout, &stderr)
			if code != 1 {
				t.Fatalf("exit %d", code)
			}
			if !strings.Contains(stderr.String(), tc.want) {
				t.Fatalf("documented message missing: %s", stderr.String())
			}
			if strings.Contains(stderr.String(), literal) {
				t.Fatalf("output exposes the literal: %s", stderr.String())
			}
		})
	}
}
