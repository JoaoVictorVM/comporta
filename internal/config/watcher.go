package config

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
)

const (
	debounceQuiet     = 500 * time.Millisecond
	debounceMaxWait   = 1 * time.Second
	reconcileInterval = 1 * time.Second
)

// fsWatcher is the part of fsnotify the loop uses.
type fsWatcher interface {
	Add(path string) error
	Remove(path string) error
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
func (a *fsnotifyAdapter) Remove(path string) error      { return a.watcher.Remove(path) }
func (a *fsnotifyAdapter) Close() error                  { return a.watcher.Close() }
func (a *fsnotifyAdapter) Events() <-chan fsnotify.Event { return a.watcher.Events }
func (a *fsnotifyAdapter) Errors() <-chan error          { return a.watcher.Errors }

// watchLoop observes the directory rather than the file because editors that save
// atomically replace the file, which would end a watch placed on it. Periodic
// reconciliation recovers events the operating system dropped.
type watchLoop struct {
	manager *Manager
	dir     string

	watcher  fsWatcher
	watching bool

	pending    bool
	firstEvent time.Time
	lastEvent  time.Time
}

func newWatchLoop(manager *Manager) *watchLoop {
	return &watchLoop{manager: manager, dir: filepath.Dir(manager.path)}
}

func (w *watchLoop) run(ctx context.Context) error {
	defer w.closeWatcher()
	w.ensureWatching()

	ticker := time.NewTicker(reconcileInterval)
	defer ticker.Stop()
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()

	for {
		var fire <-chan time.Time
		if w.pending {
			timer.Reset(max(w.deadline().Sub(w.manager.clock.Now()), 0))
			fire = timer.C
		}
		var events <-chan fsnotify.Event
		var watchErrors <-chan error
		if w.watcher != nil {
			events, watchErrors = w.watcher.Events(), w.watcher.Errors()
		}

		select {
		case <-ctx.Done():
			return nil
		case event, ok := <-events:
			if !ok {
				w.closeWatcher()
				continue
			}
			w.onEvent(event)
		case err, ok := <-watchErrors:
			if !ok {
				w.closeWatcher()
				continue
			}
			w.onWatchError(err)
		case <-ticker.C:
			w.onTick(ctx)
		case <-fire:
			w.onDeadline(ctx)
		}
	}
}

// mark records activity. Each event pushes the attempt back by the quiet period,
// but a sequence that never settles is still attempted at the maximum wait.
func (w *watchLoop) mark(now time.Time) {
	if !w.pending {
		w.pending = true
		w.firstEvent = now
	}
	w.lastEvent = now
}

func (w *watchLoop) deadline() time.Time {
	quiet := w.lastEvent.Add(debounceQuiet)
	if limit := w.firstEvent.Add(debounceMaxWait); limit.Before(quiet) {
		return limit
	}
	return quiet
}

func (w *watchLoop) onEvent(event fsnotify.Event) {
	if w.isDirectory(event.Name) && event.Op&(fsnotify.Remove|fsnotify.Rename) != 0 {
		w.watching = false
		return
	}
	const relevant = fsnotify.Create | fsnotify.Write | fsnotify.Rename | fsnotify.Remove
	if event.Op&relevant != 0 && w.isConfigFile(event.Name) {
		w.mark(w.manager.clock.Now())
	}
}

func (w *watchLoop) isConfigFile(name string) bool {
	return samePath(filepath.Clean(name), w.manager.path)
}

func (w *watchLoop) isDirectory(name string) bool {
	return samePath(filepath.Clean(name), w.dir)
}

func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// onWatchError treats any watcher failure, including an event overflow, as a
// possible lost change: it forces a new attempt and a fresh observation of the
// directory, while the active configuration stays untouched.
func (w *watchLoop) onWatchError(err error) {
	reason := "watcher_error"
	if errors.Is(err, fsnotify.ErrEventOverflow) {
		reason = "event_overflow"
	}
	w.manager.logger.Warn("config watcher recovering", "reason", reason)
	w.mark(w.manager.clock.Now())
	if w.watcher != nil {
		_ = w.watcher.Remove(w.dir)
	}
	w.watching = false
	w.ensureWatching()
}

func (w *watchLoop) onTick(ctx context.Context) {
	if !w.watching {
		w.ensureWatching()
	}
	if w.manager.reconcile(ctx) {
		w.mark(w.manager.clock.Now())
	}
}

func (w *watchLoop) onDeadline(ctx context.Context) {
	w.pending = false
	w.manager.Reload(ctx)
}

// ensureWatching may fail while the directory is absent. Reconciliation keeps
// working in the meantime and the next tick tries again.
func (w *watchLoop) ensureWatching() {
	if w.watcher == nil {
		watcher, err := w.manager.newWatcher()
		if err != nil {
			return
		}
		w.watcher = watcher
	}
	w.watching = w.watcher.Add(w.dir) == nil
}

func (w *watchLoop) closeWatcher() {
	if w.watcher != nil {
		_ = w.watcher.Close()
		w.watcher = nil
	}
	w.watching = false
}
