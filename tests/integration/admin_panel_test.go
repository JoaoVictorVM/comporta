package integration

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
)

// panelBody is the marked request of the privacy testkit.
var panelBody = gptBody

var panelNoRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// panelLogin opens a panel session with the form, as a browser would.
func (s *served) panelLogin() *http.Cookie {
	s.t.Helper()
	request, _ := http.NewRequest(http.MethodPost, s.adminURL("/login"), strings.NewReader(url.Values{"token": {s.token}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", s.adminURL(""))
	response, err := panelNoRedirect.Do(request)
	if err != nil {
		s.t.Fatal(err)
	}
	_ = response.Body.Close()
	for _, c := range response.Cookies() {
		if c.Name == "comporta_admin_session" {
			if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
				s.t.Fatalf("session cookie attributes: %+v", c)
			}
			return c
		}
	}
	s.t.Fatalf("panel login answered %d without a session", response.StatusCode)
	return nil
}

// panel reads a panel route with a session, posting a form when given.
func (s *served) panel(cookie *http.Cookie, method, path string, form url.Values) (*http.Response, string) {
	s.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	request, _ := http.NewRequest(method, s.adminURL(path), body)
	if form != nil {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		request.Header.Set("Origin", s.adminURL(""))
		request.Header.Set("HX-Request", "true")
	}
	if cookie != nil {
		request.AddCookie(cookie)
	}
	response, err := panelNoRedirect.Do(request)
	if err != nil {
		s.t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	data, _ := io.ReadAll(response.Body)
	return response, string(data)
}

// waitPanel polls a screen until it shows a phrase: records reach storage
// through the asynchronous metering worker.
func (s *served) waitPanel(cookie *http.Cookie, path, phrase string) string {
	s.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, page := s.panel(cookie, http.MethodGet, path, nil)
		if strings.Contains(page, phrase) {
			return page
		}
		if time.Now().After(deadline) {
			s.t.Fatalf("%s never showed %q", path, phrase)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func budgetedBot(text string) string {
	return strings.Replace(text, "  bot:\n    allowed_models: [fake-claude, fake-gpt]\n", "  bot:\n    allowed_models: [fake-claude, fake-gpt]\n    budget: {usd_per_day: 5}\n    per_user:\n      budget: {usd_per_day: 1}\n", 1)
}

func TestAllSevenScreensRender(t *testing.T) {
	s := startServedWith(t, fakeprovider.Options{}, budgetedBot)
	for _, user := range []string{"alice", "bob"} {
		if status, body := s.call("bot", user, panelBody); status != http.StatusOK {
			t.Fatalf("traffic: %d %s", status, body)
		}
	}
	cookie := s.panelLogin()
	s.waitPanel(cookie, "/requests", "fake-gpt")
	for path, phrase := range map[string]string{
		"/":              "Estimated cost today",
		"/apps":          "Tightest limit",
		"/apps/bot":      "apps.bot.budget.usd_per_day",
		"/users":         "End user ID",
		"/requests":      "fake-gpt",
		"/events":        "Events",
		"/providers":     "openai",
		"/configuration": "Effective configuration",
	} {
		response, page := s.panel(cookie, http.MethodGet, path, nil)
		if response.StatusCode != http.StatusOK || !strings.Contains(page, phrase) {
			t.Fatalf("%s: %d without %q", path, response.StatusCode, phrase)
		}
		if response.Header.Get("Content-Security-Policy") == "" || strings.Contains(response.Header.Get("Content-Security-Policy"), "unsafe-inline") || response.Header.Get("X-Frame-Options") != "DENY" {
			t.Fatalf("%s security headers: %v", path, response.Header)
		}
		if strings.Contains(page, s.token) {
			t.Fatalf("%s shows the admin token", path)
		}
	}
}

func TestPanelUsesConfigUsageLimitsPoliciesAndPause(t *testing.T) {
	s := startServedWith(t, fakeprovider.Options{}, budgetedBot)
	if status, body := s.call("bot", "carol", panelBody); status != http.StatusOK {
		t.Fatalf("traffic: %d %s", status, body)
	}
	cookie := s.panelLogin()
	page := s.waitPanel(cookie, "/apps/bot", "fake-gpt")
	for _, phrase := range []string{"apps.bot.budget.usd_per_day", "renews in", "block"} {
		if !strings.Contains(page, phrase) {
			t.Fatalf("app screen lacks %q", phrase)
		}
	}
	// A user's limits come from the coordinator for the user's own pseudonym.
	_, result := s.panel(cookie, http.MethodPost, "/panel/users/search", url.Values{"user": {"carol"}, "app": {"bot"}})
	if !strings.Contains(result, "apps.bot.per_user.budget.usd_per_day") || strings.Contains(result, "carol") {
		t.Fatal("the user search lacks the per-user limit or echoes the id")
	}
	// A rejected reload keeps the generation and shows the error.
	data, _ := os.ReadFile(s.path)
	if err := os.WriteFile(s.path, append(data, []byte("\nunknown_key: [\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	s.waitPanel(cookie, "/configuration", "The last reload was rejected")
	if status, body := s.admin(http.MethodPost, "/admin/v1/pause", `{"app":"bot"}`); status != http.StatusOK {
		t.Fatalf("pause: %d %s", status, body)
	}
	s.waitPanel(cookie, "/apps/bot", "Paused: this app refuses requests")
}

func TestPanelLiveRefresh(t *testing.T) {
	s := startServed(t, fakeprovider.Options{})
	cookie := s.panelLogin()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, s.adminURL("/panel/live"), nil)
	request.AddCookie(cookie)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	events := make(chan string, 16)
	go func() {
		reader := bufio.NewReader(response.Body)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				close(events)
				return
			}
			if strings.HasPrefix(line, "data: ") {
				events <- strings.TrimSpace(strings.TrimPrefix(line, "data: "))
			}
		}
	}()
	if first := <-events; !strings.Contains(first, "requests") {
		t.Fatalf("the first event is not a full refresh: %q", first)
	}
	if status, body := s.call("bot", "dave", panelBody); status != http.StatusOK {
		t.Fatalf("traffic: %d %s", status, body)
	}
	for {
		select {
		case data, ok := <-events:
			if !ok {
				t.Fatal("the stream closed")
			}
			if strings.Contains(data, "requests") {
				if strings.ContainsAny(data, " {}\"") {
					t.Fatalf("the stream carried more than scope names: %q", data)
				}
				return
			}
		case <-ctx.Done():
			t.Fatal("a new request did not refresh the requests screen")
		}
	}
}

func TestPanelAbsentFromProxy(t *testing.T) {
	s := startServed(t, fakeprovider.Options{})
	for _, path := range []string{"/", "/login", "/apps", "/panel/live", "/assets/x/app.js"} {
		response, err := http.Get(s.proxyURL(path))
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if strings.Contains(string(data), "<html") || response.StatusCode == http.StatusSeeOther || strings.Contains(response.Header.Get("Content-Type"), "text/html") {
			t.Fatalf("%s reached the panel on the proxy listener: %d", path, response.StatusCode)
		}
	}
	// Login on the proxy port is refused even with the right token.
	request, _ := http.NewRequest(http.MethodPost, s.proxyURL("/login"), strings.NewReader(url.Values{"token": {s.token}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := panelNoRedirect.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if len(response.Cookies()) != 0 {
		t.Fatal("the proxy listener issued a session")
	}
}

var shortHash = regexp.MustCompile(`u_[0-9a-f]{8}`)

func TestUserSearchFindsHash(t *testing.T) {
	s := startServed(t, fakeprovider.Options{})
	if status, body := s.call("bot", "erin-42", panelBody); status != http.StatusOK {
		t.Fatalf("traffic: %d %s", status, body)
	}
	if status, body := s.call("bot", "someone-else", panelBody); status != http.StatusOK {
		t.Fatalf("traffic: %d %s", status, body)
	}
	cookie := s.panelLogin()
	s.waitPanel(cookie, "/requests", "fake-gpt")
	_, filtered := s.panel(cookie, http.MethodPost, "/panel/requests/filter", url.Values{"app": {"bot"}, "user": {"erin-42"}})
	rows := shortHash.FindAllString(filtered, -1)
	if len(rows) == 0 {
		t.Fatal("the user filter found no request")
	}
	_, result := s.panel(cookie, http.MethodPost, "/panel/users/search", url.Values{"user": {"erin-42"}})
	found := shortHash.FindAllString(result, -1)
	if len(found) == 0 || found[0] != rows[0] || strings.Contains(result, "erin-42") {
		t.Fatalf("search found %v, requests show %v", found, rows)
	}
	// The hostile identifier an app could send is refused by the proxy and
	// never rendered by the panel.
	status, _ := s.call("bot", "<script>alert(1)</script>", panelBody)
	if status == http.StatusOK {
		t.Fatal("the proxy accepted an identifier with markup")
	}
	_, page := s.panel(cookie, http.MethodPost, "/panel/users/search", url.Values{"user": {"<script>alert(1)</script>"}})
	if strings.Contains(page, "<script>alert") || strings.Contains(page, "alert(1)") {
		t.Fatal("the search echoed markup")
	}
}

func TestPausedBanner(t *testing.T) {
	s := startServed(t, fakeprovider.Options{})
	cookie := s.panelLogin()
	if status, body := s.admin(http.MethodPost, "/admin/v1/pause", `{"app":"bot"}`); status != http.StatusOK {
		t.Fatalf("pause: %d %s", status, body)
	}
	for _, path := range []string{"/", "/apps"} {
		page := s.waitPanel(cookie, path, "Paused apps refuse every request")
		if !strings.Contains(page, "bg-bad") || !strings.Contains(page, `href="/apps/bot"`) {
			t.Fatalf("%s banner does not name the app in red", path)
		}
	}
	if status, body := s.admin(http.MethodPost, "/admin/v1/pause", `{"all":true}`); status != http.StatusOK {
		t.Fatalf("pause all: %d %s", status, body)
	}
	s.waitPanel(cookie, "/", "every app is paused by an operator")
}
