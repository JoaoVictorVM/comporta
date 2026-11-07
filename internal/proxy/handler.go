// Package proxy implements the request pipeline between SDK clients and providers.
package proxy

import (
	"net/http"

	"github.com/JoaoVictorVM/comporta/internal/auth"
	"github.com/JoaoVictorVM/comporta/internal/config"
)

// Options configures a Handler.
type Options struct {
	Auth *auth.HTTP
}

// Handler serves the fixed routes.
type Handler struct {
	auth *auth.HTTP
}

// NewHandler returns a Handler.
func NewHandler(options Options) *Handler {
	return &Handler{auth: options.Auth}
}

// ServeRoute authenticates and serves one request using the snapshot the caller
// captured, so that the whole operation sees one configuration generation.
func (h *Handler) ServeRoute(w http.ResponseWriter, r *http.Request, route Route, snapshot *config.Snapshot) {
	format := route.format(r)
	if _, ok := h.auth.Authorize(w, r, format, snapshot); !ok {
		return
	}
	WriteFailure(w, format, internalFailure())
}
