package panel

import (
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
)

// Screen names a page of the panel. It is a closed set: values reach HTML
// attributes, event names and fragment routes only from here.
type Screen string

const (
	ScreenOverview      Screen = "overview"
	ScreenApps          Screen = "apps"
	ScreenUsers         Screen = "users"
	ScreenRequests      Screen = "requests"
	ScreenEvents        Screen = "events"
	ScreenProviders     Screen = "providers"
	ScreenConfiguration Screen = "configuration"
)

// Screens is the navigation order.
var Screens = []Screen{ScreenOverview, ScreenApps, ScreenUsers, ScreenRequests, ScreenEvents, ScreenProviders, ScreenConfiguration}

// Title is the human name of a screen.
func (s Screen) Title() string {
	switch s {
	case ScreenOverview:
		return "Overview"
	case ScreenApps:
		return "Apps"
	case ScreenUsers:
		return "Users"
	case ScreenRequests:
		return "Requests"
	case ScreenEvents:
		return "Events"
	case ScreenProviders:
		return "Providers"
	case ScreenConfiguration:
		return "Configuration"
	}
	return ""
}

// Path is the page route of a screen.
func (s Screen) Path() string {
	if s == ScreenOverview {
		return "/"
	}
	return "/" + string(s)
}

// Health is the observed state of a provider.
type Health string

const (
	HealthUnknown   Health = "unknown"
	HealthHealthy   Health = "healthy"
	HealthDegraded  Health = "degraded"
	HealthUnhealthy Health = "unhealthy"
)

// Label is the text shown next to the health color, which is never the only
// indicator.
func (h Health) Label() string {
	switch h {
	case HealthHealthy:
		return "Healthy"
	case HealthDegraded:
		return "Degraded"
	case HealthUnhealthy:
		return "Unhealthy"
	}
	return "Unknown"
}

// Availability is the state of one source of a screen.
type Availability int

const (
	Available Availability = iota
	// Unavailable means the source failed; the screen shows it instead of zero.
	Unavailable
	// NotVerifiable means limit state cannot be proven, never a free budget.
	NotVerifiable
)

// Pressure is how close a ceiling is, as a whole percentage and a state that
// does not depend on color.
type Pressure struct {
	Percent int
	Known   bool
	Limit   bool
}

// Level is the severity class of a pressure, used for color and text.
func (p Pressure) Level() string {
	switch {
	case !p.Known:
		return "unknown"
	case !p.Limit:
		return "none"
	case p.Percent >= 100:
		return "exceeded"
	case p.Percent >= 80:
		return "high"
	}
	return "normal"
}

// PressureOf computes the pressure of used plus reserved against a limit.
func PressureOf(used, reserved, limit int64, unknown bool) Pressure {
	if unknown {
		return Pressure{}
	}
	if limit <= 0 {
		return Pressure{Known: true}
	}
	total := used + reserved
	if total < 0 {
		total = 0
	}
	percent := total * 100 / limit
	if percent > 999 {
		percent = 999
	}
	return Pressure{Percent: int(percent), Known: true, Limit: true}
}

// Overview is the first screen.
type Overview struct {
	At             time.Time
	GlobalPaused   bool
	Spend          Availability
	Apps           []AppSpend
	TodayCost      config.Microusd
	MonthCost      config.Microusd
	TodayRequests  int64
	TodayTokens    int64
	Incomplete     int64
	CacheSaved     config.Microusd
	DowngradeSaved config.Microusd
	Alerts         AlertsView
}

// AppSpend is the spend of one app in its own timezone.
type AppSpend struct {
	App           string
	Paused        bool
	Timezone      string
	Day           string
	Today         config.Microusd
	Month         config.Microusd
	TodayRequests int64
	DayBudget     config.Microusd
	MonthBudget   config.Microusd
	DayPressure   Pressure
	MonthPressure Pressure
	Incomplete    int64
}

// AlertsView is the optional list of recent alerts.
type AlertsView struct {
	Enabled bool
	Failed  bool
	Items   []Alert
}

// Alert is one sent alert, as the alerts feature describes it.
type Alert struct {
	At       time.Time
	Severity string
	Title    string
	App      string
}

// AppSummary is one row of the apps list.
type AppSummary struct {
	App          string
	Paused       bool
	Policy       string
	PrimaryModel string
	Today        config.Microusd
	Month        config.Microusd
	Pressure     Pressure
	PressureName string
	Limits       Availability
}

// AppDetail is the screen of one app.
type AppDetail struct {
	At              time.Time
	App             string
	Paused          bool
	GlobalPaused    bool
	Policy          string
	DowngradeTo     string
	FailMode        string
	Timezone        string
	ActiveTimezone  string
	PendingTimezone string
	PendingAt       *time.Time
	Degraded        bool
	Limits          Availability
	LimitRows       []LimitRow
	Spend           Availability
	Today           config.Microusd
	Month           config.Microusd
	Models          []ModelShare
	TopUsers        []UserCost
	Period          string
}

// LimitRow is one ceiling. Values are decimal strings of their unit.
type LimitRow struct {
	Name      string
	Scope     string
	Kind      string
	Unit      string
	Used      string
	Reserved  string
	Limit     string
	RenewsAt  *time.Time
	Unknown   bool
	Pressure  Pressure
	Dimension string
}

// ModelShare is one model of the mix.
type ModelShare struct {
	Model    string
	Attempts int64
	Cost     config.Microusd
	Percent  int
}

// UserCost is one pseudonymous user ranked by cost.
type UserCost struct {
	Hash      string
	Anonymous bool
	Cost      config.Microusd
	Requests  int64
}

// UserSearch is the result of a user search, grouped by app. It never holds
// the identifier that was typed.
type UserSearch struct {
	At      time.Time
	Invalid bool
	Spend   Availability
	Groups  []UserGroup
}

// UserGroup is the state of one pseudonym in one app.
type UserGroup struct {
	App       string
	Hash      string
	Anonymous bool
	Today     config.Microusd
	History   []UserDay
	Limits    Availability
	LimitRows []LimitRow
	Removed   bool
}

// UserDay is one day of a user's history.
type UserDay struct {
	Day      string
	Requests int64
	Cost     config.Microusd
	Tokens   int64
}

// RequestRow is the safe projection of one attempt.
type RequestRow struct {
	RequestID      string
	Attempt        int
	StartedAt      time.Time
	App            string
	User           string
	Anonymous      bool
	Provider       string
	ModelRequested string
	ModelUsed      string
	Status         string
	Outcome        string
	Policy         string
	Cost           config.Microusd
	CostKnown      bool
	InputTokens    string
	OutputTokens   string
	LatencyMS      string
	TTFBMS         string
	Flags          []string
}

// RequestDetail adds the metadata of one attempt. Content, raw errors, headers
// and identifiers never reach it.
type RequestDetail struct {
	RequestRow
	FinishedAt       time.Time
	Operation        string
	ErrorClass       string
	ErrorCode        string
	TerminalCode     string
	TerminalStatus   string
	PolicyLimit      string
	PolicyScope      string
	PolicyReason     string
	PolicyRenewsAt   *time.Time
	FallbackFrom     string
	FallbackCause    string
	ReservedCost     config.Microusd
	CacheSaved       config.Microusd
	DowngradeSaved   config.Microusd
	EstimateMethod   string
	EstimatedInput   int64
	EstimatedOutput  int64
	UsageMissing     bool
	Recovered        bool
	EstimateExceeded bool
	LimitsDegraded   bool
	CacheRead        string
	CacheWrite       string
	Detections       []Detection
	Terminal         bool
}

// Detection is a category and count of sensitive data, never a value.
type Detection struct {
	Category string
	Count    int64
	Action   string
}

// RequestList is one page of the requests screen.
type RequestList struct {
	Rows    []RequestRow
	Next    string
	Shown   int
	Capped  bool
	Filters RequestFilters
	Invalid bool
}

// RequestFilters are the validated filters echoed back to the form. The raw
// user identifier is never among them.
type RequestFilters struct {
	App      string
	UserHash string
	Model    string
	Outcome  string
}

// EventRow is the formatted projection of one event.
type EventRow struct {
	ID      string
	At      time.Time
	Type    string
	Label   string
	App     string
	Request string
	Attempt int
	Details []string
	User    string
}

// EventList is one page of the events screen.
type EventList struct {
	Rows    []EventRow
	Next    string
	Filters EventFilters
	Invalid bool
}

// EventFilters are the validated event filters.
type EventFilters struct {
	App  string
	Type string
}

// ProviderCard is the observed health of one provider.
type ProviderCard struct {
	Name           string
	Removed        bool
	Health         Health
	Attempts       int64
	Successes      int64
	Failures       int64
	ErrorRate      string
	P50MS          string
	P95MS          string
	CacheReadRatio string
	Buckets        []ProviderPoint
}

// ProviderPoint is one five-minute bucket; Present is false when nothing was
// sent in it, which is never drawn as zero.
type ProviderPoint struct {
	Start    time.Time
	Present  bool
	Attempts int64
	Failures int64
}

// Providers is the providers screen.
type Providers struct {
	From, Until time.Time
	Stats       Availability
	Cards       []ProviderCard
}

// Configuration is the configuration screen.
type Configuration struct {
	Available     bool
	YAML          string
	Generation    int64
	LastAttemptAt time.Time
	LastSuccessAt time.Time
	Result        string
	Rejected      bool
	Diagnostics   []Diagnostic
}

// Diagnostic is one structured validation message, already safe to show.
type Diagnostic struct {
	Code    string
	Path    string
	Message string
}
