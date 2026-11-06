package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/keys"
)

const (
	headerAPIKey        = "X-Api-Key"
	headerAuthorization = "Authorization"
	maxCredentialHeader = 256
	bearerScheme        = "bearer"
)

// Format is the API format errors are written in, chosen by the route.
type Format int

const (
	FormatAnthropic Format = iota
	FormatOpenAI
)

// FormatSelector picks the format of a request from its route, never its body.
type FormatSelector func(r *http.Request) Format

// Anthropic selects the Anthropic format, for Messages and count_tokens.
func Anthropic(*http.Request) Format { return FormatAnthropic }

// OpenAI selects the OpenAI format, for Chat Completions.
func OpenAI(*http.Request) Format { return FormatOpenAI }

// ModelsFormat selects for the models route, shared by both APIs: x-api-key means
// Anthropic and anything else OpenAI. Conflicting credentials are rejected later.
func ModelsFormat(r *http.Request) Format {
	if len(r.Header.Values(headerAPIKey)) > 0 {
		return FormatAnthropic
	}
	return FormatOpenAI
}

type identityKey struct{}
type snapshotKey struct{}

// IdentityFrom returns the identity accepted for this request. Without one there
// is no authenticated app, and callers must not assume a default.
func IdentityFrom(ctx context.Context) (keys.Identity, bool) {
	identity, ok := ctx.Value(identityKey{}).(keys.Identity)
	return identity, ok
}

// SnapshotFrom returns the configuration captured for this request. Using it for
// the rest of the operation keeps a reload from changing the rules midway.
func SnapshotFrom(ctx context.Context) (*config.Snapshot, bool) {
	snapshot, ok := ctx.Value(snapshotKey{}).(*config.Snapshot)
	return snapshot, ok
}

// SnapshotSource provides the active configuration.
type SnapshotSource interface {
	Capture() *config.Snapshot
}

// HTTP adapts authentication to HTTP handlers.
type HTTP struct {
	source        KeySource
	authenticator *Authenticator
	guard         *Guard
}

// NewHTTP returns an adapter. The guard is per process and must be shared by every
// protected route, so that an address blocked on one is blocked on all.
func NewHTTP(source KeySource, authenticator *Authenticator, guard *Guard) *HTTP {
	return &HTTP{source: source, authenticator: authenticator, guard: guard}
}

// Protect wraps next, capturing one configuration snapshot per request. Revoking a
// key affects new requests only: one already accepted keeps its identity.
func (h *HTTP) Protect(format FormatSelector, snapshots SnapshotSource, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authenticated, ok := h.Authorize(w, r, format(r), snapshots.Capture())
		if ok {
			next.ServeHTTP(w, authenticated)
		}
	})
}

// Authorize applies, in order: store availability, address validity and block,
// credential extraction and verification, then failure accounting. On failure it
// writes the response and returns false. An unavailable store does not count as a
// failed credential, so an outage cannot get clients blocked.
func (h *HTTP) Authorize(w http.ResponseWriter, r *http.Request, format Format, snapshot *config.Snapshot) (*http.Request, bool) {
	generation := h.source.Capture()
	if !generation.Available() {
		writeError(w, format, errUnavailable)
		return nil, false
	}
	addr, ok := connectionAddress(r)
	if !ok {
		writeError(w, format, errBadAddress)
		return nil, false
	}
	if decision := h.guard.Admit(addr); !decision.Allowed {
		writeRateLimited(w, format, decision)
		return nil, false
	}

	identity, err := h.verify(r, format, generation, snapshot)
	switch {
	case err == nil:
		ctx := context.WithValue(r.Context(), identityKey{}, identity)
		ctx = context.WithValue(ctx, snapshotKey{}, snapshot)
		return r.WithContext(ctx), true
	case errors.Is(err, ErrUnavailable):
		writeError(w, format, errUnavailable)
		return nil, false
	}
	if decision := h.guard.Failure(addr); !decision.Allowed {
		writeRateLimited(w, format, decision)
		return nil, false
	}
	if errors.Is(err, ErrRevokedKey) {
		writeError(w, format, errRevoked)
		return nil, false
	}
	writeError(w, format, errInvalid)
	return nil, false
}

func (h *HTTP) verify(r *http.Request, format Format, generation *keys.Generation, snapshot *config.Snapshot) (keys.Identity, error) {
	presented, ok := extractCredential(r, format)
	if !ok {
		return keys.Identity{}, ErrInvalidKey
	}
	key, err := keys.ParseKey(presented)
	if err != nil {
		return keys.Identity{}, ErrInvalidKey
	}
	return h.authenticator.Authenticate(key, generation, snapshot)
}

// connectionAddress uses only the address of the connection. Forwarded headers are
// ignored on purpose: without a trusted proxy, a client could send any value and
// escape the block by changing it.
func connectionAddress(r *http.Request) (netip.Addr, bool) {
	addrPort, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, false
	}
	return addrPort.Addr().Unmap().WithZone(""), true
}

// extractCredential refuses every ambiguity instead of picking one of the values:
// a repeated header or two methods at once could otherwise authenticate one way
// here and be read another way by a later consumer.
func extractCredential(r *http.Request, format Format) (string, bool) {
	apiKeys := r.Header.Values(headerAPIKey)
	authorizations := r.Header.Values(headerAuthorization)
	switch {
	case format == FormatAnthropic && len(apiKeys) == 1 && len(authorizations) == 0:
		return apiKeys[0], len(apiKeys[0]) <= maxCredentialHeader
	case format == FormatOpenAI && len(authorizations) == 1 && len(apiKeys) == 0:
		return bearerToken(authorizations[0])
	}
	return "", false
}

func bearerToken(value string) (string, bool) {
	if len(value) > maxCredentialHeader {
		return "", false
	}
	scheme, token, found := strings.Cut(value, " ")
	if !found || !strings.EqualFold(scheme, bearerScheme) || token == "" || strings.ContainsAny(token, " \t") {
		return "", false
	}
	return token, true
}

type failure struct {
	status  int
	kind    string
	message string
	code    string
}

var (
	errInvalid     = failure{http.StatusUnauthorized, "authentication_error", ErrInvalidKey.Error(), "invalid_api_key"}
	errRevoked     = failure{http.StatusUnauthorized, "authentication_error", ErrRevokedKey.Error(), "key_revoked"}
	errRateLimited = failure{http.StatusTooManyRequests, "rate_limit_error", "too many authentication attempts", "auth_rate_limited"}
	errUnavailable = failure{http.StatusServiceUnavailable, "api_error", ErrUnavailable.Error(), "auth_unavailable"}
	errBadAddress  = failure{http.StatusBadRequest, "invalid_request_error", "invalid client address", "invalid_client_address"}
)

type errorBody struct {
	Type         string `json:"type"`
	Message      string `json:"message"`
	ComportaCode string `json:"comporta_code"`
}

func writeRateLimited(w http.ResponseWriter, format Format, decision Decision) {
	w.Header().Set("Retry-After", strconv.Itoa(decision.RetryAfterSeconds()))
	writeError(w, format, errRateLimited)
}

func writeError(w http.ResponseWriter, format Format, f failure) {
	body := errorBody{Type: f.kind, Message: f.message, ComportaCode: f.code}
	var payload any
	if format == FormatAnthropic {
		payload = struct {
			Type  string    `json:"type"`
			Error errorBody `json:"error"`
		}{"error", body}
	} else {
		payload = struct {
			Error errorBody `json:"error"`
		}{body}
	}
	data, err := json.Marshal(payload)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(f.status)
	_, _ = w.Write(data)
}
