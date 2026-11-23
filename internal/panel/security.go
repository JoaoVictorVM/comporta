package panel

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/admin"
)

// ContentSecurityPolicy allows only this origin's own files: no inline script
// or style, no eval, no framing and forms that post only here.
const ContentSecurityPolicy = "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"

const (
	maxFormBytes = 16 << 10
	formDeadline = 10 * time.Second
)

// commonHeaders go on every panel response, assets and streams included.
func commonHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
}

// htmlHeaders go on every page and fragment.
func htmlHeaders(w http.ResponseWriter) {
	commonHeaders(w)
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Content-Security-Policy", ContentSecurityPolicy)
	h.Set("X-Frame-Options", "DENY")
	h.Set("Cache-Control", "no-store")
}

// sameOrigin requires exactly one Origin equal to the origin this listener
// serves. A cookie is sent by the browser on its own, so a state change or a
// search needs proof that the page asking is ours.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Values("Origin")
	return len(origin) == 1 && origin[0] == admin.RequestOrigin(r)
}

// originAllowed accepts an absent Origin or the exact one, as the JSON login
// does for clients that are not browsers.
func originAllowed(r *http.Request) bool {
	origin := r.Header.Values("Origin")
	return len(origin) == 0 || (len(origin) == 1 && origin[0] == admin.RequestOrigin(r))
}

var errForm = errors.New("invalid form")

// readForm reads a URL encoded body of at most 16 KiB with only the allowed
// fields, each at most once. Nothing of it is logged.
func readForm(w http.ResponseWriter, r *http.Request, allowed ...string) (url.Values, error) {
	if r.ContentLength > maxFormBytes {
		w.Header().Set("Connection", "close")
		return nil, errForm
	}
	types := r.Header.Values("Content-Type")
	if len(types) != 1 {
		return nil, errForm
	}
	media, _, err := mime.ParseMediaType(types[0])
	if err != nil || media != "application/x-www-form-urlencoded" {
		return nil, errForm
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(formDeadline))
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxFormBytes))
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
	if err != nil {
		w.Header().Set("Connection", "close")
		return nil, errForm
	}
	values, err := url.ParseQuery(string(data))
	clear(data)
	if err != nil {
		return nil, errForm
	}
	for key, list := range values {
		known := false
		for _, name := range allowed {
			if key == name {
				known = true
				break
			}
		}
		if !known || len(list) != 1 {
			return nil, errForm
		}
	}
	return values, nil
}

// noBody refuses a body on a request that takes none.
func noBody(r *http.Request) bool {
	return r.ContentLength <= 0 && len(r.TransferEncoding) == 0
}

// canonicalPath reports whether a raw request target is a plain path: no
// escapes, dot segments, empty segments or trailing slash, so a route can be
// matched exactly on what the client sent.
func canonicalPath(path string) bool {
	if path == "/" {
		return true
	}
	if !strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") || strings.ContainsAny(path, "%\\;") {
		return false
	}
	for _, segment := range strings.Split(path[1:], "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func itoa(n int) string { return strconv.Itoa(n) }
