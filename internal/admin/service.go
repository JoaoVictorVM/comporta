package admin

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/runtimecontrol"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/useridentity"
)

// Failures of the operational services, mapped to fixed HTTP answers.
var (
	ErrAppNotFound   = errors.New("app not found")
	ErrInvalidInput  = errors.New("invalid request")
	ErrUserBusy      = errors.New("user busy")
	ErrDeletion      = errors.New("deletion failed; no records were removed")
	ErrStorageFailed = errors.New("admin storage unavailable")
)

var appName = regexp.MustCompile(`^[a-z0-9-]{1,40}$`)

// SnapshotSource provides the configuration in force.
type SnapshotSource interface {
	Capture() *config.Snapshot
}

// Hasher computes the same app-scoped pseudonyms as a request.
type Hasher interface {
	LookupHash(app, id string) (useridentity.Hash, error)
	AnonymousHash(app string) useridentity.Hash
}

// Metering is the usage queue a deletion drains and holds still.
type Metering interface {
	Maintain(ctx context.Context, fn func(context.Context) error) error
	HasPending(subjects []storage.SubjectRef) bool
}

// LimitMaintenance is the limit coordinator seen by a deletion.
type LimitMaintenance interface {
	ForgetSubjects(subjects []storage.SubjectRef, persist func() error) (int, error)
	Apps() []string
}

// ServiceOptions configures the operational services.
type ServiceOptions struct {
	Control  *runtimecontrol.Control
	Source   SnapshotSource
	Identity Hasher
	History  storage.ForgetStore
	Metering Metering
	Limits   LimitMaintenance
	Usage    storage.UsageQueryStore
	Logger   *slog.Logger
	Now      func() time.Time
	// PrepareTimeout bounds the wait for the user's requests in flight.
	PrepareTimeout time.Duration
}

// Service holds the operations of the admin API, independent of HTTP.
type Service struct {
	control  *runtimecontrol.Control
	source   SnapshotSource
	identity Hasher
	history  storage.ForgetStore
	metering Metering
	limits   LimitMaintenance
	usage    storage.UsageQueryStore
	logger   *slog.Logger
	now      func() time.Time
	prepare  time.Duration
	// cursorKey signs usage cursors; it lives only in this process.
	cursorKey []byte
}

// NewService returns the operational services.
func NewService(options ServiceOptions) *Service {
	s := &Service{
		control: options.Control, source: options.Source, identity: options.Identity, history: options.History,
		metering: options.Metering, limits: options.Limits, usage: options.Usage, logger: options.Logger, now: options.Now, prepare: options.PrepareTimeout,
		cursorKey: newCursorKey(),
	}
	if s.prepare <= 0 {
		s.prepare = 10 * time.Second
	}
	if s.logger == nil {
		s.logger = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s
}

// PauseSelector names exactly one target: an app or every app.
type PauseSelector struct {
	App string
	All bool
}

// PauseOutcome is the effective state after a pause or resume.
type PauseOutcome struct {
	Selector     PauseSelector
	GlobalPaused bool
	AppPaused    bool
	Changed      bool
	UpdatedAt    time.Time
}

// Effective reports whether the selected app still refuses traffic.
func (o PauseOutcome) Effective() bool { return o.GlobalPaused || o.AppPaused }

// PauseState returns the persisted overrides; the caller combines them with the
// configured apps.
func (s *Service) PauseState() storage.PauseState {
	return s.control.State()
}

// SetPause pauses or resumes. Pausing an app requires it to be configured;
// resuming also accepts an app that only has a persisted override, so the
// override of a removed app can always be cleared.
func (s *Service) SetPause(ctx context.Context, selector PauseSelector, pause bool) (PauseOutcome, error) {
	if selector.All == (selector.App != "") || (!selector.All && !appName.MatchString(selector.App)) {
		return PauseOutcome{}, ErrInvalidInput
	}
	if !selector.All {
		_, configured := s.source.Capture().App(selector.App)
		if !configured && (pause || !overridden(s.control.State(), selector.App)) {
			return PauseOutcome{}, ErrAppNotFound
		}
	}
	at := s.now().UTC().Truncate(time.Microsecond)
	result, err := s.control.Apply(ctx, storage.PauseChange{All: selector.All, App: selector.App, Paused: pause, At: at})
	if err != nil {
		s.logger.Error("pause state could not be saved; the operation was not applied")
		return PauseOutcome{}, ErrStorageFailed
	}
	outcome := PauseOutcome{Selector: selector, GlobalPaused: result.State.Global, Changed: result.Changed, UpdatedAt: at}
	if !selector.All {
		for _, entry := range result.State.Apps {
			if entry.App == selector.App {
				outcome.AppPaused = entry.Paused
				if !result.Changed {
					outcome.UpdatedAt = entry.UpdatedAt
				}
			}
		}
	} else if !result.Changed && !result.State.GlobalUpdatedAt.IsZero() {
		outcome.UpdatedAt = result.State.GlobalUpdatedAt
	}
	return outcome, nil
}

func overridden(state storage.PauseState, app string) bool {
	for _, entry := range state.Apps {
		if entry.App == app {
			return true
		}
	}
	return false
}

// ForgetResult reports a deletion without naming the user or its apps.
type ForgetResult struct {
	Counts  storage.ForgetCounts
	Cleared int
}

// Forget deletes every record of an identified end user in every app with
// state, configured or removed. The raw identifier is used only in memory to
// compute each app's pseudonym. New requests of that user wait behind a
// barrier while the requests already admitted finish and reach accounting;
// then usage, limits and storage are held still while one transaction deletes
// the history. Any failure deletes nothing.
func (s *Service) Forget(ctx context.Context, id string) (ForgetResult, error) {
	if useridentity.ValidateID(id) != nil {
		return ForgetResult{}, ErrInvalidInput
	}
	prepare, cancel := context.WithTimeout(ctx, s.prepare)
	defer cancel()
	barrier, err := s.control.BeginForget(prepare, func(app string) (string, error) {
		hash, err := s.identity.LookupHash(app, id)
		if err != nil {
			return "", err
		}
		return hash.Hex(), nil
	})
	if err != nil {
		s.logger.Warn("user deletion postponed", "reason", "another deletion in progress")
		return ForgetResult{}, ErrUserBusy
	}
	defer barrier.End()
	apps, err := s.apps(prepare)
	if err != nil {
		return ForgetResult{}, ErrDeletion
	}
	if err := barrier.Include(apps); err != nil {
		return ForgetResult{}, ErrDeletion
	}
	if err := barrier.Drain(prepare); err != nil {
		s.logger.Warn("user deletion postponed", "reason", "requests still in flight")
		return ForgetResult{}, ErrUserBusy
	}
	subjects := barrier.Subjects()
	var result ForgetResult
	persist := func() error {
		counts, err := s.history.ForgetSubjects(ctx, subjects)
		result.Counts = counts
		return err
	}
	err = s.metering.Maintain(ctx, func(context.Context) error {
		if s.metering.HasPending(subjects) {
			return storage.BusyError{Reason: "usage queue not drained"}
		}
		cleared, err := s.limits.ForgetSubjects(subjects, persist)
		result.Cleared = cleared
		return err
	})
	switch {
	case err == nil:
		return result, nil
	case errors.Is(err, storage.ErrBusy):
		reason := "pending usage"
		var busy storage.BusyError
		if errors.As(err, &busy) {
			reason = busy.Reason
		}
		s.logger.Warn("user deletion postponed", "reason", reason)
		return ForgetResult{}, ErrUserBusy
	}
	s.logger.Error("user deletion failed; no records were removed")
	return ForgetResult{}, ErrDeletion
}

// apps is every app that may hold state of a user: configured, persisted in
// any table, overridden or still in the limit projection.
func (s *Service) apps(ctx context.Context) ([]string, error) {
	seen := map[string]bool{}
	for _, app := range s.source.Capture().AppNames() {
		seen[app] = true
	}
	historical, err := s.history.HistoricalApps(ctx)
	if err != nil {
		return nil, err
	}
	for _, app := range historical {
		seen[app] = true
	}
	for _, app := range s.limits.Apps() {
		seen[app] = true
	}
	for _, entry := range s.control.State().Apps {
		seen[entry.App] = true
	}
	apps := make([]string, 0, len(seen))
	for app := range seen {
		if appName.MatchString(app) {
			apps = append(apps, app)
		}
	}
	return apps, nil
}
