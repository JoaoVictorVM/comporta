package config

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	missingWarnInterval = 60 * time.Second
	stillActiveSuffix   = " (previous configuration still active)"
)

// Clock provides the current time.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// ReloadResult is the outcome of the latest attempt.
type ReloadResult string

const (
	ResultInitial   ReloadResult = "initial"
	ResultApplied   ReloadResult = "applied"
	ResultUnchanged ReloadResult = "unchanged"
	ResultRejected  ReloadResult = "rejected"
)

// ReloadStatus describes the latest attempt. It is safe to expose publicly.
type ReloadStatus struct {
	ActiveGeneration int64
	LastAttemptAt    time.Time
	LastSuccessAt    time.Time
	Result           ReloadResult
	Errors           []Diagnostic
}

// MarshalJSON renders unknown times as null and errors always as a list.
func (s ReloadStatus) MarshalJSON() ([]byte, error) {
	out := struct {
		ActiveGeneration int64        `json:"active_generation"`
		LastAttemptAt    *time.Time   `json:"last_attempt_at"`
		LastSuccessAt    *time.Time   `json:"last_success_at"`
		Result           ReloadResult `json:"result"`
		Errors           []Diagnostic `json:"errors"`
	}{ActiveGeneration: s.ActiveGeneration, Result: s.Result, Errors: s.Errors}
	if !s.LastAttemptAt.IsZero() {
		out.LastAttemptAt = &s.LastAttemptAt
	}
	if !s.LastSuccessAt.IsZero() {
		out.LastSuccessAt = &s.LastSuccessAt
	}
	if out.Errors == nil {
		out.Errors = []Diagnostic{}
	}
	return json.Marshal(out)
}

// Event is a sanitized notification: it carries only masked projections, never a
// snapshot, file bytes or raw parser errors.
type Event struct {
	Type       EventType    `json:"type"`
	Generation int64        `json:"generation"`
	At         time.Time    `json:"at"`
	Changes    []Change     `json:"changes,omitempty"`
	Errors     []Diagnostic `json:"errors,omitempty"`
}

// ChangeSink receives notifications. It runs on the writer path, so it must
// return quickly. A failure or panic is contained and never undoes a publication.
type ChangeSink interface {
	Publish(ctx context.Context, event Event) error
}

// ManagerOptions configures a Manager. Only Path is required.
type ManagerOptions struct {
	Path    string
	Options Options
	Logger  *slog.Logger
	Clock   Clock
	Sink    ChangeSink

	newWatcher func() (fsWatcher, error)
	inspect    permissionInspector
}

type publishedState struct {
	snapshot *Snapshot
	status   ReloadStatus
}

type observationKind int

const (
	observedPresent observationKind = iota
	observedMissing
	observedUnreadable
)

// observation identifies what was last read, so reconciliation detects a change
// from the bytes alone rather than from modification time or size.
type observation struct {
	kind observationKind
	sum  [sha256.Size]byte
}

// Manager is the only writer of the active configuration. Readers never block:
// they load a published state atomically, and snapshot and status always stay
// consistent with each other.
type Manager struct {
	path       string
	opts       Options
	logger     *slog.Logger
	clock      Clock
	sink       ChangeSink
	newWatcher func() (fsWatcher, error)

	state atomic.Pointer[publishedState]

	mu              sync.Mutex
	observed        observation
	lastMissingWarn time.Time
}

// NewManager performs the initial load. There is no fallback at startup: any
// failure is returned, as an *Error whenever it concerns the configuration.
func NewManager(options ManagerOptions) (*Manager, error) {
	path, err := filepath.Abs(options.Path)
	if err != nil {
		return nil, singleError(Diagnostic{Code: CodeConfigUnreadable, Message: "configuration path cannot be resolved"})
	}
	opts := options.Options
	opts.ResolveSecrets = true
	if opts.SystemTimezone == "" {
		opts.SystemTimezone = DetectSystemTimezone()
	}
	if opts.Environment == nil {
		opts.Environment, err = CaptureEnvironment(filepath.Dir(path), os.Environ(), opts.FS)
		if err != nil {
			return nil, err
		}
	}

	m := &Manager{
		path:       path,
		opts:       opts,
		logger:     options.Logger,
		clock:      options.Clock,
		sink:       options.Sink,
		newWatcher: options.newWatcher,
	}
	if m.logger == nil {
		m.logger = slog.New(slog.DiscardHandler)
	}
	if m.clock == nil {
		m.clock = systemClock{}
	}
	if m.newWatcher == nil {
		m.newWatcher = newFSNotifyWatcher
	}

	data, readErr := readLimited(opts.fileSystem(), path, MaxConfigBytes+1)
	m.observed = observe(data, readErr)
	if err := checkConfigRead(data, readErr); err != nil {
		return nil, err
	}
	candidate, err := LoadBytes(data, path, opts)
	if err != nil {
		return nil, err
	}

	now := m.clock.Now().UTC()
	snapshot := newSnapshot(1, now, candidate)
	m.state.Store(&publishedState{
		snapshot: snapshot,
		status:   ReloadStatus{ActiveGeneration: 1, LastAttemptAt: now, LastSuccessAt: now, Result: ResultInitial},
	})

	m.logWarnings(candidate.Warnings())
	inspect := options.inspect
	if inspect == nil {
		inspect = inspectPermissions
	}
	m.logWarnings(checkPermissions([]string{path, candidate.cfg.DataDir}, inspect))
	return m, nil
}

func observe(data []byte, err error) observation {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return observation{kind: observedMissing}
	case err != nil:
		return observation{kind: observedUnreadable}
	}
	return observation{kind: observedPresent, sum: sha256.Sum256(data)}
}

func (m *Manager) logWarnings(warnings []Diagnostic) {
	for _, warning := range warnings {
		m.logger.Warn(warning.String(), "code", warning.Code)
	}
}

// Capture returns the active snapshot. Hold it for the whole operation.
func (m *Manager) Capture() *Snapshot { return m.state.Load().snapshot }

// Status returns a copy of the latest reload status.
func (m *Manager) Status() ReloadStatus {
	status := m.state.Load().status
	status.Errors = slices.Clone(status.Errors)
	return status
}

// Reload rereads the file and any secret files, then publishes the result if it
// is valid. An invalid result only updates the status: the active snapshot and
// generation stay as they were.
func (m *Manager) Reload(ctx context.Context) ReloadStatus {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.clock.Now().UTC()
	data, readErr := readLimited(m.opts.fileSystem(), m.path, MaxConfigBytes+1)
	m.observed = observe(data, readErr)
	if errors.Is(readErr, fs.ErrNotExist) {
		m.recordMissing(ctx, now)
		return m.Status()
	}
	m.lastMissingWarn = time.Time{}
	if err := checkConfigRead(data, readErr); err != nil {
		m.reject(ctx, now, err)
		return m.Status()
	}
	candidate, err := LoadBytes(data, m.path, m.opts)
	if err != nil {
		m.reject(ctx, now, err)
		return m.Status()
	}

	current := m.state.Load()
	if restart := staticChanges(&current.snapshot.cfg, candidate); len(restart) > 0 {
		m.reject(ctx, now, &Error{Diagnostics: restart})
		return m.Status()
	}

	m.logWarnings(candidate.Warnings())
	next := newSnapshot(current.snapshot.generation+1, now, candidate)
	changes := diffSnapshots(current.snapshot, next)
	if len(changes) == 0 {
		m.state.Store(&publishedState{snapshot: current.snapshot, status: ReloadStatus{
			ActiveGeneration: current.snapshot.generation, LastAttemptAt: now, LastSuccessAt: now, Result: ResultUnchanged,
		}})
		m.logger.Info("config reload: no changes", "generation", current.snapshot.generation)
		return m.Status()
	}

	m.state.Store(&publishedState{snapshot: next, status: ReloadStatus{
		ActiveGeneration: next.generation, LastAttemptAt: now, LastSuccessAt: now, Result: ResultApplied,
	}})
	m.logger.Info(reloadSummary(len(next.cfg.Apps), changes), "generation", next.generation)
	m.notify(ctx, Event{Type: EventConfigChanged, Generation: next.generation, At: now, Changes: changes})
	return m.Status()
}

func (m *Manager) reject(ctx context.Context, now time.Time, err error) {
	var failure *Error
	if !errors.As(err, &failure) {
		failure = &Error{Diagnostics: []Diagnostic{{Code: CodeConfigUnreadable, Message: "configuration could not be loaded"}}}
	}
	current := m.state.Load()
	status := current.status
	status.LastAttemptAt = now
	status.Result = ResultRejected
	status.Errors = failure.Diagnostics
	m.state.Store(&publishedState{snapshot: current.snapshot, status: status})

	m.logger.Error(rejectionSummary(failure.Diagnostics), "generation", status.ActiveGeneration, "errors", failure.Diagnostics)
	m.notify(ctx, Event{
		Type: EventConfigReloadFailed, Generation: status.ActiveGeneration, At: now, Errors: slices.Clone(failure.Diagnostics),
	})
}

func rejectionSummary(diagnostics []Diagnostic) string {
	if len(diagnostics) == 0 {
		return "config reload rejected" + stillActiveSuffix
	}
	first := strings.TrimSuffix(diagnostics[0].String(), stillActiveSuffix)
	if extra := len(diagnostics) - 1; extra > 0 {
		first += " and " + strconv.Itoa(extra) + " more"
	}
	return "config reload rejected: " + first + stillActiveSuffix
}

// recordMissing keeps the snapshot and limits warnings and notifications to one
// per interval, since reconciliation sees the absence every second.
func (m *Manager) recordMissing(ctx context.Context, now time.Time) {
	diagnostic := Diagnostic{Code: CodeConfigMissing, Message: "configuration file not found"}
	current := m.state.Load()
	status := current.status
	status.LastAttemptAt = now
	status.Result = ResultRejected
	status.Errors = []Diagnostic{diagnostic}
	m.state.Store(&publishedState{snapshot: current.snapshot, status: status})

	if !m.lastMissingWarn.IsZero() && now.Sub(m.lastMissingWarn) < missingWarnInterval {
		return
	}
	m.lastMissingWarn = now
	m.logger.Warn("config file missing"+stillActiveSuffix, "code", CodeConfigMissing, "generation", status.ActiveGeneration)
	m.notify(ctx, Event{
		Type: EventConfigReloadFailed, Generation: status.ActiveGeneration, At: now, Errors: []Diagnostic{diagnostic},
	})
}

// reconcile compares the file with the last observation. It reports a difference
// instead of reloading, so that both detection paths share the debounce. An
// identical file never triggers a reload, which is why secret files are reread
// only on a save event.
func (m *Manager) reconcile(ctx context.Context) bool {
	m.mu.Lock()
	defer m.mu.Unlock()

	data, err := readLimited(m.opts.fileSystem(), m.path, MaxConfigBytes+1)
	current := observe(data, err)
	if current.kind == observedMissing {
		m.observed = current
		m.recordMissing(ctx, m.clock.Now().UTC())
		return false
	}
	return current != m.observed
}

func (m *Manager) notify(ctx context.Context, event Event) {
	if m.sink == nil {
		return
	}
	defer func() {
		if recover() != nil {
			m.logger.Error("config notification sink panicked", "event", event.Type)
		}
	}()
	// The sink's error text is not logged because its origin is unknown.
	if err := m.sink.Publish(ctx, event); err != nil {
		m.logger.Error("config notification sink failed", "event", event.Type)
	}
}

// staticChanges compares normalized effective values, so an equivalent spelling of
// the same address or directory is not a change.
func staticChanges(current *Config, candidate *Candidate) []Diagnostic {
	next := &candidate.cfg
	type field struct {
		path    string
		changed bool
	}
	fields := []field{
		{"data_dir", current.DataDir != next.DataDir},
		{"listeners.admin.bind", current.Listeners.Admin.Bind != next.Listeners.Admin.Bind},
		{"listeners.admin.port", current.Listeners.Admin.Port != next.Listeners.Admin.Port},
		{"listeners.proxy.bind", current.Listeners.Proxy.Bind != next.Listeners.Proxy.Bind},
		{"listeners.proxy.port", current.Listeners.Proxy.Port != next.Listeners.Proxy.Port},
	}
	var out []Diagnostic
	for _, f := range fields {
		if f.changed {
			position := candidate.positions[f.path]
			out = append(out, Diagnostic{
				Code: CodeRestartRequired, Path: f.path, Line: position.Line, Column: position.Column,
				Message: "change requires restart" + stillActiveSuffix,
			})
		}
	}
	return out
}

// Run watches the configuration until ctx is canceled, and returns only after
// every timer and watcher it started has stopped.
func (m *Manager) Run(ctx context.Context) error {
	return newWatchLoop(m).run(ctx)
}
