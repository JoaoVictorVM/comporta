package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
)

func newManager(t *testing.T, listeners string) (*config.Manager, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "comporta.yaml")
	content := "timezone: UTC\nproviders:\n  local:\n    api_format: openai\n    base_url: http://127.0.0.1:9\n    models: [m]\napps:\n  bot:\n    allowed_models: [m]\n" + listeners
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := config.NewManager(config.ManagerOptions{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	return manager, path
}

func reloadWith(t *testing.T, manager *config.Manager, path string, edit func(string) string) {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(edit(string(content))), 0o600); err != nil {
		t.Fatal(err)
	}
	if status := manager.Reload(context.Background()); status.Result != config.ResultApplied {
		t.Fatalf("reload: %+v", status)
	}
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

func portsConfig(t *testing.T, extra string) string {
	t.Helper()
	return fmt.Sprintf("listeners:\n  proxy: {port: %d}\n  admin: {port: %d}\n%s", freePort(t), freePort(t), extra)
}

// startRuntime runs a runtime on real sockets and returns its stop function.
func startRuntime(t *testing.T, options Options) (*Runtime, func() error) {
	t.Helper()
	rt, err := Open(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- rt.Run(ctx) }()
	stopped := false
	var result error
	stop := func() error {
		if stopped {
			return result
		}
		stopped = true
		cancel()
		select {
		case result = <-done:
		case <-time.After(20 * time.Second):
			t.Error("runtime did not stop")
		}
		return result
	}
	t.Cleanup(func() { _ = stop() })
	return rt, stop
}

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
}

func get(t *testing.T, addr net.Addr, path string) *http.Response {
	t.Helper()
	response, err := http.Get("http://" + addr.String() + path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

// rawRequest writes the bytes as they are, because net/http clients refuse many of
// the malformed values these tests need to send.
func rawRequest(t *testing.T, addr net.Addr, request string) string {
	t.Helper()
	conn, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := conn.Write([]byte(request)); err != nil {
		t.Fatal(err)
	}
	reply, _ := io.ReadAll(conn)
	return string(reply)
}

func statusOf(reply string) string {
	line, _, _ := strings.Cut(reply, "\r\n")
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return ""
	}
	return fields[1]
}

func hostRequest(host string) string {
	return "GET /healthz HTTP/1.1\r\nHost: " + host + "\r\nConnection: close\r\n\r\n"
}
