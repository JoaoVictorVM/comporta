package cli

import (
	"bytes"
	"context"
	"fmt"
	"net"
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

func TestServeLoadsAndWaitsForCancellation(t *testing.T) {
	t.Setenv("COMPORTA_TEST_KEY", "canary-serve-key")
	port := freePort(t)
	path := writeConfig(t, validConfig+fmt.Sprintf("listeners: {proxy: {port: %d}}\n", port))

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
