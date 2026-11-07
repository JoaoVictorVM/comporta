package server

import (
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

// closedAfter sends only part of the headers and measures when the server gives up.
func closedAfter(t *testing.T, addr net.Addr) time.Duration {
	t.Helper()
	conn, err := net.Dial("tcp", addr.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	began := time.Now()
	if _, err := conn.Write([]byte("GET /healthz HTTP/1.1\r\nHost: local")); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadAll(conn); err != nil {
		var timeout net.Error
		if errors.As(err, &timeout) && timeout.Timeout() {
			t.Fatal("the server kept a slow connection open")
		}
	}
	return time.Since(began)
}

func TestSlowHeadersTimeout(t *testing.T) {
	manager, _ := newManager(t, portsConfig(t, "  read_header_timeout: 200ms\n"))
	rt, _ := startRuntime(t, Options{Source: manager, Proxy: okHandler(), Admin: AdminSurface(manager)})
	for _, addr := range []net.Addr{rt.ProxyAddr(), rt.AdminAddr()} {
		elapsed := closedAfter(t, addr)
		if elapsed < 150*time.Millisecond || elapsed > 3*time.Second {
			t.Fatalf("closed after %s", elapsed)
		}
	}
}

func TestReloadHeaderTimeoutAppliesToNewConnections(t *testing.T) {
	manager, path := newManager(t, portsConfig(t, "  read_header_timeout: 8s\n"))
	rt, _ := startRuntime(t, Options{Source: manager, Proxy: okHandler(), Admin: AdminSurface(manager)})

	reloadWith(t, manager, path, func(text string) string {
		return strings.Replace(text, "read_header_timeout: 8s", "read_header_timeout: 200ms", 1)
	})
	if elapsed := closedAfter(t, rt.ProxyAddr()); elapsed > 3*time.Second {
		t.Fatalf("the reloaded value was not applied: %s", elapsed)
	}
}

func TestHeaderDeadlineDoesNotCutTheRequest(t *testing.T) {
	manager, _ := newManager(t, portsConfig(t, "  read_header_timeout: 150ms\n"))
	rt, _ := startRuntime(t, Options{Source: manager, Proxy: okHandler(), Admin: AdminSurface(manager)})
	reply := rawRequest(t, rt.AdminAddr(), hostRequest("localhost"))
	if statusOf(reply) != "200" {
		t.Fatalf("reply %q", reply)
	}
}
