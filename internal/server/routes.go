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
	return AdminSurfaceWithMetrics(source, api, nil)
}

// MetricsPath is the Prometheus scrape route. It exists only on the admin
// listener and only as GET on this exact target, without a query.
const MetricsPath = "/metrics"

// AdminSurfaceWithMetrics adds the scrape route to the admin listener. The
// Host checks run before it, as for every other admin route.
func AdminSurfaceWithMetrics(source SnapshotSource, api, metrics http.Handler) http.Handler {
	return AdminSurfaceReady(source, api, metrics, nil)
}

// ReadinessPath answers whether this replica may take traffic. It needs no
// credential, accepts no body or query and reveals no diagnostic: only ready
// or not_ready. Liveness stays on /healthz and never depends on the stores.
const ReadinessPath = "/readyz"

// AdminSurfaceReady adds the readiness route. A nil ready function keeps the
// route absent, which is the behavior of a single instance.
func AdminSurfaceReady(source SnapshotSource, api, metrics http.Handler, ready func() bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := admit(w, r, source); !ok {
			return
		}
		if api != nil && strings.HasPrefix(r.RequestURI, adminPrefix) {
			api.ServeHTTP(w, r)
			return
		}
		if metrics != nil && r.Method == http.MethodGet && r.RequestURI == MetricsPath {
			metrics.ServeHTTP(w, r)
			return
		}
		if ready != nil && r.Method == http.MethodGet && r.RequestURI == ReadinessPath && r.ContentLength <= 0 {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Cache-Control", "no-store")
			if !ready() {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(`{"status":"not_ready"}`))
				return
			}
			_, _ = w.Write([]byte(`{"status":"ready"}`))
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
