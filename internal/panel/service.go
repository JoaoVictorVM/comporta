package panel

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/limits"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/useridentity"
)

// ErrUnavailable is the only failure a screen shows: the source is not
// reachable now and a manual retry may succeed.
var ErrUnavailable = errors.New("data temporarily unavailable")

// ErrNotFound means the item asked for does not exist.
var ErrNotFound = errors.New("not found")

// ConfigSource is the configuration in force and the result of its last
// reload.
type ConfigSource interface {
	Capture() *config.Snapshot
	Status() config.ReloadStatus
}

// PauseSource returns the persisted pause overrides.
type PauseSource interface {
	PauseState() storage.PauseState
}

// LimitSource returns a consistent copy of the limit state of an app and,
// optionally, of one of its users. Nil means limit state cannot be read in
// this deployment, which the screens show as not verifiable.
type LimitSource interface {
	State(p limits.Policy, user *limits.Subject) limits.State
}

// Hasher computes the app-scoped pseudonyms of the request pipeline.
type Hasher interface {
	LookupHash(app, id string) (useridentity.Hash, error)
	AnonymousHash(app string) useridentity.Hash
}

// HistoryApps lists the apps with persisted state, configured or removed.
type HistoryApps interface {
	HistoricalApps(ctx context.Context) ([]string, error)
}

// AlertReader is the optional contract of the alerts feature: the most recent
// alerts sent, newest first.
type AlertReader interface {
	RecentAlerts(ctx context.Context, limit int) ([]Alert, error)
}

// Options are the read contracts the panel composes. Store, Config and Pause
// are required.
type Options struct {
	Store    storage.PanelStore
	Config   ConfigSource
	Pause    PauseSource
	Limits   LimitSource
	Identity Hasher
	History  HistoryApps
	Alerts   AlertReader
	Logger   *slog.Logger
	Now      func() time.Time
	// CursorKey signs page cursors. Replicas share it so a cursor issued by one
	// is accepted by another; without it the process draws its own.
	CursorKey []byte
}

// Service builds the projections of every screen. It knows nothing of HTML
// and never hands internal types to a template.
type Service struct {
	store    storage.PanelStore
	config   ConfigSource
	pause    PauseSource
	limits   LimitSource
	identity Hasher
	history  HistoryApps
	alerts   AlertReader
	logger   *slog.Logger
	now      func() time.Time
	cursors  cursorSigner
}

// NewService returns the read service.
func NewService(o Options) *Service {
	s := &Service{store: o.Store, config: o.Config, pause: o.Pause, limits: o.Limits, identity: o.Identity, history: o.History, alerts: o.Alerts, logger: o.Logger, now: o.Now, cursors: newCursorSigner(o.CursorKey)}
	if s.logger == nil {
		s.logger = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s
}

// query bounds one storage read by the screen deadline. A failure is logged
// with a fixed category only.
func (s *Service) query(ctx context.Context, screen Screen, fn func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(ctx, storage.PanelQueryTimeout)
	defer cancel()
	if err := fn(ctx); err != nil {
		class := "unavailable"
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			class = "timeout"
		case errors.Is(err, storage.ErrInvalid):
			class = "invalid_data"
		}
		s.logger.Warn("panel query failed", "screen", string(screen), "class", class)
		return ErrUnavailable
	}
	return nil
}

// paused combines the global override with the app's own.
func paused(state storage.PauseState, app string) bool {
	if state.Global {
		return true
	}
	for _, entry := range state.Apps {
		if entry.App == app {
			return entry.Paused
		}
	}
	return false
}

func appPaused(state storage.PauseState, app string) bool {
	for _, entry := range state.Apps {
		if entry.App == app {
			return entry.Paused
		}
	}
	return false
}

// window is the day and month of one app in its own timezone.
type window struct {
	day, monthStart string
	loc             *time.Location
}

func windowFor(now time.Time, timezone string) window {
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		loc = time.UTC
	}
	local := now.In(loc)
	return window{day: local.Format(time.DateOnly), monthStart: time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc).Format(time.DateOnly), loc: loc}
}

// appTimezone is the timezone in force for an app's day.
func appTimezone(snapshot *config.Snapshot, app config.App) string {
	if app.Timezone != "" {
		return app.Timezone
	}
	if tz := snapshot.Timezone(); tz != "" {
		return tz
	}
	return "UTC"
}

// limitState reads limit state, or reports that it cannot be verified.
func (s *Service) limitState(snapshot *config.Snapshot, app string, user *limits.Subject) (limits.State, Availability) {
	if s.limits == nil {
		return limits.State{}, NotVerifiable
	}
	policy, ok := limits.PolicyFor(snapshot, app)
	if !ok {
		return limits.State{}, NotVerifiable
	}
	state := s.limits.State(policy, user)
	if state.Degraded {
		return state, NotVerifiable
	}
	return state, Available
}

func limitRows(views []limits.LimitView, scope limits.Scope) []LimitRow {
	var out []LimitRow
	for _, v := range views {
		if v.Scope != scope {
			continue
		}
		out = append(out, LimitRow{
			Name: v.LimitName, Scope: string(v.Scope), Kind: string(v.Kind), Unit: string(v.Unit), Dimension: string(v.Dimension),
			Used: unitValue(v.Unit, v.Used), Reserved: unitValue(v.Unit, v.Reserved), Limit: unitValue(v.Unit, v.Limit),
			RenewsAt: v.RenewsAt, Unknown: v.Unknown, Pressure: PressureOf(v.Used, v.Reserved, v.Limit, v.Unknown),
		})
	}
	return out
}

// unitValue formats a ceiling value in its unit: dollars for money, a whole
// number otherwise.
func unitValue(unit limits.Unit, v int64) string {
	if unit == limits.UnitMicrousd {
		return "$" + config.Microusd(v).String()
	}
	return strconv.FormatInt(v, 10)
}

// configuredApps returns the configured apps in a stable order.
func configuredApps(snapshot *config.Snapshot) []string {
	return slices.Clone(snapshot.AppNames())
}

// ShortHash abbreviates a pseudonym for display, prefixed so it is never
// mistaken for a user identifier.
func ShortHash(hash string) string {
	if len(hash) < 8 {
		return ""
	}
	return "u_" + hash[:8]
}
