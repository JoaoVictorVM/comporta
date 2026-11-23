package integration

import (
	"net/http"
	"strings"
	"testing"

	"golang.org/x/net/html"

	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
)

func htmlWalk(n *html.Node, fn func(*html.Node)) {
	fn(n)
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		htmlWalk(c, fn)
	}
}

func htmlAttr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func htmlHas(n *html.Node, key string) bool {
	for _, a := range n.Attr {
		if a.Key == key {
			return true
		}
	}
	return false
}

// TestPanelSemanticSmoke checks every screen for the structure assistive
// technology relies on: language, one main landmark reachable by a skip link,
// one first-level heading, named navigation, labelled controls, captioned
// tables and named scroll regions.
func TestPanelSemanticSmoke(t *testing.T) {
	s := startServed(t, fakeprovider.Options{})
	if status, body := s.call("bot", "frank", panelBody); status != http.StatusOK {
		t.Fatalf("traffic: %d %s", status, body)
	}
	cookie := s.panelLogin()
	s.waitPanel(cookie, "/requests", "fake-gpt")
	for _, path := range []string{"/login", "/", "/apps", "/apps/bot", "/users", "/requests", "/events", "/providers", "/configuration"} {
		var page string
		if path == "/login" {
			_, page = s.panel(nil, http.MethodGet, path, nil)
		} else {
			_, page = s.panel(cookie, http.MethodGet, path, nil)
		}
		doc, err := html.Parse(strings.NewReader(page))
		if err != nil {
			t.Fatal(err)
		}
		var mains, h1s, navs int
		ids := map[string]bool{}
		labels := map[string]bool{}
		var controls []*html.Node
		skip := false
		htmlWalk(doc, func(n *html.Node) {
			if n.Type != html.ElementNode {
				return
			}
			if id := htmlAttr(n, "id"); id != "" {
				if ids[id] {
					t.Fatalf("%s: duplicate id %q", path, id)
				}
				ids[id] = true
			}
			switch n.Data {
			case "html":
				if htmlAttr(n, "lang") == "" {
					t.Fatalf("%s: no document language", path)
				}
			case "main":
				mains++
			case "h1":
				h1s++
			case "nav":
				navs++
				if htmlAttr(n, "aria-label") == "" {
					t.Fatalf("%s: unnamed navigation", path)
				}
			case "label":
				labels[htmlAttr(n, "for")] = true
			case "input", "select", "textarea":
				if htmlAttr(n, "type") != "hidden" {
					controls = append(controls, n)
				}
			case "table":
				captioned := false
				for c := n.FirstChild; c != nil; c = c.NextSibling {
					captioned = captioned || (c.Type == html.ElementNode && c.Data == "caption")
				}
				if !captioned {
					t.Fatalf("%s: table without caption", path)
				}
			case "a":
				if htmlAttr(n, "href") == "#main" {
					skip = true
				}
			case "img", "canvas":
				if htmlAttr(n, "alt") == "" && htmlAttr(n, "aria-hidden") != "true" {
					t.Fatalf("%s: %s without a text alternative", path, n.Data)
				}
			}
			if htmlAttr(n, "role") == "region" && (htmlAttr(n, "aria-label") == "" || !htmlHas(n, "tabindex")) {
				t.Fatalf("%s: scroll region without a name or keyboard focus", path)
			}
		})
		if mains != 1 || h1s != 1 {
			t.Fatalf("%s: %d main landmarks and %d first-level headings", path, mains, h1s)
		}
		if path != "/login" && (!skip || navs == 0 || !ids["main"]) {
			t.Fatalf("%s: no skip link to main or no navigation", path)
		}
		for _, c := range controls {
			if id := htmlAttr(c, "id"); id == "" || !labels[id] {
				t.Fatalf("%s: control %q without a label", path, htmlAttr(c, "name"))
			}
		}
	}
}
