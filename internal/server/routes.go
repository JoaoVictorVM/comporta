package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/auth"
	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/proxy"
)

const adminPrefix = "/admin/v1/"

// SnapshotSource provides the active configuration.
type SnapshotSource interface {
	Capture() *config.Snapshot
}

// RouteHandler serves an allowed proxy route.
type RouteHandler interface {
	ServeRoute(w http.ResponseWriter, r *http.Request, route proxy.Route, snapshot *config.Snapshot)
}

// ProxySurface is the handler of the proxy listener. It holds only the conversation
// routes: admin routes do not exist here, so no path can reach them.
func ProxySurface(source SnapshotSource, routes RouteHandler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		snapshot, ok := admit(w, r, source)
		if !ok {
			return
		}
		path, _, _ := strings.Cut(r.RequestURI, "?")
		route, ok := proxy.MatchRoute(r.Method, path)
		if !ok {
			proxy.WriteFailure(w, auth.ModelsFormat(r), proxy.EndpointNotAllowed())
			return
		}
		routes.ServeRoute(w, r, route, snapshot)
	})
}

// AdminSurface is the handler of an admin listener without the admin API: only
// the static health response.
func AdminSurface(source SnapshotSource) http.Handler {
	return AdminSurfaceWith(source, nil)
}

// AdminSurfaceWith is the handler of the admin listener. The admin API lives
// only here, under its own prefix, behind the same Host checks as the proxy;
// healthz stays public and carries no operational data.
func AdminSurfaceWith(source SnapshotSource, api http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := admit(w, r, source); !ok {
			return
		}
		if api != nil && strings.HasPrefix(r.RequestURI, adminPrefix) {
			api.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet || r.RequestURI != "/healthz" {
			proxy.WriteFailure(w, auth.FormatOpenAI, proxy.Failure{
				Status: http.StatusNotFound, Code: proxy.CodeEndpointNotAllowed,
				Class: proxy.ClassInvalidRequest, Message: "endpoint not allowed by Comporta",
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
}

// admit runs the checks shared by both surfaces. An absolute request target comes
// first because net/http replaces the Host with the one inside the target, so the
// real header could not be judged.
func admit(w http.ResponseWriter, r *http.Request, source SnapshotSource) (*config.Snapshot, bool) {
	// The admission deadline covers only the headers, which were read by now.
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
	if !strings.HasPrefix(r.RequestURI, "/") {
		proxy.WriteFailure(w, auth.ModelsFormat(r), proxy.InvalidTarget())
		return nil, false
	}
	snapshot := source.Capture()
	if allowed, safeHost := checkHost(r.Host, snapshot); !allowed {
		proxy.WriteFailure(w, auth.ModelsFormat(r), proxy.HostNotAllowed(safeHost))
		return nil, false
	}
	return snapshot, true
}
