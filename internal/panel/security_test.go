package panel_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/JoaoVictorVM/comporta/internal/panel"
	"github.com/JoaoVictorVM/comporta/internal/panel/paneltest"
)

var screens = []string{"/", "/apps", "/apps/bot", "/users", "/requests", "/events", "/providers", "/configuration"}

func TestPanelSecurityHeaders(t *testing.T) {
	env := paneltest.New(t)
	paneltest.Seed(t, env)
	cookie := env.Login(t)
	targets := append(append([]string{}, screens...), "/panel/fragment/overview", "/panel/fragment/apps", "/panel/fragment/providers", "/panel/fragment/configuration", "/panel/fragment/apps/bot")
	for _, target := range targets {
		res, _ := do(t, env, http.MethodGet, target, cookie, nil, nil)
		if res.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d", target, res.StatusCode)
		}
		h := res.Header
		if h.Get("Content-Security-Policy") != panel.ContentSecurityPolicy || h.Get("X-Frame-Options") != "DENY" || h.Get("X-Content-Type-Options") != "nosniff" ||
			h.Get("Referrer-Policy") != "no-referrer" || h.Get("Cache-Control") != "no-store" || h.Get("Content-Type") != "text/html; charset=utf-8" {
			t.Fatalf("%s headers: %v", target, h)
		}
	}
	res, _ := do(t, env, http.MethodGet, "/login", nil, nil, nil)
	if res.Header.Get("Content-Security-Policy") != panel.ContentSecurityPolicy || res.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatal("login page without security headers")
	}
}

func TestCSPHasNoUnsafeDirectives(t *testing.T) {
	csp := panel.ContentSecurityPolicy
	for _, forbidden := range []string{"unsafe-inline", "unsafe-eval", "*", "http:", "https:"} {
		if strings.Contains(csp, forbidden) {
			t.Fatalf("CSP allows %s", forbidden)
		}
	}
	for _, required := range []string{"script-src 'self'", "style-src 'self'", "frame-ancestors 'none'", "object-src 'none'", "base-uri 'none'", "form-action 'self'"} {
		if !strings.Contains(csp, required) {
			t.Fatalf("CSP lacks %s", required)
		}
	}
	// Nothing rendered needs an exception: no inline script, inline style,
	// event handler attribute or javascript URL anywhere.
	env := paneltest.New(t)
	paneltest.Seed(t, env)
	cookie := env.Login(t)
	pages := []string{}
	for _, target := range screens {
		_, body := do(t, env, http.MethodGet, target, cookie, nil, nil)
		pages = append(pages, body)
	}
	_, login := do(t, env, http.MethodGet, "/login", nil, nil, nil)
	pages = append(pages, login)
	for _, page := range pages {
		doc := parse(t, page)
		walk(doc, func(n *html.Node) {
			if n.Type != html.ElementNode {
				return
			}
			if n.Data == "style" {
				t.Fatal("inline style element")
			}
			if src, ok := attr(n, "src"); n.Data == "script" && (!ok || !strings.HasPrefix(src, "/assets/")) {
				t.Fatalf("script not from the embedded assets: %q", src)
			}
			if n.Data == "script" && strings.TrimSpace(text(n)) != "" {
				t.Fatal("inline script")
			}
			for _, a := range n.Attr {
				if a.Key == "style" || strings.HasPrefix(a.Key, "on") || strings.HasPrefix(a.Key, "hx-on") || strings.Contains(strings.ToLower(a.Val), "javascript:") || strings.HasPrefix(a.Val, "js:") {
					t.Fatalf("executable or inline attribute %s=%q on <%s>", a.Key, a.Val, n.Data)
				}
				if (a.Key == "href" || a.Key == "src") && (strings.HasPrefix(a.Val, "http") || strings.HasPrefix(a.Val, "//")) {
					t.Fatalf("external reference %s", a.Val)
				}
			}
		})
	}
}

func TestLoginNeverEchoesToken(t *testing.T) {
	env := paneltest.New(t)
	wrong := strings.Repeat("f", 64)
	res, body := do(t, env, http.MethodPost, "/login", nil, url.Values{"token": {wrong}}, nil)
	if res.StatusCode != http.StatusUnauthorized || strings.Contains(body, wrong) || !strings.Contains(body, "Invalid admin token.") {
		t.Fatalf("failed login: %d", res.StatusCode)
	}
	res, body = do(t, env, http.MethodPost, "/login", nil, url.Values{"token": {paneltest.Token}}, nil)
	if res.StatusCode != http.StatusSeeOther || strings.Contains(body, paneltest.Token) || strings.Contains(res.Header.Get("Location"), paneltest.Token) {
		t.Fatalf("successful login: %d %q", res.StatusCode, res.Header.Get("Location"))
	}
	for _, c := range res.Cookies() {
		if strings.Contains(c.Value, paneltest.Token) {
			t.Fatal("the token became the cookie")
		}
	}
	// The token is never accepted from the URL.
	res, body = do(t, env, http.MethodPost, "/login?token="+paneltest.Token, nil, url.Values{}, nil)
	if res.StatusCode != http.StatusBadRequest || strings.Contains(body, paneltest.Token) {
		t.Fatalf("token in the query: %d", res.StatusCode)
	}
}

func TestAssetsHaveFixedMIME(t *testing.T) {
	env := paneltest.New(t)
	for name, mime := range map[string]string{
		"app.css": "text/css; charset=utf-8", "app.js": "text/javascript; charset=utf-8", "htmx.min.js": "text/javascript; charset=utf-8",
		"htmx-ext-sse.min.js": "text/javascript; charset=utf-8", "uPlot.iife.min.js": "text/javascript; charset=utf-8", "uPlot.min.css": "text/css; charset=utf-8",
	} {
		target := panel.AssetURL(name)
		res, _ := do(t, env, http.MethodGet, target, nil, nil, nil)
		if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != mime || res.Header.Get("X-Content-Type-Options") != "nosniff" ||
			!strings.Contains(res.Header.Get("Cache-Control"), "immutable") || res.Header.Get("ETag") == "" {
			t.Fatalf("%s: %d %v", name, res.StatusCode, res.Header)
		}
	}
	for _, target := range []string{"/assets/0000000000000000/app.js", "/assets/" + strings.Split(panel.AssetURL("app.js"), "/")[2] + "/manifest.json", "/assets/x/../app.js", "/assets/vendor/htmx.min.js"} {
		if res, _ := do(t, env, http.MethodGet, target, nil, nil, nil); res.StatusCode != http.StatusNotFound {
			t.Fatalf("%s: %d", target, res.StatusCode)
		}
	}
}

// TestVendoredAssetsMatchManifest keeps every third-party file identical to
// the published one its manifest names.
func TestVendoredAssetsMatchManifest(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("assets", "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Packages []struct {
			Name, Version, License, Source, Integrity string
			Files                                     []struct{ Path, From, SHA256 string }
		}
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, p := range manifest.Packages {
		if p.Version == "" || p.License == "" || !strings.HasPrefix(p.Source, "https://registry.npmjs.org/") || !strings.HasPrefix(p.Integrity, "sha512-") {
			t.Fatalf("incomplete entry %+v", p)
		}
		for _, f := range p.Files {
			content, err := os.ReadFile(filepath.Join("assets", filepath.FromSlash(f.Path)))
			if err != nil {
				t.Fatal(err)
			}
			sum := sha256.Sum256(content)
			if hex.EncodeToString(sum[:]) != f.SHA256 {
				t.Fatalf("%s differs from the manifest", f.Path)
			}
			listed[f.Path] = true
		}
	}
	_ = filepath.WalkDir(filepath.Join("assets", "vendor"), func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			rel, _ := filepath.Rel("assets", path)
			if !listed[filepath.ToSlash(rel)] {
				t.Fatalf("%s is not in the manifest", rel)
			}
		}
		return nil
	})
	// Our own script never evaluates code or reaches another origin.
	own, _ := os.ReadFile(filepath.Join("assets", "app.js"))
	if regexp.MustCompile(`\beval\(|new Function|innerHTML|https?://`).Match(own) {
		t.Fatal("app.js evaluates code, writes HTML or names an external origin")
	}
}
