package panel

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed assets/app.css assets/app.js assets/manifest.json assets/vendor
var assetFS embed.FS

// asset is one embedded file served under its content digest.
type asset struct {
	name    string
	path    string
	mime    string
	digest  string
	content []byte
}

// servedAssets is the closed set of files the browser may load. Licenses and
// the manifest are embedded for review but never served.
var servedAssets = []struct{ name, path, mime string }{
	{"app.css", "assets/app.css", "text/css; charset=utf-8"},
	{"app.js", "assets/app.js", "text/javascript; charset=utf-8"},
	{"htmx.min.js", "assets/vendor/htmx.min.js", "text/javascript; charset=utf-8"},
	{"htmx-ext-sse.min.js", "assets/vendor/htmx-ext-sse.min.js", "text/javascript; charset=utf-8"},
	{"uPlot.iife.min.js", "assets/vendor/uPlot.iife.min.js", "text/javascript; charset=utf-8"},
	{"uPlot.min.css", "assets/vendor/uPlot.min.css", "text/css; charset=utf-8"},
}

var assets = loadAssets()

// loadAssets reads every served file once. A missing file is a build defect,
// never a reason to fetch it elsewhere.
func loadAssets() map[string]asset {
	out := map[string]asset{}
	for _, a := range servedAssets {
		content, err := fs.ReadFile(assetFS, a.path)
		if err != nil {
			panic("panel: embedded asset missing: " + a.path)
		}
		sum := sha256.Sum256(content)
		out[a.name] = asset{name: a.name, path: a.path, mime: a.mime, digest: hex.EncodeToString(sum[:8]), content: content}
	}
	return out
}

// AssetURL is the immutable URL of an embedded asset.
func AssetURL(name string) string {
	a, ok := assets[name]
	if !ok {
		panic("panel: unknown asset " + name)
	}
	return "/assets/" + a.digest + "/" + a.name
}

// serveAsset answers GET /assets/{digest}/{name} for the exact digest of an
// allowed file. Anything else is not found.
func serveAsset(w http.ResponseWriter, r *http.Request, rest string) bool {
	digest, name, ok := strings.Cut(rest, "/")
	if !ok {
		return false
	}
	a, ok := assets[name]
	if !ok || a.digest != digest {
		return false
	}
	h := w.Header()
	h.Set("Content-Type", a.mime)
	h.Set("Cache-Control", "public, max-age=31536000, immutable")
	h.Set("ETag", `"`+a.digest+`"`)
	if match := r.Header.Get("If-None-Match"); match == `"`+a.digest+`"` {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	h.Set("Content-Length", itoa(len(a.content)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(a.content)
	}
	return true
}
