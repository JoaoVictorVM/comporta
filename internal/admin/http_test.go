package admin

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAdminRequiresToken(t *testing.T) {
	f := newAPI(t, nil)
	for path, current := range f.handler.routes {
		for method := range current.methods {
			body := ""
			if method == http.MethodPost {
				body = `{}`
			}
			if path == Prefix+"session" && method == http.MethodPost {
				body = `{"token":"` + strings.Repeat("0", 64) + `"}`
			}
			for _, header := range []http.Header{
				jsonHeader(),
				jsonHeader("Authorization", "Bearer "+strings.Repeat("a", 64)),
				jsonHeader("Authorization", "Bearer"),
				jsonHeader("Authorization", "Basic "+f.token),
				jsonHeader("X-API-Key", f.token),
				jsonHeader("Cookie", "token="+f.token),
				jsonHeader("Cookie", SessionCookie+"="+f.token),
			} {
				response := f.do(call{method: method, target: path, body: body, header: header, remote: "198.51.100.7:1"})
				if response.Code != http.StatusUnauthorized || errorCode(t, response) != "invalid_admin_token" {
					t.Fatalf("%s %s without a valid token: %d %s", method, path, response.Code, response.Body)
				}
				if !strings.Contains(response.Body.String(), `"message":"invalid admin token"`) {
					t.Fatalf("message: %s", response.Body)
				}
				f.clock.advance(GuardWindow)
			}
		}
	}
}

func TestAdminTenthFailureArmsBlock(t *testing.T) {
	f := newAPI(t, nil)
	wrong := call{method: http.MethodDelete, target: "/admin/v1/session", header: http.Header{"Authorization": {"Bearer " + strings.Repeat("b", 64)}}}
	for i := 1; i <= GuardMaxFailures; i++ {
		if response := f.do(wrong); response.Code != http.StatusUnauthorized {
			t.Fatalf("failure %d: %d", i, response.Code)
		}
	}
	blocked := f.do(call{method: http.MethodPost, target: "/admin/v1/session", body: `{"token":"` + f.token + `"}`, header: jsonHeader()})
	if blocked.Code != http.StatusTooManyRequests || errorCode(t, blocked) != "admin_rate_limited" {
		t.Fatalf("the eleventh attempt, with a valid token: %d", blocked.Code)
	}
	if blocked.Header().Get("Retry-After") != "300" {
		t.Fatalf("Retry-After %q", blocked.Header().Get("Retry-After"))
	}
	other := f.do(call{method: http.MethodPost, target: "/admin/v1/session", body: `{"token":"` + f.token + `"}`, header: jsonHeader(), remote: "192.0.2.99:1"})
	if other.Code != http.StatusCreated {
		t.Fatalf("another address must not be blocked: %d", other.Code)
	}
	f.clock.advance(GuardCooldown - time.Second)
	if response := f.do(call{method: http.MethodPost, target: "/admin/v1/session", body: `{"token":"` + f.token + `"}`, header: jsonHeader()}); response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "1" {
		t.Fatalf("one second before the end: %d %q", response.Code, response.Header().Get("Retry-After"))
	}
	f.clock.advance(time.Second)
	if response := f.do(call{method: http.MethodPost, target: "/admin/v1/session", body: `{"token":"` + f.token + `"}`, header: jsonHeader()}); response.Code != http.StatusCreated {
		t.Fatalf("after five minutes: %d", response.Code)
	}

	g := newAPI(t, nil)
	for range GuardMaxFailures - 1 {
		g.do(wrong)
	}
	g.clock.advance(GuardWindow)
	if response := g.do(wrong); response.Code != http.StatusUnauthorized {
		t.Fatal("failures older than the window must not count")
	}
	if response := g.do(wrong); response.Code != http.StatusUnauthorized {
		t.Fatal("the block armed with failures outside the window")
	}
}

func TestAdminRejectsTokenInURL(t *testing.T) {
	f := newAPI(t, nil)
	response := f.do(call{method: http.MethodPost, target: "/admin/v1/session?token=" + f.token, body: `{"token":"` + f.token + `"}`, header: jsonHeader()})
	if response.Code != http.StatusBadRequest || strings.Contains(response.Body.String(), f.token) {
		t.Fatalf("token in the URL: %d %s", response.Code, response.Body)
	}
	response = f.do(call{method: http.MethodDelete, target: "/admin/v1/session?", header: f.bearer()})
	if response.Code != http.StatusBadRequest {
		t.Fatalf("an empty query: %d", response.Code)
	}
}

func TestAdminStrictJSON(t *testing.T) {
	f := newAPI(t, nil)
	marker := "cmpmarker" + strings.Repeat("z", 8)
	cases := []struct {
		body   string
		header http.Header
		status int
		code   string
	}{
		{`{"token":"` + f.token + `","extra":"` + marker + `"}`, jsonHeader(), 400, "invalid_request"},
		{`{"token":"` + f.token + `"}{"token":"x"}`, jsonHeader(), 400, "invalid_request"},
		{`{"token":"` + marker, jsonHeader(), 400, "invalid_request"},
		{`{"token":"` + f.token + `"}`, http.Header{"Content-Type": {"text/plain"}}, 415, "unsupported_media_type"},
		{`{"token":"` + f.token + `"}`, http.Header{}, 415, "unsupported_media_type"},
		{`{"token":"` + strings.Repeat("a", 17<<10) + `"}`, jsonHeader(), 413, "body_too_large"},
		{``, jsonHeader(), 400, "invalid_request"},
	}
	for i, c := range cases {
		response := f.do(call{method: http.MethodPost, target: "/admin/v1/session", body: c.body, header: c.header})
		if response.Code != c.status || errorCode(t, response) != c.code {
			t.Fatalf("case %d: %d %s", i, response.Code, response.Body)
		}
		if strings.Contains(response.Body.String(), marker) || strings.Contains(response.Body.String(), f.token) {
			t.Fatalf("case %d echoed the input", i)
		}
		for name, want := range map[string]string{"Cache-Control": "no-store", "X-Content-Type-Options": "nosniff", "Referrer-Policy": "no-referrer", "Content-Type": "application/json"} {
			if response.Header().Get(name) != want {
				t.Fatalf("case %d header %s=%q", i, name, response.Header().Get(name))
			}
		}
	}
	if response := f.do(call{method: http.MethodPost, target: "/admin/v1/session", body: `{"token":"` + f.token + `"}`, header: http.Header{"Content-Type": {"application/json; charset=utf-8"}}}); response.Code != http.StatusCreated {
		t.Fatalf("a charset parameter: %d", response.Code)
	}
}

func TestAdminMethodAllowlist(t *testing.T) {
	f := newAPI(t, nil)
	response := f.do(call{method: http.MethodGet, target: "/admin/v1/session", header: f.bearer()})
	if response.Code != http.StatusMethodNotAllowed || response.Header().Get("Allow") != "DELETE, POST" {
		t.Fatalf("GET session: %d Allow=%q", response.Code, response.Header().Get("Allow"))
	}
	for _, target := range []string{"/admin/v1/", "/admin/v1/sessions", "/admin/v1/session/", "/admin/v1//session", "/admin/v1/Session"} {
		if response := f.do(call{method: http.MethodPost, target: target, body: `{}`, header: f.bearer()}); response.Code != http.StatusNotFound {
			t.Fatalf("%s: %d", target, response.Code)
		}
	}
}
