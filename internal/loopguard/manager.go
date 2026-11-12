package loopguard

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
)

// Internal ceilings. They are not configurable in YAML, so a hostile or mistaken
// value can never size an allocation.
const (
	DefaultMaxSubjects    = 100_000
	DefaultMaxOccurrences = 1_000_000
)

// SnapshotSource provides the configuration in force.
type SnapshotSource interface {
	Capture() *config.Snapshot
}

// Options configures a Manager.
type Options struct {
	Clock  Clock
	Source SnapshotSource
	Logger *slog.Logger
	// MaxSubjects and MaxOccurrences replace the internal ceilings in tests.
	MaxSubjects    int
	MaxOccurrences int64
}

type appState struct {
	generation int64
	policy     Policy
	// epoch changes when the detection thresholds change, which discards the
	// history counted under the previous ones on the next evaluation.
	epoch   uint64
	removed bool
}

type subjectState struct {
	mu sync.Mutex
	// refs is guarded by Manager.mu. A subject is removed from the map only
	// with no reference, so two live instances never split its lock.
	refs int
	dead atomic.Bool
	// The fields below are guarded by mu.
	held          int64
	epoch         uint64
	similarWindow time.Duration
	volume        arrivals
	sigs          signatures
	episode       *Episode
	blockedUntil  time.Duration
}

func (s *subjectState) empty() bool {
	return s.episode == nil && s.volume.len() == 0 && s.sigs.len() == 0
}

// purge drops expired history and an ended episode. It returns the occurrences
// freed.
func (s *subjectState) purge(now time.Duration) int64 {
	if s.episode != nil && now >= s.blockedUntil {
		s.episode = nil
	}
	freed := int64(s.volume.purge(now-RateWindow) + s.sigs.purge(now-s.similarWindow))
	s.held -= freed
	return freed
}

func (s *subjectState) reset() int64 {
	freed := int64(s.volume.reset() + s.sigs.reset())
	s.held -= freed
	return freed
}

// nextExpiry is the earliest instant at which purge would change something.
func (s *subjectState) nextExpiry() (time.Duration, bool) {
	var next time.Duration
	found := false
	consider := func(at time.Duration) {
		if !found || at < next {
			next, found = at, true
		}
	}
	if s.episode != nil {
		consider(s.blockedUntil)
	}
	if at, ok := s.volume.oldest(); ok {
		consider(at + RateWindow)
	}
	if at, ok := s.sigs.oldest(); ok {
		consider(at + s.similarWindow)
	}
	return next, found
}

// Manager evaluates arrivals atomically per subject. Subjects are evaluated in
// parallel; no I/O ever happens under its locks.
type Manager struct {
	clock          Clock
	source         SnapshotSource
	logger         *slog.Logger
	maxSubjects    int
	maxOccurrences int64
	occurrences    atomic.Int64

	mu       sync.Mutex
	apps     map[string]*appState
	subjects map[Subject]*subjectState
	// sweepAt is the deadline the worker is armed for.
	sweepAt  time.Duration
	sweeping bool
	wake     chan struct{}
}

// New returns a Manager. Its worker must run for expired state to be released.
func New(options Options) *Manager {
	m := &Manager{
		clock: options.Clock, source: options.Source, logger: options.Logger,
		maxSubjects: options.MaxSubjects, maxOccurrences: options.MaxOccurrences,
		apps: map[string]*appState{}, subjects: map[Subject]*subjectState{}, wake: make(chan struct{}, 1),
	}
	if m.clock == nil {
		m.clock = SystemClock()
	}
	if m.logger == nil {
		m.logger = slog.New(slog.DiscardHandler)
	}
	if m.maxSubjects <= 0 {
		m.maxSubjects = DefaultMaxSubjects
	}
	if m.maxOccurrences <= 0 {
		m.maxOccurrences = DefaultMaxOccurrences
	}
	return m
}

// Check evaluates one arrival and, when allowed, counts it. A cut starts a
// fixed cooldown; retries during it get the same episode and never extend it.
func (m *Manager) Check(req Request) (Decision, error) {
	if req.Subject.App == "" || req.Subject.Hash == "" || !req.Policy.valid() {
		return Decision{}, ErrInvalid
	}
	for {
		state, epoch, err := m.acquire(req)
		if err != nil {
			return Decision{}, err
		}
		if state == nil {
			return Decision{Allowed: true}, nil
		}
		state.mu.Lock()
		if state.dead.Load() {
			m.release(state)
			continue
		}
		decision, err := m.evaluate(state, epoch, req)
		m.release(state)
		return decision, err
	}
}

// acquire applies the policy and pins the subject. A nil state means the guard
// is disabled for the app.
func (m *Manager) acquire(req Request) (*subjectState, uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	app, err := m.applyLocked(req.Subject.App, req.Policy)
	if err != nil {
		return nil, 0, err
	}
	if !req.Policy.Enabled {
		return nil, 0, nil
	}
	state := m.subjects[req.Subject]
	if state == nil {
		if len(m.subjects) >= m.maxSubjects {
			m.sweepLocked(m.clock.Elapsed())
			if len(m.subjects) >= m.maxSubjects {
				return nil, 0, ErrCapacity
			}
		}
		state = &subjectState{epoch: app.epoch, similarWindow: req.Policy.SimilarWindow}
		m.subjects[req.Subject] = state
	}
	state.refs++
	return state, app.epoch, nil
}

// release must be called with state.mu held; it unlocks it.
func (m *Manager) release(state *subjectState) {
	m.mu.Lock()
	state.refs--
	if state.refs == 0 && state.dead.Load() {
		m.occurrences.Add(-state.held)
		state.held = 0
	}
	m.mu.Unlock()
	state.mu.Unlock()
}

func (m *Manager) evaluate(state *subjectState, epoch uint64, req Request) (Decision, error) {
	policy := req.Policy
	now := m.clock.Elapsed()
	if state.epoch != epoch || state.similarWindow != policy.SimilarWindow {
		m.occurrences.Add(-state.reset())
		state.epoch, state.similarWindow = epoch, policy.SimilarWindow
	}
	if state.episode != nil && now < state.blockedUntil {
		return Decision{Episode: *state.episode, Remaining: state.blockedUntil - now}, nil
	}
	m.occurrences.Add(-state.purge(now))

	var episode *Episode
	switch {
	case req.HasSignature && state.sigs.count(req.Signature) >= policy.SimilarRequests:
		episode = &Episode{Reason: ReasonSimilar, Count: policy.SimilarRequests + 1, Limit: policy.SimilarRequests, Window: policy.SimilarWindow}
	case int64(state.volume.len()) >= policy.RequestsPerMinute:
		episode = &Episode{Reason: ReasonRate, Count: policy.RequestsPerMinute + 1, Limit: policy.RequestsPerMinute, Window: RateWindow}
	}
	if episode != nil {
		episode.EventID, episode.At, episode.RequestID, episode.Cooldown = newEventID(), m.clock.Now(), req.RequestID, policy.Cooldown
		m.occurrences.Add(-state.reset())
		state.episode, state.blockedUntil = episode, now+policy.Cooldown
		m.schedule(state.blockedUntil)
		return Decision{First: true, Episode: *episode, Remaining: policy.Cooldown}, nil
	}

	need := int64(1)
	if req.HasSignature {
		need++
	}
	if !m.reserve(need) {
		m.mu.Lock()
		m.sweepLocked(now)
		m.mu.Unlock()
		if !m.reserve(need) {
			return Decision{}, ErrCapacity
		}
	}
	state.volume.push(now)
	if req.HasSignature {
		state.sigs.push(now, req.Signature)
	}
	state.held += need
	m.schedule(now + min(RateWindow, policy.SimilarWindow))
	return Decision{Allowed: true}, nil
}

func (m *Manager) reserve(n int64) bool {
	for {
		current := m.occurrences.Load()
		if current+n > m.maxOccurrences {
			return false
		}
		if m.occurrences.CompareAndSwap(current, current+n) {
			return true
		}
	}
}

// schedule wakes the worker when a deadline earlier than the armed one appears.
// Later deadlines are found by the sweep itself, so steady traffic does not
// wake it on every request.
func (m *Manager) schedule(at time.Duration) {
	m.mu.Lock()
	earlier := !m.sweeping || at < m.sweepAt
	if earlier {
		m.sweepAt, m.sweeping = at, true
	}
	m.mu.Unlock()
	if earlier {
		select {
		case m.wake <- struct{}{}:
		default:
		}
	}
}

// applyLocked records the policy of an app, rejecting one older than what was
// already applied. A newer policy that disables the app drops all its state;
// one with other thresholds discards the history but keeps active cooldowns.
func (m *Manager) applyLocked(name string, policy Policy) (*appState, error) {
	app := m.apps[name]
	if app != nil && policy.Generation < app.generation {
		return nil, ErrStale
	}
	if app != nil && policy.Generation == app.generation {
		if app.removed {
			return nil, ErrStale
		}
		return app, nil
	}
	if app == nil {
		app = &appState{}
		m.apps[name] = app
	} else if app.policy.Enabled && policy.Enabled && !app.policy.sameDetection(policy) {
		app.epoch++
	}
	if !policy.Enabled {
		m.dropAppLocked(name)
	}
	app.generation, app.policy, app.removed = policy.Generation, policy, false
	return app, nil
}

// removeLocked forgets one subject. A subject in use is marked dead: its holder
// re-evaluates on a fresh instance and the last reference frees its memory.
func (m *Manager) removeLocked(subject Subject, state *subjectState) {
	delete(m.subjects, subject)
	state.dead.Store(true)
	if state.refs > 0 {
		return
	}
	state.mu.Lock()
	m.occurrences.Add(-state.held)
	state.reset()
	state.held, state.episode = 0, nil
	state.mu.Unlock()
}

func (m *Manager) dropAppLocked(app string) {
	for subject, state := range m.subjects {
		if subject.App == app {
			m.removeLocked(subject, state)
		}
	}
}

// sweepLocked releases expired history and idle subjects, and returns the next
// deadline. Subjects in use are skipped; their holder keeps them consistent.
func (m *Manager) sweepLocked(now time.Duration) (time.Duration, bool) {
	var next time.Duration
	found := false
	for subject, state := range m.subjects {
		if state.refs > 0 {
			continue
		}
		state.mu.Lock()
		m.occurrences.Add(-state.purge(now))
		if state.empty() {
			state.mu.Unlock()
			delete(m.subjects, subject)
			state.dead.Store(true)
			continue
		}
		if at, ok := state.nextExpiry(); ok && (!found || at < next) {
			next, found = at, true
		}
		state.mu.Unlock()
	}
	return next, found
}

func newEventID() string {
	var id [16]byte
	_, _ = rand.Read(id[:])
	return hex.EncodeToString(id[:])
}
