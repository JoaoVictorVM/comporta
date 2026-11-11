package admin

import (
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func loginCookie(t *testing.T, f *apiFixture) *http.Cookie {
	t.Helper()
	response := f.do(call{method: http.MethodPost, target: "/admin/v1/session", body: `{"token":"` + f.token + `"}`, header: jsonHeader("Origin", testOrigin)})
	if response.Code != http.StatusCreated {
		t.Fatalf("login: %d %s", response.Code, response.Body)
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("login set %d cookies", len(cookies))
	}
	return cookies[0]
}

func TestSessionCookieFlags(t *testing.T) {
	f := newAPI(t, nil)
	cookie := loginCookie(t, f)
	if cookie.Name != SessionCookie || cookie.Path != "/" || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.MaxAge != 43200 {
		t.Fatalf("cookie flags: %+v", cookie)
	}
	if cookie.Secure {
		t.Fatal("Secure without TLS on the connection")
	}
	if strings.Contains(cookie.Value, f.token) {
		t.Fatal("the session carries the token")
	}
}

func TestSessionExpiresAtTwelveHours(t *testing.T) {
	clock := newFakeClock()
	sessions := NewSessions(clock.Now)
	id, expires, err := sessions.Create()
	if err != nil {
		t.Fatal(err)
	}
	if !expires.Equal(clock.Now().Add(12 * time.Hour)) {
		t.Fatalf("expiry %v", expires)
	}
	clock.advance(12*time.Hour - time.Nanosecond)
	if !sessions.Valid(id) {
		t.Fatal("session ended early")
	}
	clock.advance(time.Nanosecond)
	if sessions.Valid(id) {
		t.Fatal("use must not renew a session past its absolute expiry")
	}
}

func TestLogoutInvalidatesSession(t *testing.T) {
	f := newAPI(t, nil)
	cookie := loginCookie(t, f)
	header := http.Header{"Cookie": {cookie.Name + "=" + cookie.Value}, "Origin": {testOrigin}}
	response := f.do(call{method: http.MethodDelete, target: "/admin/v1/session", header: header})
	if response.Code != http.StatusNoContent {
		t.Fatalf("logout: %d %s", response.Code, response.Body)
	}
	expired := response.Result().Cookies()
	if len(expired) != 1 || expired[0].MaxAge >= 0 {
		t.Fatal("logout must expire the cookie")
	}
	again := f.do(call{method: http.MethodDelete, target: "/admin/v1/session", header: header})
	if again.Code != http.StatusUnauthorized {
		t.Fatalf("a logged out session still works: %d", again.Code)
	}
}

func TestSessionOriginRequired(t *testing.T) {
	f := newAPI(t, nil)
	cookie := loginCookie(t, f)
	withCookie := func(origin ...string) http.Header {
		header := http.Header{"Cookie": {cookie.Name + "=" + cookie.Value}}
		for _, value := range origin {
			header.Add("Origin", value)
		}
		return header
	}
	if response := f.do(call{method: http.MethodDelete, target: "/admin/v1/session", header: withCookie()}); response.Code != http.StatusForbidden || errorCode(t, response) != "origin_not_allowed" {
		t.Fatalf("a cookie state change without Origin: %d", response.Code)
	}
	if response := f.do(call{method: http.MethodDelete, target: "/admin/v1/session", header: withCookie("http://evil.example")}); response.Code != http.StatusForbidden {
		t.Fatalf("a foreign Origin: %d", response.Code)
	}
	login := f.do(call{method: http.MethodPost, target: "/admin/v1/session", body: `{"token":"` + f.token + `"}`, header: jsonHeader("Origin", "http://evil.example")})
	if login.Code != http.StatusForbidden {
		t.Fatalf("login from a foreign Origin: %d", login.Code)
	}
	bearer := f.do(call{method: http.MethodPost, target: "/admin/v1/session", body: `{"token":"` + f.token + `"}`, header: jsonHeader()})
	if bearer.Code != http.StatusCreated {
		t.Fatalf("login without Origin, as a non-browser client: %d", bearer.Code)
	}
}

func TestSessionCapacity(t *testing.T) {
	clock := newFakeClock()
	sessions := NewSessions(clock.Now)
	for range MaxSessions {
		if _, _, err := sessions.Create(); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := sessions.Create(); !errors.Is(err, ErrSessionsFull) {
		t.Fatalf("over capacity: %v", err)
	}
	clock.advance(SessionLifetime)
	if _, _, err := sessions.Create(); err != nil {
		t.Fatalf("expired sessions must free capacity: %v", err)
	}
	if sessions.Len() != 1 {
		t.Fatalf("sweep left %d sessions", sessions.Len())
	}
}
