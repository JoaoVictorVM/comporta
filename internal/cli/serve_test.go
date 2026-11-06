package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
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
	path := writeConfig(t, validConfig)
	provision(t, path)
	logs, stop := startServe(t, path)
	waitForLog(t, logs, "configuration active")

	edited := strings.Replace(validConfig, "timezone: UTC", "timezone: America/Sao_Paulo", 1)
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
	path := writeConfig(t, validConfig)
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

func TestServeStillOpensNoPorts(t *testing.T) {
	t.Setenv("COMPORTA_TEST_KEY", "value")
	proxy, admin := freePort(t), freePort(t)
	path := writeConfig(t, validConfig+fmt.Sprintf("listeners: {proxy: {port: %d}, admin: {port: %d}}\n", proxy, admin))
	provision(t, path)
	logs, stop := startServe(t, path)
	waitForLog(t, logs, "configuration active")
	for _, port := range []int{proxy, admin} {
		listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			t.Fatalf("serve must not bind port %d: %v", port, err)
		}
		_ = listener.Close()
	}
	if strings.Contains(logs.String(), "listening") {
		t.Fatalf("serve must not announce ports: %s", logs.String())
	}
	if code := stop(); code != 0 {
		t.Fatalf("exit %d", code)
	}
}

func TestServeLoadsAndWaitsForCancellation(t *testing.T) {
	t.Setenv("COMPORTA_TEST_KEY", "canary-serve-key")
	port := freePort(t)
	path := writeConfig(t, validConfig+fmt.Sprintf("listeners: {proxy: {port: %d}}\n", port))
	provision(t, path)

	var stdout, stderr lockedBuffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exit := make(chan int, 1)
	go func() { exit <- Execute(ctx, []string{"serve", "--config", path}, &stdout, &stderr) }()

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(stderr.String(), "configuration active") {
		if time.Now().After(deadline) {
			t.Fatalf("serve did not become active: %s", stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if strings.Contains(stderr.String(), "listening") {
		t.Fatalf("serve must not announce open ports: %s", stderr.String())
	}
	listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatalf("serve must not bind the configured port: %v", err)
	}
	_ = listener.Close()

	select {
	case code := <-exit:
		t.Fatalf("serve exited early with %d: %s", code, stderr.String())
	case <-time.After(100 * time.Millisecond):
	}
	cancel()
	select {
	case code := <-exit:
		if code != 0 {
			t.Fatalf("normal shutdown must exit 0, got %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop after cancellation")
	}
	logs := stderr.String()
	if !strings.Contains(logs, `"level":"INFO"`) || !strings.Contains(logs, "shutdown complete") {
		t.Fatalf("logs must be JSON and report shutdown: %s", logs)
	}
	if strings.Contains(logs, "canary-serve-key") {
		t.Fatalf("logs expose a secret: %s", logs)
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
