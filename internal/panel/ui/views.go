// Package ui renders the admin panel. Templates receive only the projections
// of package panel, and every value reaching an attribute that changes
// behavior comes from a closed set defined there.
package ui

import (
	"strconv"
	"time"

	"github.com/a-h/templ"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/panel"
)

//go:generate go run github.com/a-h/templ/cmd/templ@v0.3.906 generate

// Views implements panel.Views.
type Views struct{}

var _ panel.Views = Views{}

func (Views) Login(p panel.LoginPage) templ.Component { return loginPage(p) }
func (Views) Error(f panel.Frame, e panel.ErrorPage) templ.Component {
	return layout(f, e.Title, errorBody(e))
}
func (Views) ErrorFragment(e panel.ErrorPage) templ.Component { return errorBody(e) }
func (Views) Overview(f panel.Frame, o panel.Overview) templ.Component {
	return layout(f, "Overview", overviewPage(o))
}
func (Views) OverviewFragment(o panel.Overview) templ.Component { return overviewBody(o) }
func (Views) Apps(f panel.Frame, apps []panel.AppSummary, a panel.Availability) templ.Component {
	return layout(f, "Apps", appsPage(apps, a))
}
func (Views) AppsFragment(apps []panel.AppSummary, a panel.Availability) templ.Component {
	return appsBody(apps, a)
}
func (Views) App(f panel.Frame, d panel.AppDetail) templ.Component {
	return layout(f, "App "+d.App, appPage(d))
}
func (Views) AppFragment(d panel.AppDetail) templ.Component { return appBody(d) }
func (Views) Users(f panel.Frame, result *panel.UserSearch) templ.Component {
	return layout(f, "Users", usersPage(f, result))
}
func (Views) UserResult(r panel.UserSearch) templ.Component { return userResult(r) }
func (Views) Requests(f panel.Frame, l panel.RequestList) templ.Component {
	return layout(f, "Requests", requestsPage(f, l))
}
func (Views) RequestsFragment(l panel.RequestList) templ.Component { return requestsBody(l) }
func (Views) Request(f panel.Frame, d panel.RequestDetail) templ.Component {
	return layout(f, "Request", requestPage(d))
}
func (Views) Events(f panel.Frame, l panel.EventList) templ.Component {
	return layout(f, "Events", eventsPage(f, l))
}
func (Views) EventsFragment(l panel.EventList) templ.Component { return eventsBody(l) }
func (Views) Providers(f panel.Frame, p panel.Providers) templ.Component {
	return layout(f, "Providers", providersPage(p))
}
func (Views) ProvidersFragment(p panel.Providers) templ.Component { return providersBody(p) }
func (Views) Configuration(f panel.Frame, c panel.Configuration) templ.Component {
	return layout(f, "Configuration", configurationPage(c))
}
func (Views) ConfigurationFragment(c panel.Configuration) templ.Component {
	return configurationBody(c)
}

// htmxConfig keeps HTMX to this origin, without script evaluation, history
// snapshots or the indicator style it would otherwise inject inline.
const htmxConfig = `{"selfRequestsOnly":true,"allowScriptTags":false,"allowEval":false,"historyCacheSize":0,"refreshOnHistoryMiss":true,"includeIndicatorStyles":false,"defaultSwapStyle":"innerHTML"}`

func usd(m config.Microusd) string { return panel.Cost(m) }

func count(n int64) string { return strconv.FormatInt(n, 10) }

func number(n int) string { return strconv.Itoa(n) }

func stamp(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format("2006-01-02 15:04:05 UTC")
}

func iso(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// renews is the time left until a ceiling renews, like "renews in 6h".
func renews(at *time.Time, now time.Time) string {
	if at == nil {
		return "no fixed renewal"
	}
	left := at.Sub(now)
	switch {
	case left <= 0:
		return "renewing now"
	case left < time.Minute:
		return "renews in under a minute"
	case left < time.Hour:
		return "renews in " + strconv.Itoa(int(left/time.Minute)) + "m"
	case left < 48*time.Hour:
		return "renews in " + strconv.Itoa(int(left/time.Hour)) + "h"
	}
	return "renews in " + strconv.Itoa(int(left/(24*time.Hour))) + "d"
}

func pressureText(p panel.Pressure) string {
	switch {
	case !p.Known:
		return "Unknown: cannot be verified"
	case !p.Limit:
		return "No limit"
	}
	return strconv.Itoa(p.Percent) + "% used"
}

// barWidth picks one of a fixed set of width classes, so no style attribute is
// ever needed under the content security policy.
func barWidth(p panel.Pressure) string {
	if !p.Known || !p.Limit {
		return "w-0"
	}
	switch {
	case p.Percent >= 100:
		return "w-full"
	case p.Percent >= 90:
		return "w-11/12"
	case p.Percent >= 80:
		return "w-4/5"
	case p.Percent >= 70:
		return "w-8/12"
	case p.Percent >= 60:
		return "w-3/5"
	case p.Percent >= 50:
		return "w-1/2"
	case p.Percent >= 40:
		return "w-2/5"
	case p.Percent >= 30:
		return "w-4/12"
	case p.Percent >= 20:
		return "w-1/5"
	case p.Percent >= 10:
		return "w-1/12"
	case p.Percent > 0:
		return "w-1"
	}
	return "w-0"
}

func barColor(p panel.Pressure) string {
	switch p.Level() {
	case "exceeded":
		return "bg-bad"
	case "high":
		return "bg-warn"
	case "normal":
		return "bg-ok"
	}
	return "bg-unknown"
}

func healthClass(h panel.Health) string {
	switch h {
	case panel.HealthHealthy:
		return "bg-ok-bg text-ok"
	case panel.HealthDegraded:
		return "bg-warn-bg text-warn"
	case panel.HealthUnhealthy:
		return "bg-bad-bg text-bad"
	}
	return "bg-unknown-bg text-unknown"
}

func outcomeClass(outcome string) string {
	switch outcome {
	case "ok", "cached":
		return "bg-ok-bg text-ok"
	case "downgraded", "fallback":
		return "bg-warn-bg text-warn"
	case "blocked", "loop_cut":
		return "bg-bad-bg text-bad"
	}
	return "bg-unknown-bg text-unknown"
}

func requestURL(r panel.RequestRow) templ.SafeURL {
	return templ.SafeURL("/requests/" + r.RequestID + "/" + strconv.Itoa(r.Attempt))
}

func eventRequestURL(e panel.EventRow) templ.SafeURL {
	return templ.SafeURL("/requests/" + e.Request + "/" + strconv.Itoa(e.Attempt))
}

func appURL(app string) templ.SafeURL { return templ.SafeURL("/apps/" + app) }

func appFragmentURL(app string) string { return "/panel/fragment/apps/" + app }

func cursorURL(screen panel.Screen, cursor string) templ.SafeURL {
	return templ.SafeURL(screen.Path() + "?cursor=" + cursor)
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func pausedSpend(apps []panel.AppSpend) []string {
	var out []string
	for _, a := range apps {
		if a.Paused {
			out = append(out, a.App)
		}
	}
	return out
}

func pausedSummaries(apps []panel.AppSummary) []string {
	var out []string
	for _, a := range apps {
		if a.Paused {
			out = append(out, a.App)
		}
	}
	return out
}
