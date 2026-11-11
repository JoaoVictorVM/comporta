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

// ServiceOptions configures the operational services.
type ServiceOptions struct {
	Control *runtimecontrol.Control
	Source  SnapshotSource
	Logger  *slog.Logger
	Now     func() time.Time
}

// Service holds the operations of the admin API, independent of HTTP.
type Service struct {
	control *runtimecontrol.Control
	source  SnapshotSource
	logger  *slog.Logger
	now     func() time.Time
}

// NewService returns the operational services.
func NewService(options ServiceOptions) *Service {
	s := &Service{control: options.Control, source: options.Source, logger: options.Logger, now: options.Now}
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
