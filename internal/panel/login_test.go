package panel_test

import (
	"bufio"
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/admin"
	"github.com/JoaoVictorVM/comporta/internal/panel"
	"github.com/JoaoVictorVM/comporta/internal/panel/paneltest"
	"github.com/JoaoVictorVM/comporta/internal/panel/ui"
)

func TestLoginCreatesExistingSession(t *testing.T) {
	env := paneltest.New(t)
	cookie := env.Login(t)
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/" || cookie.MaxAge != int(admin.SessionLifetime/time.Second) {
		t.Fatalf("cookie attributes: %+v", cookie)
	}
	if !env.Sessions.Valid(cookie.Value) || env.Sessions.Len() != 1 {
		t.Fatal("the login did not create a session in the admin store")
	}
	if _, ok := env.Sessions.Expiry(cookie.Value); !ok {
		t.Fatal("session without expiry")
	}
	// An authenticated visit to the login form goes straight to the panel.
	res, _ := do(t, env, http.MethodGet, "/login", cookie, nil, nil)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/" {
		t.Fatalf("login with a session: %d", res.StatusCode)
	}
	// The form refuses unknown fields and a foreign origin.
	res, _ = do(t, env, http.MethodPost, "/login", nil, url.Values{"token": {paneltest.Token}, "next": {"/x"}}, nil)
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("extra field: %d", res.StatusCode)
	}
	res, _ = do(t, env, http.MethodPost, "/login", nil, url.Values{"token": {paneltest.Token}}, map[string]string{"Origin": "http://evil.example"})
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign origin: %d", res.StatusCode)
	}
}

func TestLoginUsesAdminGuard(t *testing.T) {
	env := paneltest.New(t)
	for range admin.GuardMaxFailures {
		if res, _ := do(t, env, http.MethodPost, "/login", nil, url.Values{"token": {strings.Repeat("a", 64)}}, nil); res.StatusCode != http.StatusUnauthorized && res.StatusCode != http.StatusTooManyRequests {
			t.Fatalf("failure answered %d", res.StatusCode)
		}
	}
	res, _ := do(t, env, http.MethodPost, "/login", nil, url.Values{"token": {strings.Repeat("a", 64)}}, nil)
	if res.StatusCode != http.StatusTooManyRequests || res.Header.Get("Retry-After") == "" {
		t.Fatalf("guard did not arm: %d", res.StatusCode)
	}
	// Even the right token waits for the cooldown.
	res, _ = do(t, env, http.MethodPost, "/login", nil, url.Values{"token": {paneltest.Token}}, nil)
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("blocked address logged in: %d", res.StatusCode)
	}
}

func TestLogoutRequiresOrigin(t *testing.T) {
	env := paneltest.New(t)
	cookie := env.Login(t)
	res, _ := do(t, env, http.MethodPost, "/logout", cookie, url.Values{}, map[string]string{"Origin": ""})
	if res.StatusCode != http.StatusForbidden || !env.Sessions.Valid(cookie.Value) {
		t.Fatalf("logout without origin: %d", res.StatusCode)
	}
	res, _ = do(t, env, http.MethodPost, "/logout", cookie, url.Values{}, map[string]string{"Origin": "http://evil.example"})
	if res.StatusCode != http.StatusForbidden || !env.Sessions.Valid(cookie.Value) {
		t.Fatalf("logout from another origin: %d", res.StatusCode)
	}
	res, _ = do(t, env, http.MethodPost, "/logout", cookie, url.Values{}, nil)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/login" || env.Sessions.Valid(cookie.Value) {
		t.Fatalf("logout: %d", res.StatusCode)
	}
	expired := false
	for _, c := range res.Cookies() {
		expired = expired || (c.Name == admin.SessionCookie && c.MaxAge < 0)
	}
	if !expired {
		t.Fatal("logout did not expire the cookie")
	}
	// Searches carry a user identifier: they need the origin too.
	cookie = env.Login(t)
	res, _ = do(t, env, http.MethodPost, "/panel/users/search", cookie, url.Values{"user": {"x"}}, map[string]string{"Origin": ""})
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("search without origin: %d", res.StatusCode)
	}
}

func TestSessionExpiresDuringSSE(t *testing.T) {
	env := paneltest.New(t)
	now := time.Now()
	expiry := make(chan time.Time)
	live := panel.NewLiveHub(panel.LiveOptions{Now: func() time.Time { return now }, After: func(d time.Duration) <-chan time.Time {
		if d == panel.HeartbeatInterval {
			return nil
		}
		return expiry
	}})
	sessions := admin.NewSessions(func() time.Time { return now })
	handler := panel.NewHandler(panel.HandlerOptions{Service: env.Service, Views: ui.Views{}, Credential: nil, Sessions: sessions, Guard: admin.NewGuard(nil, nil), Live: live})
	id, _, err := sessions.Create()
	if err != nil {
		t.Fatal(err)
	}
	server := httpTestServer(t, handler)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server+"/panel/live", nil)
	req.AddCookie(&http.Cookie{Name: admin.SessionCookie, Value: id})
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content type %q", res.Header.Get("Content-Type"))
	}
	reader := bufio.NewReader(res.Body)
	first := readEvent(t, reader)
	if !strings.Contains(first, "event: refresh") || !strings.Contains(first, "data: overview,apps,requests,events,providers,configuration") {
		t.Fatalf("first event: %q", first)
	}
	expiry <- now
	last := readEvent(t, reader)
	if !strings.Contains(last, "event: session-expired") {
		t.Fatalf("expiry event: %q", last)
	}
	if _, err := reader.ReadString('\n'); err == nil {
		t.Fatal("the stream stayed open after the session expired")
	}
	// Without a session the stream only says so.
	req, _ = http.NewRequestWithContext(ctx, http.MethodGet, server+"/panel/live", nil)
	res2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res2.Body.Close()
	if event := readEvent(t, bufio.NewReader(res2.Body)); !strings.Contains(event, "session-expired") {
		t.Fatalf("stream without session: %q", event)
	}
}

// readEvent reads one server-sent event, skipping comments and the retry
// hint.
func readEvent(t *testing.T, r *bufio.Reader) string {
	t.Helper()
	var lines []string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended: %v (%q)", err, lines)
		}
		line = strings.TrimRight(line, "\n")
		if line == "" {
			if len(lines) > 0 && !strings.HasPrefix(lines[0], "retry:") && !strings.HasPrefix(lines[0], ":") {
				return strings.Join(lines, "\n")
			}
			lines = nil
			continue
		}
		lines = append(lines, line)
	}
}
