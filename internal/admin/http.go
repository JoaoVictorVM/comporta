package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/auth"
)

const (
	// Prefix is the base path of the admin API.
	Prefix = "/admin/v1/"

	maxBodyBytes = 16 << 10
	bodyDeadline = 10 * time.Second

	// GuardMaxFailures is the failure that arms the block of an address.
	GuardMaxFailures = 10
	// GuardWindow is the sliding window in which failures are counted.
	GuardWindow = 60 * time.Second
	// GuardCooldown is how long a blocked address is refused.
	GuardCooldown = 5 * time.Minute
)

// NewGuard returns the guard of the admin API, separate from the proxy's.
func NewGuard(clock auth.Clock, logger *slog.Logger) *auth.Guard {
	return auth.NewGuardWith(clock, logger, auth.GuardOptions{MaxFailures: GuardMaxFailures, Window: GuardWindow, Cooldown: GuardCooldown})
}

// apiError is a sanitized failure. Messages are fixed vocabulary and never echo
// the request.
type apiError struct {
	status     int
	code       string
	message    string
	retryAfter int
}

func (e *apiError) Error() string { return e.message }

var (
	errInvalidToken     = &apiError{http.StatusUnauthorized, "invalid_admin_token", "invalid admin token", 0}
	errOrigin           = &apiError{http.StatusForbidden, "origin_not_allowed", "origin not allowed", 0}
	errNotFound         = &apiError{http.StatusNotFound, "not_found", "admin endpoint not found", 0}
	errMethod           = &apiError{http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed", 0}
	errTooLarge         = &apiError{http.StatusRequestEntityTooLarge, "body_too_large", "request body exceeds 16 KiB", 0}
	errMediaType        = &apiError{http.StatusUnsupportedMediaType, "unsupported_media_type", "content type must be application/json", 0}
	errSessionsFull     = &apiError{http.StatusServiceUnavailable, "admin_sessions_full", "admin session capacity reached", 0}
	errStorage          = &apiError{http.StatusServiceUnavailable, "admin_storage_unavailable", "admin storage unavailable; the operation was not applied", 0}
	errAppNotFound      = &apiError{http.StatusNotFound, "app_not_found", "app not found", 0}
	errUserBusy         = &apiError{http.StatusConflict, "user_busy", "user has requests in progress; try again", 0}
	errDeletionFailed   = &apiError{http.StatusServiceUnavailable, "deletion_failed", "deletion failed; no records were removed", 0}
	errInternal         = &apiError{http.StatusInternalServerError, "internal_error", "internal error", 0}
	errQueryNotAccepted = invalid("query parameters are not accepted")
)

func invalid(message string) *apiError {
	return &apiError{http.StatusBadRequest, "invalid_request", message, 0}
}

type access int

const (
	accessLogin access = iota
	accessSession
	accessAny
)

type route struct {
	methods map[string]handlerFunc
	access  access
}

// handlerFunc serves an authenticated request. body is the strict JSON body,
// already limited and checked, or nil for methods without one.
type handlerFunc func(w http.ResponseWriter, r *http.Request, body []byte) *apiError

// HandlerOptions configures the admin API.
type HandlerOptions struct {
	Credential *Credential
	Sessions   *Sessions
	Guard      *auth.Guard
	Service    *Service
	Logger     *slog.Logger
	Now        func() time.Time
}

// Handler serves /admin/v1. It never exists on the proxy listener.
type Handler struct {
	credential *Credential
	sessions   *Sessions
	guard      *auth.Guard
	service    *Service
	logger     *slog.Logger
	now        func() time.Time
	routes     map[string]route
}

// NewHandler returns the admin API.
func NewHandler(options HandlerOptions) *Handler {
	h := &Handler{
		credential: options.Credential, sessions: options.Sessions, guard: options.Guard,
		service: options.Service, logger: options.Logger, now: options.Now,
	}
	if h.sessions == nil {
		h.sessions = NewSessions(nil)
	}
	if h.guard == nil {
		h.guard = NewGuard(nil, options.Logger)
	}
	if h.logger == nil {
		h.logger = slog.New(slog.DiscardHandler)
	}
	if h.now == nil {
		h.now = time.Now
	}
	h.routes = map[string]route{
		Prefix + "session": {access: accessLogin, methods: map[string]handlerFunc{
			http.MethodPost: h.login, http.MethodDelete: h.logout,
		}},
	}
	h.registerOperations()
	return h
}

// ServeHTTP checks the route before any credential, so the answer to an unknown
// path never depends on the token; data and app names come only after it.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	header := w.Header()
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("Referrer-Policy", "no-referrer")
	path, query, hasQuery := strings.Cut(r.RequestURI, "?")
	current, ok := h.routes[path]
	if !ok {
		writeError(w, errNotFound)
		return
	}
	serve, ok := current.methods[r.Method]
	if !ok {
		allowed := make([]string, 0, len(current.methods))
		for method := range current.methods {
			allowed = append(allowed, method)
		}
		slices.Sort(allowed)
		header.Set("Allow", strings.Join(allowed, ", "))
		writeError(w, errMethod)
		return
	}
	if hasQuery || query != "" {
		writeError(w, errQueryNotAccepted)
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
	origin := r.Header.Values("Origin")
	if len(origin) > 1 || (len(origin) == 1 && origin[0] != requestOrigin(r)) {
		writeError(w, errOrigin)
		return
	}
	if current.access == accessLogin && r.Method == http.MethodPost {
		h.serveLogin(w, r, addr)
		return
	}
	viaSession, failure := h.authenticate(r)
	if failure != nil {
		if decision := h.guard.Failure(addr); !decision.Allowed {
			writeError(w, rateLimited(decision))
			return
		}
		writeError(w, failure)
		return
	}
	if current.access == accessSession && !viaSession {
		writeError(w, errInvalidToken)
		return
	}
	// A cookie is sent by the browser on its own, so a state change needs proof
	// that the page asking for it is ours.
	if viaSession && r.Method != http.MethodGet && len(origin) == 0 {
		writeError(w, errOrigin)
		return
	}
	body, failure := readBody(w, r)
	if failure != nil {
		writeError(w, failure)
		return
	}
	if failure := serve(w, r, body); failure != nil {
		writeError(w, failure)
	}
}

func rateLimited(decision auth.Decision) *apiError {
	return &apiError{http.StatusTooManyRequests, "admin_rate_limited", "too many failed admin authentications; try again later", max(1, decision.RetryAfterSeconds())}
}

// authenticate accepts the admin token as a bearer credential or a session
// cookie. The token itself is never read from a cookie, the URL or another
// header.
func (h *Handler) authenticate(r *http.Request) (viaSession bool, failure *apiError) {
	authorizations := r.Header.Values("Authorization")
	if len(authorizations) > 0 {
		if len(authorizations) != 1 {
			return false, errInvalidToken
		}
		scheme, token, ok := strings.Cut(authorizations[0], " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") || !h.credential.Verify(token) {
			return false, errInvalidToken
		}
		return false, nil
	}
	var found []*http.Cookie
	for _, cookie := range r.Cookies() {
		if cookie.Name == SessionCookie {
			found = append(found, cookie)
		}
	}
	if len(found) != 1 || !h.sessions.Valid(found[0].Value) {
		return false, errInvalidToken
	}
	return true, nil
}

type loginRequest struct {
	Token string `json:"token"`
}

type loginResponse struct {
	ExpiresAt string `json:"expires_at"`
}

// serveLogin verifies the token in the body under the same guard as any other
// credential.
func (h *Handler) serveLogin(w http.ResponseWriter, r *http.Request, addr netip.Addr) {
	body, failure := readBody(w, r)
	if failure != nil {
		writeError(w, failure)
		return
	}
	var request loginRequest
	if failure := decodeStrict(body, &request); failure != nil {
		writeError(w, failure)
		return
	}
	if !h.credential.Verify(request.Token) {
		if decision := h.guard.Failure(addr); !decision.Allowed {
			writeError(w, rateLimited(decision))
			return
		}
		writeError(w, errInvalidToken)
		return
	}
	if failure := h.login(w, r, body); failure != nil {
		writeError(w, failure)
	}
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request, _ []byte) *apiError {
	id, expires, err := h.sessions.Create()
	if errors.Is(err, ErrSessionsFull) {
		return errSessionsFull
	}
	if err != nil {
		return errInternal
	}
	http.SetCookie(w, sessionCookie(id, r.TLS != nil))
	writeJSON(w, http.StatusCreated, loginResponse{ExpiresAt: expires.UTC().Format(time.RFC3339)})
	return nil
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request, _ []byte) *apiError {
	for _, cookie := range r.Cookies() {
		if cookie.Name == SessionCookie {
			h.sessions.Delete(cookie.Value)
		}
	}
	http.SetCookie(w, expiredCookie(r.TLS != nil))
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// readBody returns nil for a request without a body. A body must be JSON and
// fit the admin limit; it is read within its own deadline.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, *apiError) {
	if r.ContentLength > maxBodyBytes {
		w.Header().Set("Connection", "close")
		return nil, errTooLarge
	}
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(bodyDeadline))
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
	if err != nil {
		w.Header().Set("Connection", "close")
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return nil, errTooLarge
		}
		return nil, invalid("request body could not be read")
	}
	if len(data) == 0 {
		if r.Method == http.MethodPost {
			return nil, invalid("request body is required")
		}
		return nil, nil
	}
	if r.Method != http.MethodPost {
		return nil, invalid("this endpoint accepts no body")
	}
	contentTypes := r.Header.Values("Content-Type")
	if len(contentTypes) != 1 {
		return nil, errMediaType
	}
	media, _, err := mime.ParseMediaType(contentTypes[0])
	if err != nil || media != "application/json" {
		return nil, errMediaType
	}
	return data, nil
}

// decodeStrict accepts exactly one JSON object with known fields.
func decodeStrict(body []byte, target any) *apiError {
	if body == nil {
		return invalid("request body is required")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return invalid("request body is not valid JSON for this endpoint")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return invalid("request body must hold a single JSON object")
	}
	return nil
}

type errorEnvelope struct {
	Error errorBody `json:"error"`
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeError(w http.ResponseWriter, failure *apiError) {
	if failure.retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(failure.retryAfter))
	}
	writeJSON(w, failure.status, errorEnvelope{Error: errorBody{Code: failure.code, Message: failure.message}})
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		status, data = http.StatusInternalServerError, []byte(`{"error":{"code":"internal_error","message":"internal error"}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

// operationContext bounds an operation by the admin deadline while still ending
// when the client leaves.
func operationContext(r *http.Request, limit time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), limit)
}

// registerOperations adds the routes backed by the operational services.
func (h *Handler) registerOperations() {
	if h.service == nil {
		return
	}
	h.routes[Prefix+"pause"] = route{access: accessAny, methods: map[string]handlerFunc{
		http.MethodGet: h.getPause, http.MethodPost: h.pauseHandler(true),
	}}
	h.routes[Prefix+"resume"] = route{access: accessAny, methods: map[string]handlerFunc{
		http.MethodPost: h.pauseHandler(false),
	}}
	h.routes[Prefix+"users/forget"] = route{access: accessAny, methods: map[string]handlerFunc{
		http.MethodPost: h.forget,
	}}
}

// ForgetTimeout bounds a deletion request end to end.
const ForgetTimeout = 25 * time.Second

type forgetRequest struct {
	UserID *string `json:"user_id"`
}

// ForgetDeleted counts the rows removed per category.
type ForgetDeleted struct {
	Requests         int64 `json:"requests"`
	UsageDaily       int64 `json:"usage_daily"`
	Events           int64 `json:"events"`
	Journal          int64 `json:"journal"`
	BudgetState      int64 `json:"budget_state"`
	ReservationItems int64 `json:"reservation_items"`
}

// ForgetResponse never repeats the identifier, the pseudonyms or the apps.
type ForgetResponse struct {
	DeletedRows           int64         `json:"deleted_rows"`
	Deleted               ForgetDeleted `json:"deleted"`
	UnlinkedRows          int64         `json:"unlinked_rows"`
	ClearedMemorySubjects int           `json:"cleared_memory_subjects"`
}

func (h *Handler) forget(w http.ResponseWriter, r *http.Request, body []byte) *apiError {
	var request forgetRequest
	if failure := decodeStrict(body, &request); failure != nil {
		return failure
	}
	if request.UserID == nil {
		return invalid("user_id is required")
	}
	ctx, cancel := operationContext(r, ForgetTimeout)
	defer cancel()
	result, err := h.service.Forget(ctx, *request.UserID)
	if errors.Is(err, ErrInvalidInput) {
		return invalid("user_id is not a valid user ID")
	}
	if err != nil {
		return serviceFailure(err)
	}
	c := result.Counts
	writeJSON(w, http.StatusOK, ForgetResponse{
		DeletedRows: c.DeletedRows(),
		Deleted: ForgetDeleted{Requests: c.Requests, UsageDaily: c.UsageDaily, Events: c.Events, Journal: c.Journal,
			BudgetState: c.BudgetState, ReservationItems: c.ReservationItems},
		UnlinkedRows: c.Unlinked, ClearedMemorySubjects: result.Cleared,
	})
	return nil
}

type pauseRequest struct {
	App *string `json:"app"`
	All *bool   `json:"all"`
}

type pauseResponse struct {
	App             string `json:"app,omitempty"`
	All             bool   `json:"all,omitempty"`
	GlobalPaused    bool   `json:"global_paused"`
	AppPaused       *bool  `json:"app_paused,omitempty"`
	EffectivePaused *bool  `json:"effective_paused,omitempty"`
	Changed         bool   `json:"changed"`
	UpdatedAt       string `json:"updated_at"`
}

type pauseEntry struct {
	App       string `json:"app"`
	Paused    bool   `json:"paused"`
	UpdatedAt string `json:"updated_at"`
}

type pauseStateResponse struct {
	GlobalPaused bool         `json:"global_paused"`
	Apps         []pauseEntry `json:"apps"`
}

func timestamp(at time.Time) string { return at.UTC().Format(time.RFC3339) }

func (h *Handler) getPause(w http.ResponseWriter, _ *http.Request, _ []byte) *apiError {
	state := h.service.PauseState()
	response := pauseStateResponse{GlobalPaused: state.Global, Apps: []pauseEntry{}}
	for _, entry := range state.Apps {
		response.Apps = append(response.Apps, pauseEntry{App: entry.App, Paused: entry.Paused, UpdatedAt: timestamp(entry.UpdatedAt)})
	}
	writeJSON(w, http.StatusOK, response)
	return nil
}

func (h *Handler) pauseHandler(pause bool) handlerFunc {
	return func(w http.ResponseWriter, r *http.Request, body []byte) *apiError {
		var request pauseRequest
		if failure := decodeStrict(body, &request); failure != nil {
			return failure
		}
		var selector PauseSelector
		switch {
		case request.App != nil && request.All == nil:
			selector.App = *request.App
		case request.All != nil && request.App == nil && *request.All:
			selector.All = true
		default:
			return invalid("send exactly one of app or all:true")
		}
		ctx, cancel := operationContext(r, 10*time.Second)
		defer cancel()
		outcome, err := h.service.SetPause(ctx, selector, pause)
		if err != nil {
			return serviceFailure(err)
		}
		response := pauseResponse{GlobalPaused: outcome.GlobalPaused, Changed: outcome.Changed, UpdatedAt: timestamp(outcome.UpdatedAt)}
		if selector.All {
			response.All = true
		} else {
			appPaused, effective := outcome.AppPaused, outcome.Effective()
			response.App, response.AppPaused, response.EffectivePaused = selector.App, &appPaused, &effective
		}
		writeJSON(w, http.StatusOK, response)
		return nil
	}
}

// serviceFailure maps service errors to fixed answers; anything unexpected is
// reported as an unconfirmed operation.
func serviceFailure(err error) *apiError {
	switch {
	case errors.Is(err, ErrInvalidInput):
		return invalid("request is not valid for this endpoint")
	case errors.Is(err, ErrAppNotFound):
		return errAppNotFound
	case errors.Is(err, ErrUserBusy):
		return errUserBusy
	case errors.Is(err, ErrDeletion):
		return errDeletionFailed
	}
	return errStorage
}
