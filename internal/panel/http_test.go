package panel_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/panel/paneltest"
	"github.com/JoaoVictorVM/comporta/internal/proxy"
	"github.com/JoaoVictorVM/comporta/internal/server"
)

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

// do sends a request with an optional session cookie and form body.
func do(t *testing.T, env *paneltest.Env, method, target string, cookie *http.Cookie, form url.Values, headers map[string]string) (*http.Response, string) {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, env.Server.URL+target, body)
	if err != nil {
		t.Fatal(err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", env.Server.URL)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	for k, v := range headers {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	res, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	return res, string(data)
}

func parse(t *testing.T, page string) *html.Node {
	t.Helper()
	doc, err := html.Parse(strings.NewReader(page))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func walk(n *html.Node, fn func(*html.Node)) {
	fn(n)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		walk(c, fn)
	}
}

func attr(n *html.Node, name string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val, true
		}
	}
	return "", false
}

func elements(doc *html.Node, tag string) []*html.Node {
	var out []*html.Node
	walk(doc, func(n *html.Node) {
		if n.Type == html.ElementNode && n.Data == tag {
			out = append(out, n)
		}
	})
	return out
}

func text(n *html.Node) string {
	var b strings.Builder
	walk(n, func(c *html.Node) {
		if c.Type == html.TextNode {
			b.WriteString(c.Data)
		}
	})
	return b.String()
}

type routeSource struct{}

func (routeSource) ServeRoute(w http.ResponseWriter, _ *http.Request, _ proxy.Route, _ *config.Snapshot) {
	w.WriteHeader(http.StatusTeapot)
}

func TestPanelRoutesOnlyOnAdmin(t *testing.T) {
	env := paneltest.New(t)
	admin := server.AdminSurfacePanel(env.Manager, nil, nil, nil, env.Handler)
	proxySurface := server.ProxySurface(env.Manager, routeSource{})
	for _, target := range []string{"/", "/login", "/apps", "/panel/live", "/requests"} {
		rec := httptest.NewRecorder()
		proxySurface.ServeHTTP(rec, localRequest(target))
		if rec.Code == http.StatusSeeOther || strings.Contains(rec.Body.String(), "<html") {
			t.Fatalf("%s reached the panel on the proxy listener: %d", target, rec.Code)
		}
		rec = httptest.NewRecorder()
		admin.ServeHTTP(rec, localRequest(target))
		if rec.Code == http.StatusNotFound || rec.Code == http.StatusBadRequest {
			t.Fatalf("%s missing from the admin listener", target)
		}
	}
	// The health route keeps its answer and an unknown path keeps the fixed
	// not-found of the admin listener.
	rec := httptest.NewRecorder()
	admin.ServeHTTP(rec, localRequest("/healthz"))
	if rec.Body.String() != `{"status":"ok"}` {
		t.Fatalf("healthz: %s", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	admin.ServeHTTP(rec, localRequest("/unknown"))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "endpoint_not_allowed") {
		t.Fatalf("unknown path: %d %s", rec.Code, rec.Body.String())
	}
}

func TestProtectedPageRedirects(t *testing.T) {
	env := paneltest.New(t)
	for _, target := range []string{"/", "/apps", "/apps/bot", "/users", "/requests", "/events", "/providers", "/configuration", "/requests/0123456789abcdef0123456789abcdef/1", "/panel/fragment/overview"} {
		res, body := do(t, env, http.MethodGet, target, nil, nil, nil)
		if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/login" {
			t.Fatalf("%s: %d %q", target, res.StatusCode, res.Header.Get("Location"))
		}
		if strings.Contains(body, target) && target != "/" {
			t.Fatalf("%s: the body names the protected route", target)
		}
	}
	// A bearer token never opens a page.
	res, _ := do(t, env, http.MethodGet, "/", nil, nil, map[string]string{"Authorization": "Bearer " + paneltest.Token})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("bearer opened a page: %d", res.StatusCode)
	}
	cookie := env.Login(t)
	res, _ = do(t, env, http.MethodGet, "/", cookie, nil, nil)
	if res.StatusCode != http.StatusOK {
		t.Fatalf("with a session: %d", res.StatusCode)
	}
}

func TestHTMXSessionExpiry(t *testing.T) {
	env := paneltest.New(t)
	res, body := do(t, env, http.MethodGet, "/panel/fragment/overview", nil, nil, map[string]string{"HX-Request": "true"})
	if res.StatusCode != http.StatusUnauthorized || res.Header.Get("HX-Redirect") != "/login" || body != "" {
		t.Fatalf("fragment without session: %d %q %q", res.StatusCode, res.Header.Get("HX-Redirect"), body)
	}
	cookie := env.Login(t)
	env.Sessions.Delete(cookie.Value)
	res, _ = do(t, env, http.MethodPost, "/panel/requests/filter", cookie, url.Values{"app": {"bot"}}, map[string]string{"HX-Request": "true"})
	if res.StatusCode != http.StatusUnauthorized || res.Header.Get("HX-Redirect") != "/login" {
		t.Fatalf("expired session: %d", res.StatusCode)
	}
}

func TestMethodsAndPathsAreExact(t *testing.T) {
	env := paneltest.New(t)
	cookie := env.Login(t)
	for _, c := range []struct {
		method, target string
		want           int
	}{
		{http.MethodGet, "/apps/", http.StatusNotFound},
		{http.MethodGet, "/apps/./bot", http.StatusNotFound},
		{http.MethodGet, "/apps/%62ot", http.StatusNotFound},
		{http.MethodGet, "/apps/Bot", http.StatusNotFound},
		{http.MethodGet, "//apps", http.StatusNotFound},
		{http.MethodGet, "/apps/bot/extra", http.StatusNotFound},
		{http.MethodGet, "/panel/fragment/users", http.StatusNotFound},
		{http.MethodGet, "/requests/0123/1", http.StatusNotFound},
		{http.MethodGet, "/requests/0123456789abcdef0123456789abcdef/9", http.StatusNotFound},
		{http.MethodPost, "/apps", http.StatusMethodNotAllowed},
		{http.MethodDelete, "/", http.StatusMethodNotAllowed},
		{http.MethodGet, "/logout", http.StatusMethodNotAllowed},
		{http.MethodGet, "/apps?x=1", http.StatusBadRequest},
		{http.MethodGet, "/requests?other=1", http.StatusBadRequest},
		{http.MethodGet, "/requests?cursor=forged.value", http.StatusBadRequest},
		{http.MethodGet, "/login?token=" + paneltest.Token, http.StatusBadRequest},
		{http.MethodGet, "/apps/missing", http.StatusNotFound},
	} {
		res, body := do(t, env, c.method, c.target, cookie, nil, nil)
		if res.StatusCode != c.want {
			t.Fatalf("%s %s: %d, want %d", c.method, c.target, res.StatusCode, c.want)
		}
		if strings.Contains(body, paneltest.Token) {
			t.Fatal("a token was echoed")
		}
	}
	if env.Handler.Owns("/metrics") || env.Handler.Owns("/admin/v1/session") || env.Handler.Owns("/healthz") {
		t.Fatal("the panel claims a route of another surface")
	}
}

func httpTestServer(t *testing.T, h http.Handler) string {
	t.Helper()
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s.URL
}

func localRequest(target string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, target, nil)
	r.Host = "localhost"
	return r
}
