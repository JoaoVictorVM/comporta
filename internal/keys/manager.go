package keys

import (
	"context"
	"crypto/sha256"
	"errors"
	"iter"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	debounceQuiet       = 100 * time.Millisecond
	debounceMaxWait     = 500 * time.Millisecond
	reconcileInterval   = 1 * time.Second
	unavailableLogEvery = 30 * time.Second
)

// Entry is one stored key as authentication needs it.
type Entry struct {
	Digest   Digest
	Identity Identity
	Revoked  bool
}

// Generation is one immutable state of the store, captured once per operation so
// that a concurrent change cannot alter a decision halfway through. Its number is
// local to this process and unrelated to the revision in the document.
type Generation struct {
	number    int64
	available bool
	entries   []Entry
}

// Number is an increasing identifier local to this process.
func (g *Generation) Number() int64 { return g.number }

// Available reports whether the store could be read and validated. An unavailable
// generation must not authorize anything, whatever the configured fail mode is.
func (g *Generation) Available() bool { return g.available }

// Entries yields copies of the stored keys, revoked ones included.
func (g *Generation) Entries() iter.Seq[Entry] {
	return func(yield func(Entry) bool) {
		for _, entry := range g.entries {
			if !yield(entry) {
				return
			}
		}
	}
}

func newGeneration(number int64, doc *document) (*Generation, error) {
	entries := make([]Entry, 0, len(doc.keys))
	for i := range doc.keys {
		r := &doc.keys[i]
		digest, err := decodeDigest(r.sha256)
		if err != nil {
			return nil, err
		}
		entries = append(entries, Entry{
			Digest:   digest,
			Identity: Identity{appName: r.app, keyID: r.id},
			Revoked:  !r.active(),
		})
	}
	return &Generation{number: number, available: true, entries: entries}, nil
}

// fsWatcher is the part of fsnotify the loop uses.
type fsWatcher interface {
	Add(path string) error
	Close() error
	Events() <-chan fsnotify.Event
	Errors() <-chan error
}

type fsnotifyAdapter struct {
	watcher *fsnotify.Watcher
}

func newFSNotifyWatcher() (fsWatcher, error) {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, err
	}
	return &fsnotifyAdapter{watcher: watcher}, nil
}

func (a *fsnotifyAdapter) Add(path string) error         { return a.watcher.Add(path) }
func (a *fsnotifyAdapter) Close() error                  { return a.watcher.Close() }
func (a *fsnotifyAdapter) Events() <-chan fsnotify.Event { return a.watcher.Events }
func (a *fsnotifyAdapter) Errors() <-chan error          { return a.watcher.Errors }

// ManagerOptions configures a Manager. Only Dir is required.
type ManagerOptions struct {
	Dir    string
	Logger *slog.Logger
	Clock  Clock

	newWatcher func() (fsWatcher, error)
}

// Manager publishes the store for authentication. Readers never block: they load
// an immutable generation atomically.
type Manager struct {
	store      *Store
	logger     *slog.Logger
	clock      Clock
	newWatcher func() (fsWatcher, error)
	usage      *usageTracker

	state atomic.Pointer[Generation]

	mu        sync.Mutex
	number    int64
	observed  [sha256.Size]byte
	lastError string
	lastLog   time.Time
}

// NewManager performs the initial read. The store must already exist: a service
// that recreated it could silently run with an empty set after losing the file.
func NewManager(options ManagerOptions) (*Manager, error) {
	m := &Manager{
		store:      NewStore(options.Dir),
		logger:     options.Logger,
		clock:      options.Clock,
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
	m.store.clock = m.clock
	m.usage = newUsageTracker(m.store, m.clock, m.logger)

	data, err := m.store.readRaw()
	if err != nil {
		return nil, err
	}
	doc, err := parseDocument(data)
	if err != nil {
		return nil, err
	}
	m.number = 1
	generation, err := newGeneration(m.number, doc)
	if err != nil {
		return nil, err
	}
	m.observed = sha256.Sum256(data)
	m.state.Store(generation)
	return m, nil
}

// Capture returns the current generation. Hold it for the whole operation.
func (m *Manager) Capture() *Generation { return m.state.Load() }

// RecordUse notes a successful authentication. It only touches memory.
func (m *Manager) RecordUse(keyID string, at time.Time) { m.usage.record(keyID, at) }

// Run observes the store until the context ends, then saves pending usage times
// for at most five seconds. It always returns nil today; the signature matches the
// configuration manager so that both are supervised the same way.
func (m *Manager) Run(ctx context.Context) error {
	var group sync.WaitGroup
	group.Go(func() { m.usage.run(ctx) })
	group.Go(func() { m.watch(ctx) })
	group.Wait()
	return nil
}

// reconcile rereads the file and publishes when the outcome changed. Comparing
// content catches edits that keep the size, modification time or revision.
func (m *Manager) reconcile() {
	m.mu.Lock()
	defer m.mu.Unlock()

	data, err := m.store.readRaw()
	var doc *document
	if err == nil {
		doc, err = parseDocument(data)
	}
	if err != nil {
		m.publishUnavailable(err)
		return
	}
	sum := sha256.Sum256(data)
	current := m.state.Load()
	if current.available && sum == m.observed {
		return
	}
	m.number++
	generation, err := newGeneration(m.number, doc)
	if err != nil {
		m.publishUnavailable(err)
		return
	}
	m.observed = sum
	m.state.Store(generation)
	if !current.available {
		m.lastError = ""
		m.logger.Info("key store recovered", "generation", generation.number)
		return
	}
	m.logger.Debug("key store reloaded", "generation", generation.number)
}

// publishUnavailable blocks authentication rather than keep trusting the previous
// state: a revocation might be exactly what can no longer be read.
func (m *Manager) publishUnavailable(err error) {
	reason := unavailableReason(err)
	now := m.clock.Now()
	if m.state.Load().available {
		m.number++
		m.state.Store(&Generation{number: m.number})
	}
	if reason != m.lastError || now.Sub(m.lastLog) >= unavailableLogEvery {
		m.logger.Error("key store unavailable; authentication is blocked until it recovers", "reason", reason)
		m.lastError, m.lastLog = reason, now
	}
}

func unavailableReason(err error) string {
	switch {
	case errors.Is(err, ErrStoreMissing):
		return "missing"
	case errors.Is(err, ErrStoreCorrupt):
		return "corrupt"
	case errors.Is(err, ErrStoreInsecure):
		return "insecure"
	}
	return "io_error"
}

// watch observes the directory because a commit replaces the file, which would end
// a watch placed on the file itself. Reconciliation runs regardless, so a failing
// watcher delays nothing beyond one tick and the deadline holds without events.
func (m *Manager) watch(ctx context.Context) {
	dir := m.store.dir
	var watcher fsWatcher
	watching := false
	closeWatcher := func() {
		if watcher != nil {
			_ = watcher.Close()
			watcher = nil
		}
		watching = false
	}
	defer closeWatcher()
	ensureWatching := func() {
		if watcher == nil {
			created, err := m.newWatcher()
			if err != nil {
				return
			}
			watcher = created
		}
		watching = watcher.Add(dir) == nil
	}
	ensureWatching()

	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()

	pending := false
	var first, last time.Time
	for {
		var fire <-chan time.Time
		if pending {
			timer.Reset(max(debounceDeadline(first, last).Sub(time.Now()), 0))
			fire = timer.C
		}
		var events <-chan fsnotify.Event
		var watchErrors <-chan error
		if watcher != nil {
			events, watchErrors = watcher.Events(), watcher.Errors()
		}

		select {
		case <-ctx.Done():
			return
		case event, ok := <-events:
			if !ok {
				closeWatcher()
				continue
			}
			if isDocumentEvent(event) {
				now := time.Now()
				if !pending {
					pending, first = true, now
				}
				last = now
			}
		case _, ok := <-watchErrors:
			closeWatcher()
			if ok {
				m.logger.Warn("key store watcher recovering")
			}
		case <-ticker.C:
			if !watching {
				ensureWatching()
			}
			m.reconcile()
		case <-fire:
			pending = false
			m.reconcile()
		}
	}
}

func debounceDeadline(first, last time.Time) time.Time {
	quiet := last.Add(debounceQuiet)
	if limit := first.Add(debounceMaxWait); limit.Before(quiet) {
		return limit
	}
	return quiet
}

func isDocumentEvent(event fsnotify.Event) bool {
	const relevant = fsnotify.Create | fsnotify.Write | fsnotify.Rename | fsnotify.Remove
	return event.Op&relevant != 0 && strings.EqualFold(filepath.Base(event.Name), documentFile)
}
