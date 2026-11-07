package server

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
)

func TestHostAllowlistBothListeners(t *testing.T) {
	manager, _ := newManager(t, portsConfig(t, "  allowed_hosts: [gateway.example]\n"))
	rt, _ := startRuntime(t, Options{Source: manager, Proxy: ProxySurface(manager, &routeRecorder{}), Admin: AdminSurface(manager)})

	for name, addr := range map[string]net.Addr{"proxy": rt.ProxyAddr(), "admin": rt.AdminAddr()} {
		t.Run(name, func(t *testing.T) {
			reply := rawRequest(t, addr, hostRequest("evil.example"))
			if statusOf(reply) != "421" {
				t.Fatalf("reply: %s", reply)
			}
			if !strings.Contains(reply, `host \"evil.example\" not allowed; add it to allowed_hosts`) || !strings.Contains(reply, "host_not_allowed") {
				t.Fatalf("message: %s", reply)
			}
			for _, host := range []string{"localhost", "LOCALHOST:8080", "127.0.0.1:1", "[::1]:8080", "Gateway.Example"} {
				if status := statusOf(rawRequest(t, addr, hostRequest(host))); status == "421" {
					t.Fatalf("%s rejected", host)
				}
			}
		})
	}
}

func TestHostRejectsSuffixAndMalformedIPv6(t *testing.T) {
	manager, _ := newManager(t, portsConfig(t, "  allowed_hosts: [gateway.example]\n"))
	rt, _ := startRuntime(t, Options{Source: manager, Proxy: okHandler(), Admin: AdminSurface(manager)})
	for _, host := range []string{
		"gateway.example.evil.example", "evil.gateway.example", "localhost.", "user@localhost",
		"[::1", "[not-ipv6]", "[::1]x", "[127.0.0.1]", "localhost:99999", "localhost:abc", "::1",
	} {
		reply := rawRequest(t, rt.AdminAddr(), hostRequest(host))
		status := statusOf(reply)
		// net/http itself answers 400 for some of these before any handler runs.
		if status != "421" && status != "400" {
			t.Errorf("%q: status %s", host, status)
		}
		if strings.Contains(host, "@") && strings.Contains(reply, "user@") {
			t.Errorf("%q echoed: %s", host, reply)
		}
	}
}

func TestNormalizeHost(t *testing.T) {
	cases := map[string]string{
		"Example.COM":     "example.com",
		"example.com:443": "example.com",
		"[::1]":           "[::1]",
		"[::1]:80":        "[::1]",
		"[FE80::1]":       "[fe80::1]",
	}
	for raw, want := range cases {
		got, ok := NormalizeHost(raw)
		if !ok || got != want {
			t.Errorf("%q: got %q, %v", raw, got, ok)
		}
	}
	for _, raw := range []string{"", ":80", "a:b:c", "[fe80::1%25en0]", "*.example", "a b", "a/b", "example.com:0", "example.com:"} {
		if got, ok := NormalizeHost(raw); ok {
			t.Errorf("%q accepted as %q", raw, got)
		}
	}
}

func TestForwardedHostIgnored(t *testing.T) {
	manager, _ := newManager(t, portsConfig(t, ""))
	rt, _ := startRuntime(t, Options{Source: manager, Proxy: okHandler(), Admin: AdminSurface(manager)})
	reply := rawRequest(t, rt.AdminAddr(), "GET /healthz HTTP/1.1\r\nHost: evil.example\r\nX-Forwarded-Host: localhost\r\nForwarded: host=localhost\r\nConnection: close\r\n\r\n")
	if statusOf(reply) != "421" {
		t.Fatalf("forwarded host was trusted: %s", reply)
	}
	reply = rawRequest(t, rt.AdminAddr(), "GET /healthz HTTP/1.1\r\nHost: localhost\r\nX-Forwarded-Host: evil.example\r\nConnection: close\r\n\r\n")
	if statusOf(reply) != "200" {
		t.Fatalf("forwarded host was used: %s", reply)
	}
}

func TestAllowedHostsApplyAfterReload(t *testing.T) {
	manager, path := newManager(t, portsConfig(t, ""))
	rt, _ := startRuntime(t, Options{Source: manager, Proxy: okHandler(), Admin: AdminSurface(manager)})
	if status := statusOf(rawRequest(t, rt.AdminAddr(), hostRequest("added.example"))); status != "421" {
		t.Fatalf("status %s", status)
	}
	reloadWith(t, manager, path, func(text string) string {
		return text + "  allowed_hosts: [added.example]\n"
	})
	if status := statusOf(rawRequest(t, rt.AdminAddr(), hostRequest("added.example"))); status != "200" {
		t.Fatalf("status %s", status)
	}
}

func TestHealthBodyIsStatic(t *testing.T) {
	manager, _ := newManager(t, portsConfig(t, ""))
	rt, _ := startRuntime(t, Options{Source: manager, Proxy: okHandler(), Admin: AdminSurface(manager)})
	response := get(t, rt.AdminAddr(), "/healthz")
	var body map[string]any
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 1 || body["status"] != "ok" || response.StatusCode != http.StatusOK {
		t.Fatalf("status %d, body %v", response.StatusCode, body)
	}
}

func FuzzNormalizeHost(f *testing.F) {
	for _, seed := range []string{"localhost", "[::1]:8080", "evil.example", "a@b", "[fe80::1%25en0]", "x:1:2", "*.a"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		host, ok := NormalizeHost(raw)
		if !ok {
			return
		}
		if strings.ContainsAny(host, "@/\\?#%* \t,") {
			t.Fatalf("%q normalized to %q", raw, host)
		}
		if again, stable := NormalizeHost(host); !stable || again != host {
			t.Fatalf("%q is not stable: %q", host, again)
		}
		if echoableHost.MatchString(host) && strings.ContainsAny(host, "\"\\") {
			t.Fatalf("%q is unsafe to echo", host)
		}
	})
}
