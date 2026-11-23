package panel

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"github.com/JoaoVictorVM/comporta/internal/admin"
	"github.com/JoaoVictorVM/comporta/internal/auth"
)

// Frame is what every page shares: the screen, the navigation and the state
// banners. It never carries request data.
type Frame struct {
	Screen       Screen
	GlobalPaused bool
	Apps         []string
	LiveURL      string
	At           time.Time
}

// LoginPage is the state of the login form. The token is never part of it.
type LoginPage struct {
	Failed      bool
	RateLimited bool
	Unavailable bool
}

// ErrorPage is a safe failure: a status and a fixed message.
type ErrorPage struct {
	Status  int
	Title   string
	Message string
}

// Views renders the screens. It lives in its own package so templates depend
// on the projections and never the other way around.
type Views interface {
	Login(LoginPage) templ.Component
	Error(Frame, ErrorPage) templ.Component
	ErrorFragment(ErrorPage) templ.Component
	Overview(Frame, Overview) templ.Component
	OverviewFragment(Overview) templ.Component
	Apps(Frame, []AppSummary, Availability) templ.Component
	AppsFragment([]AppSummary, Availability) templ.Component
	App(Frame, AppDetail) templ.Component
	AppFragment(AppDetail) templ.Component
	Users(Frame, *UserSearch) templ.Component
	UserResult(UserSearch) templ.Component
	Requests(Frame, RequestList) templ.Component
	RequestsFragment(RequestList) templ.Component
	Request(Frame, RequestDetail) templ.Component
	Events(Frame, EventList) templ.Component
	EventsFragment(EventList) templ.Component
	Providers(Frame, Providers) templ.Component
	ProvidersFragment(Providers) templ.Component
	Configuration(Frame, Configuration) templ.Component
	ConfigurationFragment(Configuration) templ.Component
}

// HandlerOptions configures the panel surface. Credential, Sessions and Guard
// are the admin API's own, so a login here is the same login.
type HandlerOptions struct {
	Service    *Service
	Views      Views
	Credential admin.Verifier
	Sessions   admin.ExpiringSessions
	Guard      auth.Limiter
	Live       *LiveHub
	Logger     *slog.Logger
}

// Handler serves the panel on the admin listener only.
type Handler struct {
	o HandlerOptions
}

// NewHandler returns the panel surface.
func NewHandler(o HandlerOptions) *Handler {
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	return &Handler{o: o}
}

var (
	appSegment     = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)
	requestSegment = regexp.MustCompile(`^[0-9a-f]{32}$`)
	attemptSegment = regexp.MustCompile(`^[0-5]$`)
)

// route is one matched panel route.
type route struct {
	name    string
	screen  Screen
	methods []string
	public  bool
	params  []string
	cursor  bool
}

// match recognizes the exact panel routes. It never decodes the target, so an
// escaped or dotted path cannot reach a handler under another name.
func match(path string) (route, bool) {
	if !canonicalPath(path) {
		return route{}, false
	}
	get := []string{http.MethodGet}
	post := []string{http.MethodPost}
	switch path {
	case "/login":
		return route{name: "login", methods: []string{http.MethodGet, http.MethodPost}, public: true}, true
	case "/logout":
		return route{name: "logout", methods: post}, true
	case "/":
		return route{name: "page", screen: ScreenOverview, methods: get}, true
	case "/apps", "/users", "/providers", "/configuration":
		return route{name: "page", screen: Screen(path[1:]), methods: get}, true
	case "/requests", "/events":
		return route{name: "page", screen: Screen(path[1:]), methods: get, cursor: true}, true
	case "/panel/users/search":
		return route{name: "users-search", screen: ScreenUsers, methods: post}, true
	case "/panel/requests/filter":
		return route{name: "requests-filter", screen: ScreenRequests, methods: post}, true
	case "/panel/events/filter":
		return route{name: "events-filter", screen: ScreenEvents, methods: post}, true
	case "/panel/live":
		return route{name: "live", methods: get}, true
	}
	parts := strings.Split(path[1:], "/")
	switch {
	case len(parts) == 2 && parts[0] == "apps" && appSegment.MatchString(parts[1]):
		return route{name: "app", screen: ScreenApps, methods: get, params: parts[1:]}, true
	case len(parts) == 3 && parts[0] == "requests" && requestSegment.MatchString(parts[1]) && attemptSegment.MatchString(parts[2]):
		return route{name: "request", screen: ScreenRequests, methods: get, params: parts[1:]}, true
	case len(parts) == 3 && parts[0] == "panel" && parts[1] == "fragment":
		switch Screen(parts[2]) {
		case ScreenOverview, ScreenApps, ScreenProviders, ScreenConfiguration:
			return route{name: "fragment", screen: Screen(parts[2]), methods: get}, true
		}
	case len(parts) == 4 && parts[0] == "panel" && parts[1] == "fragment" && parts[2] == "apps" && appSegment.MatchString(parts[3]):
		return route{name: "app-fragment", screen: ScreenApps, methods: get, params: parts[3:]}, true
	case len(parts) == 3 && parts[0] == "assets":
		return route{name: "asset", methods: []string{http.MethodGet, http.MethodHead}, public: true, params: parts[1:]}, true
	}
	return route{}, false
}

// Owns reports whether a request target is a panel route. The admin listener
// asks before handing it over, so unknown targets keep failing closed there.
func (h *Handler) Owns(target string) bool {
	path, _, _ := strings.Cut(target, "?")
	_, ok := match(path)
	return ok
}

// ServeHTTP serves one panel route.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	status := h.serve(w, r)
	if status >= http.StatusInternalServerError {
		path, _, _ := strings.Cut(r.RequestURI, "?")
		rt, _ := match(path)
		h.o.Logger.Warn("panel request failed", "route", rt.name, "screen", string(rt.screen), "status", status, "duration_ms", time.Since(start).Milliseconds())
	}
}

func (h *Handler) serve(w http.ResponseWriter, r *http.Request) int {
	path, query, hasQuery := strings.Cut(r.RequestURI, "?")
	rt, ok := match(path)
	if !ok {
		return h.fail(w, r, Frame{}, http.StatusNotFound)
	}
	allowed := false
	for _, m := range rt.methods {
		allowed = allowed || r.Method == m
	}
	if !allowed {
		w.Header().Set("Allow", strings.Join(rt.methods, ", "))
		return h.fail(w, r, Frame{}, http.StatusMethodNotAllowed)
	}
	if rt.name == "asset" {
		commonHeaders(w)
		if hasQuery || !noBody(r) || !serveAsset(w, r, strings.Join(rt.params, "/")) {
			return h.fail(w, r, Frame{}, http.StatusNotFound)
		}
		return http.StatusOK
	}
	cursor := ""
	if hasQuery {
		key, value, _ := strings.Cut(query, "=")
		if !rt.cursor || r.Method != http.MethodGet || key != "cursor" || value == "" || len(value) > MaxCursorBytes || strings.ContainsAny(value, "&%+=") {
			return h.fail(w, r, Frame{}, http.StatusBadRequest)
		}
		cursor = value
	}
	if r.Method == http.MethodGet && !noBody(r) {
		return h.fail(w, r, Frame{}, http.StatusBadRequest)
	}
	if rt.name == "login" {
		if r.Method == http.MethodPost {
			return h.login(w, r)
		}
		if _, ok := h.session(r); ok {
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return http.StatusSeeOther
		}
		return h.render(w, r, http.StatusOK, h.o.Views.Login(LoginPage{}))
	}
	expires, ok := h.session(r)
	if !ok {
		return h.unauthorized(w, r, rt)
	}
	// Every change and every post that carries a search must come from a page
	// of this origin.
	if r.Method == http.MethodPost && !sameOrigin(r) {
		return h.fail(w, r, Frame{}, http.StatusForbidden)
	}
	frame := h.frame(rt.screen)
	ctx := r.Context()
	switch rt.name {
	case "logout":
		if _, err := readForm(w, r, "logout"); err != nil {
			return h.fail(w, r, frame, http.StatusBadRequest)
		}
		h.logout(w, r)
		return http.StatusSeeOther
	case "live":
		if h.o.Live == nil {
			return h.fail(w, r, frame, http.StatusNotFound)
		}
		if err := h.o.Live.serve(w, r, expires); errors.Is(err, ErrStreamsFull) {
			return h.failWith(w, r, frame, ErrorPage{Status: http.StatusServiceUnavailable, Title: "Live updates unavailable", Message: "Too many live connections. Reload the page to see current data."})
		}
		return http.StatusOK
	case "page":
		return h.page(ctx, w, r, frame, cursor)
	case "fragment":
		return h.fragment(ctx, w, r, rt.screen)
	case "app", "app-fragment":
		detail, err := h.o.Service.App(ctx, rt.params[0])
		if errors.Is(err, ErrNotFound) {
			return h.fail(w, r, frame, http.StatusNotFound)
		}
		if rt.name == "app" {
			return h.render(w, r, http.StatusOK, h.o.Views.App(frame, detail))
		}
		return h.render(w, r, http.StatusOK, h.o.Views.AppFragment(detail))
	case "request":
		attempt, _ := strconv.Atoi(rt.params[1])
		detail, err := h.o.Service.Request(ctx, rt.params[0], attempt)
		switch {
		case errors.Is(err, ErrNotFound):
			return h.fail(w, r, frame, http.StatusNotFound)
		case err != nil:
			return h.fail(w, r, frame, http.StatusServiceUnavailable)
		}
		return h.render(w, r, http.StatusOK, h.o.Views.Request(frame, detail))
	case "users-search":
		form, err := readForm(w, r, "user", "app", "anonymous_app")
		if err != nil {
			return h.render(w, r, http.StatusBadRequest, h.o.Views.UserResult(UserSearch{Invalid: true}))
		}
		q := UserQuery{ID: form.Get("user"), App: form.Get("app")}
		if anon := form.Get("anonymous_app"); anon != "" {
			q = UserQuery{App: anon, Anonymous: true}
			if form.Get("user") != "" || form.Get("app") != "" {
				q.ID = "invalid"
			}
		}
		result := h.o.Service.SearchUser(ctx, q)
		status := http.StatusOK
		if result.Invalid {
			status = http.StatusBadRequest
		}
		if !fragmentRequest(r) {
			return h.render(w, r, status, h.o.Views.Users(frame, &result))
		}
		return h.render(w, r, status, h.o.Views.UserResult(result))
	case "requests-filter":
		form, err := readForm(w, r, "app", "user", "user_hash", "model", "outcome", "cursor")
		if err != nil {
			return h.render(w, r, http.StatusBadRequest, h.o.Views.RequestsFragment(RequestList{Invalid: true}))
		}
		list, err := h.o.Service.Requests(ctx, RequestQuery{App: form.Get("app"), UserID: form.Get("user"), UserHash: form.Get("user_hash"), Model: form.Get("model"), Outcome: form.Get("outcome"), Cursor: form.Get("cursor")})
		if err != nil {
			return h.unavailableFragment(w, r)
		}
		status := http.StatusOK
		if list.Invalid {
			status = http.StatusBadRequest
		}
		if !fragmentRequest(r) {
			return h.render(w, r, status, h.o.Views.Requests(frame, list))
		}
		return h.render(w, r, status, h.o.Views.RequestsFragment(list))
	case "events-filter":
		form, err := readForm(w, r, "app", "type", "cursor")
		if err != nil {
			return h.render(w, r, http.StatusBadRequest, h.o.Views.EventsFragment(EventList{Invalid: true}))
		}
		list, err := h.o.Service.Events(ctx, EventQuery{App: form.Get("app"), Type: form.Get("type"), Cursor: form.Get("cursor")})
		if err != nil {
			return h.unavailableFragment(w, r)
		}
		status := http.StatusOK
		if list.Invalid {
			status = http.StatusBadRequest
		}
		if !fragmentRequest(r) {
			return h.render(w, r, status, h.o.Views.Events(frame, list))
		}
		return h.render(w, r, status, h.o.Views.EventsFragment(list))
	}
	return h.fail(w, r, frame, http.StatusNotFound)
}

func (h *Handler) page(ctx context.Context, w http.ResponseWriter, r *http.Request, frame Frame, cursor string) int {
	s, v := h.o.Service, h.o.Views
	switch frame.Screen {
	case ScreenOverview:
		return h.render(w, r, http.StatusOK, v.Overview(frame, s.Overview(ctx)))
	case ScreenApps:
		apps, availability := s.Apps(ctx)
		return h.render(w, r, http.StatusOK, v.Apps(frame, apps, availability))
	case ScreenUsers:
		return h.render(w, r, http.StatusOK, v.Users(frame, nil))
	case ScreenRequests:
		list, err := s.Requests(ctx, RequestQuery{Cursor: cursor})
		if err != nil {
			return h.fail(w, r, frame, http.StatusServiceUnavailable)
		}
		if list.Invalid {
			return h.fail(w, r, frame, http.StatusBadRequest)
		}
		return h.render(w, r, http.StatusOK, v.Requests(frame, list))
	case ScreenEvents:
		list, err := s.Events(ctx, EventQuery{Cursor: cursor})
		if err != nil {
			return h.fail(w, r, frame, http.StatusServiceUnavailable)
		}
		if list.Invalid {
			return h.fail(w, r, frame, http.StatusBadRequest)
		}
		return h.render(w, r, http.StatusOK, v.Events(frame, list))
	case ScreenProviders:
		return h.render(w, r, http.StatusOK, v.Providers(frame, s.Providers(ctx)))
	case ScreenConfiguration:
		return h.render(w, r, http.StatusOK, v.Configuration(frame, s.Configuration()))
	}
	return h.fail(w, r, frame, http.StatusNotFound)
}

func (h *Handler) fragment(ctx context.Context, w http.ResponseWriter, r *http.Request, screen Screen) int {
	s, v := h.o.Service, h.o.Views
	switch screen {
	case ScreenOverview:
		return h.render(w, r, http.StatusOK, v.OverviewFragment(s.Overview(ctx)))
	case ScreenApps:
		apps, availability := s.Apps(ctx)
		return h.render(w, r, http.StatusOK, v.AppsFragment(apps, availability))
	case ScreenProviders:
		return h.render(w, r, http.StatusOK, v.ProvidersFragment(s.Providers(ctx)))
	case ScreenConfiguration:
		return h.render(w, r, http.StatusOK, v.ConfigurationFragment(s.Configuration()))
	}
	return h.fail(w, r, Frame{}, http.StatusNotFound)
}

func (h *Handler) frame(screen Screen) Frame {
	snapshot := h.o.Service.config.Capture()
	pause := h.o.Service.pause.PauseState()
	f := Frame{Screen: screen, GlobalPaused: pause.Global, Apps: configuredApps(snapshot), At: h.o.Service.now().UTC()}
	if h.o.Live != nil {
		f.LiveURL = "/panel/live"
	}
	return f
}

// session returns the expiry of the request's single session cookie. A bearer
// token never opens a page.
func (h *Handler) session(r *http.Request) (time.Time, bool) {
	var found []*http.Cookie
	for _, c := range r.Cookies() {
		if c.Name == admin.SessionCookie {
			found = append(found, c)
		}
	}
	if len(found) != 1 || h.o.Sessions == nil {
		return time.Time{}, false
	}
	return h.o.Sessions.Expiry(found[0].Value)
}

// unauthorized sends a page to the login form and a fragment request to a
// redirect the client follows. The body never names the protected route.
func (h *Handler) unauthorized(w http.ResponseWriter, r *http.Request, rt route) int {
	htmlHeaders(w)
	if rt.name == "live" {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("event: session-expired\ndata: expired\n\n"))
		return http.StatusOK
	}
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/login")
		w.WriteHeader(http.StatusUnauthorized)
		return http.StatusUnauthorized
	}
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusUnauthorized)
		return http.StatusUnauthorized
	}
	w.Header().Set("Location", "/login")
	w.WriteHeader(http.StatusSeeOther)
	return http.StatusSeeOther
}

// login turns the admin token into the existing session, under the same guard
// and origin rule as the JSON login. The token is never echoed.
func (h *Handler) login(w http.ResponseWriter, r *http.Request) int {
	addr, ok := auth.ConnectionAddress(r)
	if !ok {
		return h.render(w, r, http.StatusBadRequest, h.o.Views.Login(LoginPage{Failed: true}))
	}
	if decision := h.o.Guard.Admit(addr); !decision.Allowed {
		w.Header().Set("Retry-After", strconv.Itoa(max(1, decision.RetryAfterSeconds())))
		return h.render(w, r, http.StatusTooManyRequests, h.o.Views.Login(LoginPage{RateLimited: true}))
	}
	if !originAllowed(r) {
		return h.render(w, r, http.StatusForbidden, h.o.Views.Login(LoginPage{Failed: true}))
	}
	form, err := readForm(w, r, "token")
	if err != nil {
		return h.render(w, r, http.StatusBadRequest, h.o.Views.Login(LoginPage{Failed: true}))
	}
	token := form.Get("token")
	form.Del("token")
	if !h.o.Credential.Verify(token) {
		if decision := h.o.Guard.Failure(addr); !decision.Allowed {
			w.Header().Set("Retry-After", strconv.Itoa(max(1, decision.RetryAfterSeconds())))
			return h.render(w, r, http.StatusTooManyRequests, h.o.Views.Login(LoginPage{RateLimited: true}))
		}
		return h.render(w, r, http.StatusUnauthorized, h.o.Views.Login(LoginPage{Failed: true}))
	}
	id, _, err := h.o.Sessions.Create()
	if err != nil {
		return h.render(w, r, http.StatusServiceUnavailable, h.o.Views.Login(LoginPage{Unavailable: true}))
	}
	commonHeaders(w)
	w.Header().Set("Cache-Control", "no-store")
	http.SetCookie(w, admin.SessionCookieFor(id, r.TLS != nil))
	w.Header().Set("Location", "/")
	w.WriteHeader(http.StatusSeeOther)
	return http.StatusSeeOther
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	for _, c := range r.Cookies() {
		if c.Name == admin.SessionCookie {
			h.o.Sessions.Delete(c.Value)
		}
	}
	commonHeaders(w)
	w.Header().Set("Cache-Control", "no-store")
	http.SetCookie(w, admin.ExpiredSessionCookie(r.TLS != nil))
	w.Header().Set("Location", "/login")
	w.WriteHeader(http.StatusSeeOther)
}

func (h *Handler) render(w http.ResponseWriter, r *http.Request, status int, c templ.Component) int {
	htmlHeaders(w)
	w.WriteHeader(status)
	if err := c.Render(r.Context(), w); err != nil {
		h.o.Logger.Warn("panel render failed", "class", "render")
	}
	return status
}

var errorPages = map[int]ErrorPage{
	http.StatusBadRequest:          {http.StatusBadRequest, "Invalid request", "The request was not accepted. Go back and try again."},
	http.StatusForbidden:           {http.StatusForbidden, "Not allowed", "This action must come from a page of this panel."},
	http.StatusNotFound:            {http.StatusNotFound, "Not found", "This page does not exist."},
	http.StatusMethodNotAllowed:    {http.StatusMethodNotAllowed, "Not allowed", "This page does not accept that method."},
	http.StatusServiceUnavailable:  {http.StatusServiceUnavailable, "Data temporarily unavailable", "The data could not be read now. Try again in a moment."},
	http.StatusInternalServerError: {http.StatusInternalServerError, "Internal error", "Something went wrong."},
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, frame Frame, status int) int {
	return h.failWith(w, r, frame, errorPages[status])
}

func (h *Handler) failWith(w http.ResponseWriter, r *http.Request, frame Frame, page ErrorPage) int {
	if fragmentRequest(r) || frame.Screen == "" {
		return h.render(w, r, page.Status, h.o.Views.ErrorFragment(page))
	}
	return h.render(w, r, page.Status, h.o.Views.Error(frame, page))
}

// fragmentRequest tells an HTMX swap from a plain form post, which gets the
// whole page so the panel stays usable without JavaScript.
func fragmentRequest(r *http.Request) bool { return r.Header.Get("HX-Request") == "true" }

func (h *Handler) unavailableFragment(w http.ResponseWriter, r *http.Request) int {
	return h.render(w, r, http.StatusServiceUnavailable, h.o.Views.ErrorFragment(errorPages[http.StatusServiceUnavailable]))
}
