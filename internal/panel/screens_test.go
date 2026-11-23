package panel_test

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/JoaoVictorVM/comporta/internal/panel"
	"github.com/JoaoVictorVM/comporta/internal/panel/paneltest"
	"github.com/JoaoVictorVM/comporta/internal/panel/ui"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

const hostile = `<script>alert("x")</script><img src=x onerror=alert(1)>`

// noInjectedMarkup fails when a page holds a script element that is not one
// of the embedded assets, or any element the hostile input would create.
func noInjectedMarkup(t *testing.T, page string) {
	t.Helper()
	doc := parse(t, page)
	walk(doc, func(n *html.Node) {
		if n.Type != html.ElementNode {
			return
		}
		if n.Data == "script" {
			if src, _ := attr(n, "src"); !strings.HasPrefix(src, "/assets/") {
				t.Fatalf("injected script element: %q", src)
			}
		}
		if n.Data == "img" {
			t.Fatal("injected img element")
		}
		for _, a := range n.Attr {
			if strings.HasPrefix(a.Key, "on") {
				t.Fatalf("injected handler %s", a.Key)
			}
		}
	})
}

func TestEffectiveYAMLIsEscapedAndMasked(t *testing.T) {
	t.Setenv("PANEL_TEST_SECRET", "panel-secret-canary-value")
	env := paneltest.NewWith(t, strings.Replace(paneltest.Config, "    base_url: http://localhost:12345\n", "    base_url: http://localhost:12345\n    api_key: ${PANEL_TEST_SECRET}\n", 1))
	cookie := env.Login(t)
	_, page := do(t, env, http.MethodGet, "/configuration", cookie, nil, nil)
	if strings.Contains(page, "panel-secret-canary-value") || strings.Contains(page, "PANEL_TEST_SECRET") && !strings.Contains(env.Manager.Capture().EffectiveYAML(), "PANEL_TEST_SECRET") {
		t.Fatal("the configuration screen exposes a secret")
	}
	pres := elements(parse(t, page), "pre")
	if len(pres) != 1 || text(pres[0]) != env.Manager.Capture().EffectiveYAML() {
		t.Fatal("the effective YAML is not shown as text")
	}
	var out bytes.Buffer
	if err := (ui.Views{}).ConfigurationFragment(panel.Configuration{Available: true, YAML: "name: " + hostile}).Render(context.Background(), &out); err != nil {
		t.Fatal(err)
	}
	noInjectedMarkup(t, out.String())
	if pres := elements(parse(t, out.String()), "pre"); len(pres) != 1 || text(pres[0]) != "name: "+hostile {
		t.Fatal("hostile YAML text was altered instead of escaped")
	}
}

func TestHostileInputRenderedEscaped(t *testing.T) {
	env := paneltest.New(t)
	paneltest.Seed(t, env)
	cookie := env.Login(t)
	for _, c := range []struct {
		target string
		form   url.Values
	}{
		{"/panel/users/search", url.Values{"user": {hostile}}},
		{"/panel/users/search", url.Values{"user": {hostile}, "app": {"bot"}}},
		{"/panel/requests/filter", url.Values{"app": {"bot"}, "user": {hostile}}},
		{"/panel/requests/filter", url.Values{"model": {hostile}}},
		{"/panel/events/filter", url.Values{"type": {hostile}}},
	} {
		for _, fragment := range []string{"true", ""} {
			res, page := do(t, env, http.MethodPost, c.target, cookie, c.form, map[string]string{"HX-Request": fragment})
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("%s %v: %d", c.target, c.form, res.StatusCode)
			}
			noInjectedMarkup(t, page)
			if strings.Contains(page, `alert("x")`) || strings.Contains(page, "alert(1)") || strings.Contains(page, "&lt;script") {
				t.Fatalf("%s echoed the hostile input", c.target)
			}
		}
	}
}

func TestAllScreensRenderSeeded(t *testing.T) {
	env := paneltest.New(t)
	users := paneltest.Seed(t, env)
	env.Pause.Set(storage.PauseState{Apps: []storage.AppPause{{App: "web", Paused: true}}})
	cookie := env.Login(t)
	want := map[string][]string{
		"/":              {"Estimated cost today", "bot", "web", "Paused", "From the response cache", "Alerts are not enabled yet."},
		"/apps":          {"Tightest limit", "downgrade", "Paused"},
		"/apps/bot":      {"apps.bot.budget.usd_per_day", "renews in", "Model mix", "Top 10 users", "u_"},
		"/users":         {"End user ID", "Anonymous bucket of app"},
		"/requests":      {"Recent requests", "cached", "downgraded", "blocked", "usage missing", "sensitive data"},
		"/events":        {"Loop cut", "Fallback", "Pause", "Blocked", "claude-a"},
		"/providers":     {"anthropic", "openai", "Unhealthy", "p95 latency ms"},
		"/configuration": {"Effective configuration", "Active generation"},
	}
	for target, phrases := range want {
		res, page := do(t, env, http.MethodGet, target, cookie, nil, nil)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d", target, res.StatusCode)
		}
		for _, phrase := range phrases {
			if !strings.Contains(page, phrase) {
				t.Fatalf("%s lacks %q", target, phrase)
			}
		}
		for _, raw := range users {
			if strings.Contains(page, raw) {
				t.Fatalf("%s shows a raw user id", target)
			}
		}
	}
	// A detail link from the list opens the metadata page.
	_, list := do(t, env, http.MethodGet, "/requests", cookie, nil, nil)
	var detail string
	walk(parse(t, list), func(n *html.Node) {
		if href, ok := attr(n, "href"); ok && strings.HasPrefix(href, "/requests/") && detail == "" {
			detail = href
		}
	})
	res, page := do(t, env, http.MethodGet, detail, cookie, nil, nil)
	if res.StatusCode != http.StatusOK || !strings.Contains(page, "Metadata only") {
		t.Fatalf("detail %s: %d", detail, res.StatusCode)
	}
	// A search by raw id answers with the pseudonym and never the id.
	res, page = do(t, env, http.MethodPost, "/panel/users/search", cookie, url.Values{"user": {users[0]}}, map[string]string{"HX-Request": "true"})
	hash, _ := env.Identity.LookupHash("bot", users[0])
	if res.StatusCode != http.StatusOK || !strings.Contains(page, panel.ShortHash(hash.Hex())) || strings.Contains(page, users[0]) {
		t.Fatalf("search: %d", res.StatusCode)
	}
}
