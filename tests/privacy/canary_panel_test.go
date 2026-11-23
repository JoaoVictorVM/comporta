package privacy

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/storage/sqlite"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

var panelLinks = regexp.MustCompile(`(?:href|src)="(/[^"]*)"`)

// TestPanelCanaryAbsentFromHTMLSSEAssetsAndLogs reads every screen, fragment,
// search answer, detail, the live stream and the assets after marked traffic,
// and requires that no prompt, raw user identifier or token reaches them.
func TestPanelCanaryAbsentFromHTMLSSEAssetsAndLogs(t *testing.T) {
	run.markCase("process/admin-panel")
	g := startLive(t, "success", "")
	defer g.stop()
	serveStdout, serveStderr := g.logs[2], g.logs[3]
	var token string
	for deadline := time.Now().Add(10 * time.Second); token == "" && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		data, _ := os.ReadFile(serveStdout)
		if match := regexp.MustCompile(`store it now\): ([0-9a-f]{64})`).FindSubmatch(data); match != nil {
			token = string(match[1])
		}
	}
	if token == "" {
		t.Fatal("the admin token was not shown on the first start")
	}
	run.markConstructor("AnthropicBody")
	user := testkit.UserMarker()
	if status := g.send("panel-traffic", "/v1/messages", g.key, user, testkit.AnthropicBody()); status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	reader, err := sqlite.OpenReader(context.Background(), filepath.Join(g.dir, "data"))
	if err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		page, err := reader.Records(context.Background(), storage.RecordFilter{AppName: "bot", Limit: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Records) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("positive control: the request was not recorded")
		}
	}
	reader.Close()

	base := "http://127.0.0.1:" + itoa(g.adminPort)
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	tokens := testkit.NewScanner(token)
	var cookie *http.Cookie
	fetch := func(name, method, path string, form url.Values) []byte {
		t.Helper()
		var body io.Reader
		if form != nil {
			body = strings.NewReader(form.Encode())
		}
		request, err := http.NewRequest(method, base+path, body)
		if err != nil {
			t.Fatal(err)
		}
		if form != nil {
			request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			request.Header.Set("Origin", base)
		}
		if cookie != nil {
			request.AddCookie(cookie)
		}
		response, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		data, _ := io.ReadAll(response.Body)
		run.scanner.Scan("panel", name, data)
		tokens.Scan("panel", name, data)
		return data
	}
	fetch("panel-login-form", http.MethodGet, "/login", nil)
	fetch("panel-login-wrong", http.MethodPost, "/login", url.Values{"token": {user}})
	request, _ := http.NewRequest(http.MethodPost, base+"/login", strings.NewReader(url.Values{"token": {token}}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Origin", base)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	for _, c := range response.Cookies() {
		if c.Name == "comporta_admin_session" {
			cookie = c
		}
	}
	if cookie == nil || strings.Contains(response.Header.Get("Location"), token) {
		t.Fatalf("positive control: login %d", response.StatusCode)
	}
	links := map[string]bool{}
	for _, path := range []string{"/", "/apps", "/apps/bot", "/users", "/requests", "/events", "/providers", "/configuration",
		"/panel/fragment/overview", "/panel/fragment/apps", "/panel/fragment/providers", "/panel/fragment/configuration", "/panel/fragment/apps/bot"} {
		page := fetch("panel-page", http.MethodGet, path, nil)
		for _, match := range panelLinks.FindAllSubmatch(page, -1) {
			links[string(match[1])] = true
		}
	}
	detail := false
	for link := range links {
		if strings.HasPrefix(link, "/requests/") || strings.HasPrefix(link, "/assets/") {
			fetch("panel-linked", http.MethodGet, link, nil)
			detail = detail || strings.HasPrefix(link, "/requests/")
		}
	}
	if !detail {
		t.Fatal("positive control: the requests screen links no detail")
	}
	result := fetch("panel-search", http.MethodPost, "/panel/users/search", url.Values{"user": {user}})
	if !strings.Contains(string(result), "u_") {
		t.Fatal("positive control: the search found no pseudonym")
	}
	fetch("panel-search-app", http.MethodPost, "/panel/users/search", url.Values{"user": {user}, "app": {"bot"}})
	fetch("panel-filter", http.MethodPost, "/panel/requests/filter", url.Values{"app": {"bot"}, "user": {user}})
	fetch("panel-events", http.MethodPost, "/panel/events/filter", url.Values{"app": {"bot"}})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/panel/live", nil)
	stream.AddCookie(cookie)
	response, err = http.DefaultClient.Do(stream)
	if err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewReader(response.Body)
	var seen []byte
	for !strings.Contains(string(seen), "event: refresh") {
		line, err := lines.ReadBytes('\n')
		if err != nil {
			t.Fatal("positive control: the live stream sent no refresh")
		}
		seen = append(seen, line...)
	}
	cancel()
	_ = response.Body.Close()
	run.scanner.Scan("panel", "panel-live", seen)
	tokens.Scan("panel", "panel-live", seen)
	g.stop()
	run.scanner.ScanFile("logs", serveStderr)
	tokens.ScanFile("logs", serveStderr)
	if err := tokens.Err("panel", "logs"); err != nil {
		t.Fatalf("the admin token leaked: %v", err)
	}
}
