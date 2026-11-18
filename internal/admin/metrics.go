package admin

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/JoaoVictorVM/comporta/internal/auth"
)

// MetricsSource is the telemetry as the scrape endpoint sees it.
type MetricsSource interface {
	MetricsEnabled() bool
	WriteMetrics(io.Writer) error
}

// MetricsContentType is the Prometheus text exposition format.
const MetricsContentType = "text/plain; version=0.0.4; charset=utf-8"

var (
	errMetricsUnavailable = &apiError{http.StatusServiceUnavailable, "metrics_unavailable", "metrics unavailable", 0}
	errBodyNotAccepted    = invalid("request body is not accepted")
)

// Metrics serves GET /metrics. Only the admin token as a bearer credential
// is accepted: neither a panel session cookie nor an app key authorizes a
// scrape. Failed attempts share the admin API's guard, so the endpoint cannot
// be used to guess the token. The router has already checked the Host, the
// method and the exact path.
func (h *Handler) Metrics(source MetricsSource) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header := w.Header()
		header.Set("Cache-Control", "no-store")
		header.Set("X-Content-Type-Options", "nosniff")
		if r.ContentLength != 0 || len(r.TransferEncoding) > 0 {
			writeError(w, errBodyNotAccepted)
			return
		}
		if !source.MetricsEnabled() {
			writeError(w, errNotFound)
			return
		}
		addr, ok := auth.ConnectionAddress(r)
		if !ok {
			writeError(w, invalid("connection address could not be read"))
			return
		}
		if decision := h.guard.Admit(addr); !decision.Allowed {
			writeError(w, rateLimited(decision))
			return
		}
		if !h.bearer(r) {
			if decision := h.guard.Failure(addr); !decision.Allowed {
				writeError(w, rateLimited(decision))
				return
			}
			writeError(w, errInvalidToken)
			return
		}
		var body bytes.Buffer
		if err := source.WriteMetrics(&body); err != nil {
			writeError(w, errMetricsUnavailable)
			return
		}
		header.Set("Content-Type", MetricsContentType)
		header.Set("Content-Length", strconv.Itoa(body.Len()))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body.Bytes())
	})
}

// bearer accepts exactly one Authorization header with the admin token.
func (h *Handler) bearer(r *http.Request) bool {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return false
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	return ok && strings.EqualFold(scheme, "Bearer") && h.credential.Verify(token)
}
