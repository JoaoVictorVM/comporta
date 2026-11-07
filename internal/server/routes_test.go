package server

import (
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/proxy"
)

type routeRecorder struct{ hits atomic.Int64 }

func (r *routeRecorder) ServeRoute(w http.ResponseWriter, _ *http.Request, _ proxy.Route, _ *config.Snapshot) {
	r.hits.Add(1)
	w.WriteHeader(http.StatusNoContent)
}

func call(method, target string, extra ...string) string {
	return method + " " + target + " HTTP/1.1\r\nHost: localhost\r\n" + strings.Join(extra, "") + "Connection: close\r\n\r\n"
}

func startProxySurface(t *testing.T) (*Runtime, *routeRecorder) {
	t.Helper()
	manager, _ := newManager(t, portsConfig(t, ""))
	recorder := &routeRecorder{}
	rt, _ := startRuntime(t, Options{Source: manager, Proxy: ProxySurface(manager, recorder), Admin: AdminSurface(manager)})
	return rt, recorder
}

func TestOnlyExactProxyRoutesAllowed(t *testing.T) {
	rt, recorder := startProxySurface(t)
	allowed := [][2]string{
		{"POST", "/v1/messages"}, {"POST", "/v1/messages/count_tokens"},
		{"POST", "/v1/chat/completions"}, {"GET", "/v1/models"},
	}
	for _, route := range allowed {
		if status := statusOf(rawRequest(t, rt.ProxyAddr(), call(route[0], route[1]))); status != "204" {
			t.Errorf("%v: status %s", route, status)
		}
	}
	if got := recorder.hits.Load(); got != int64(len(allowed)) {
		t.Fatalf("hits %d", got)
	}
	recorder.hits.Store(0)
	refused := [][2]string{
		{"GET", "/v1/messages"}, {"PUT", "/v1/messages"}, {"POST", "/v1/models"}, {"OPTIONS", "/v1/messages"},
		{"DELETE", "/v1/models"}, {"HEAD", "/v1/models"}, {"POST", "/v1/messages/"}, {"POST", "/v1/complete"},
		{"POST", "/v1/responses"}, {"POST", "/v1/messages/batches"}, {"GET", "/"},
	}
	for _, route := range refused {
		reply := rawRequest(t, rt.ProxyAddr(), call(route[0], route[1]))
		if statusOf(reply) != "404" {
			t.Errorf("%v: reply %s", route, reply)
		}
		if strings.Contains(reply, "Location:") {
			t.Errorf("%v: redirected", route)
		}
	}
	if got := recorder.hits.Load(); got != 0 {
		t.Fatalf("a refused route reached the handler %d times", got)
	}
}

func TestFilesNeverForwarded(t *testing.T) {
	rt, recorder := startProxySurface(t)
	reply := rawRequest(t, rt.ProxyAddr(), call("POST", "/v1/files", "Content-Length: 5\r\n")+"hello")
	if statusOf(reply) != "404" || !strings.Contains(reply, "endpoint not allowed by Comporta") || !strings.Contains(reply, "POST /v1/messages") {
		t.Fatalf("reply: %s", reply)
	}
	if recorder.hits.Load() != 0 {
		t.Fatal("the request reached the handler")
	}
}

func TestAdminRoutesNeverReachProxy(t *testing.T) {
	rt, recorder := startProxySurface(t)
	if status := statusOf(rawRequest(t, rt.ProxyAddr(), call("GET", "/healthz"))); status != "404" {
		t.Fatalf("proxy exposes healthz: %s", status)
	}
	for _, target := range []string{"/v1/messages", "/v1/models", "/debug/pprof/", "/debug/vars", "/metrics", "/healthz/", "/healthz?x=1", "/readyz"} {
		if status := statusOf(rawRequest(t, rt.AdminAddr(), call("GET", target))); status != "404" {
			t.Errorf("admin %s: status %s", target, status)
		}
	}
	if status := statusOf(rawRequest(t, rt.AdminAddr(), call("POST", "/healthz"))); status != "404" {
		t.Errorf("admin POST healthz: status %s", status)
	}
	if recorder.hits.Load() != 0 {
		t.Fatal("the admin listener reached a proxy route")
	}
}

func TestEncodedPathCannotBypassAllowlist(t *testing.T) {
	rt, recorder := startProxySurface(t)
	for _, target := range []string{
		"/v1/%6Dessages", "//v1/messages", "/v1//messages", "/v1/./messages", "/v1/x/../messages",
		"/v1/messages%2f", "/v1%2fmessages", "/./v1/messages", "/V1/MESSAGES", "/v1/messages;x",
	} {
		reply := rawRequest(t, rt.ProxyAddr(), call("POST", target))
		if statusOf(reply) != "404" || strings.Contains(reply, "Location:") {
			t.Errorf("%s: %s", target, reply)
		}
	}
	if got := recorder.hits.Load(); got != 0 {
		t.Fatalf("%d bypasses", got)
	}
}

func TestAbsoluteAndAuthorityTargetsAreRejected(t *testing.T) {
	rt, recorder := startProxySurface(t)
	cases := []string{
		"POST http://localhost/v1/messages HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n",
		"POST http://evil.example/v1/messages HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n",
		"CONNECT evil.example:443 HTTP/1.1\r\nHost: evil.example:443\r\nConnection: close\r\n\r\n",
		"OPTIONS * HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n",
	}
	for _, text := range cases {
		if status := statusOf(rawRequest(t, rt.ProxyAddr(), text)); status != "400" {
			t.Errorf("%q: status %s", text, status)
		}
	}
	if recorder.hits.Load() != 0 {
		t.Fatal("a forward proxy request reached the handler")
	}
}

func TestRefusalIsDeliveredWithUnreadBody(t *testing.T) {
	rt, recorder := startProxySurface(t)
	body := strings.Repeat("x", 128<<10)
	for range 20 {
		reply := rawRequest(t, rt.ProxyAddr(), call("POST", "/v1/files", "Content-Length: 131072\r\n")+body)
		if statusOf(reply) != "404" || !strings.Contains(reply, "endpoint_not_allowed") {
			t.Fatalf("reply: %.200q", reply)
		}
	}
	if recorder.hits.Load() != 0 {
		t.Fatal("the request reached the handler")
	}
}
