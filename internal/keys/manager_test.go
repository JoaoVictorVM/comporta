package keys

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

// silentWatcher delivers nothing, which is what a watcher that lost its events
// looks like to the loop. Only reconciliation can notice a change then.
type silentWatcher struct {
	events chan fsnotify.Event
	errors chan error
}

func newSilentWatcher() (fsWatcher, error) {
	return &silentWatcher{events: make(chan fsnotify.Event), errors: make(chan error)}, nil
}

func (s *silentWatcher) Add(string) error              { return nil }
func (s *silentWatcher) Close() error                  { return nil }
func (s *silentWatcher) Events() <-chan fsnotify.Event { return s.events }
func (s *silentWatcher) Errors() <-chan error          { return s.errors }

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func startManager(t *testing.T, store *Store, newWatcher func() (fsWatcher, error)) (*Manager, *syncBuffer) {
	t.Helper()
	logs := &syncBuffer{}
	manager, err := NewManager(ManagerOptions{
		Dir:        store.dir,
		Logger:     slog.New(slog.NewJSONHandler(logs, nil)),
		newWatcher: newWatcher,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- manager.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-stopped:
		case <-time.After(10 * time.Second):
			t.Error("manager did not stop")
		}
	})
	return manager, logs
}

func revokedInGeneration(generation *Generation, id string) bool {
	for entry := range generation.Entries() {
		if entry.Identity.KeyID() == id {
			return entry.Revoked
		}
	}
	return false
}

func waitFor(t *testing.T, limit time.Duration, what string, done func() bool) time.Duration {
	t.Helper()
	start := time.Now()
	for !done() {
		if time.Since(start) > limit {
			t.Fatalf("%s did not happen within %v", what, limit)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return time.Since(start)
}

func TestRevocationVisibleWithinTwoSeconds(t *testing.T) {
	store := newTestStore(t)
	created := mustCreate(t, store, "bot")
	manager, _ := startManager(t, store, nil)
	if revokedInGeneration(manager.Capture(), created.Metadata.ID) {
		t.Fatal("the key starts active")
	}
	if _, err := store.Revoke(context.Background(), created.Metadata.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "revocation", func() bool {
		return revokedInGeneration(manager.Capture(), created.Metadata.ID)
	})
}

func TestReconcileRecoversLostWatchEvent(t *testing.T) {
	store := newTestStore(t)
	created := mustCreate(t, store, "bot")
	manager, _ := startManager(t, store, newSilentWatcher)
	if _, err := store.Revoke(context.Background(), created.Metadata.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "revocation without events", func() bool {
		return revokedInGeneration(manager.Capture(), created.Metadata.ID)
	})
}

func TestWatcherCreationFailureDoesNotDisableReconcile(t *testing.T) {
	store := newTestStore(t)
	created := mustCreate(t, store, "bot")
	failing := func() (fsWatcher, error) { return nil, errors.New("injected") }
	manager, _ := startManager(t, store, failing)
	if _, err := store.Revoke(context.Background(), created.Metadata.ID); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 2*time.Second, "revocation with a broken watcher", func() bool {
		return revokedInGeneration(manager.Capture(), created.Metadata.ID)
	})
}

func TestAtomicSaveReloadsStore(t *testing.T) {
	store := newTestStore(t)
	created := mustCreate(t, store, "bot")
	manager, _ := startManager(t, store, nil)

	content := strings.Replace(readFile(t, store.documentPath()), `"revoked_at": null`, `"revoked_at": "2026-11-06T13:00:00Z"`, 1)
	writeRawDocument(t, store, content)
	waitFor(t, 2*time.Second, "reload after an external atomic save", func() bool {
		return revokedInGeneration(manager.Capture(), created.Metadata.ID)
	})
}

func TestSameRevisionEditIsDetected(t *testing.T) {
	store := newTestStore(t)
	created := mustCreate(t, store, "bot")
	manager, _ := startManager(t, store, newSilentWatcher)
	before := manager.Capture().Number()

	content := strings.Replace(readFile(t, store.documentPath()), `"revoked_at": null`, `"revoked_at": "2026-11-06T13:00:00Z"`, 1)
	writeRawDocument(t, store, content)
	waitFor(t, 2*time.Second, "edit that keeps the revision", func() bool {
		return revokedInGeneration(manager.Capture(), created.Metadata.ID)
	})
	if manager.Capture().Number() <= before {
		t.Fatal("generations must increase")
	}
}

func TestInvalidOrMissingStoreDisablesAuthentication(t *testing.T) {
	cases := map[string]func(t *testing.T, store *Store){
		"corrupt": func(t *testing.T, store *Store) { writeRawDocument(t, store, "{broken") },
		"empty":   func(t *testing.T, store *Store) { writeRawDocument(t, store, "") },
		"missing": func(t *testing.T, store *Store) {
			if err := os.Remove(store.documentPath()); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, breakStore := range cases {
		t.Run(name, func(t *testing.T) {
			store := newTestStore(t)
			mustCreate(t, store, "bot")
			manager, logs := startManager(t, store, nil)
			if !manager.Capture().Available() {
				t.Fatal("must start available")
			}
			breakStore(t, store)
			waitFor(t, 2*time.Second, "unavailable state", func() bool { return !manager.Capture().Available() })
			if !strings.Contains(logs.String(), "key store unavailable") {
				t.Fatalf("logs: %s", logs)
			}
			count := 0
			for range manager.Capture().Entries() {
				count++
			}
			if count != 0 {
				t.Fatal("an unavailable generation must hold no entries")
			}
		})
	}
}

func TestUnavailableLogsAreRateLimited(t *testing.T) {
	store := newTestStore(t)
	mustCreate(t, store, "bot")
	logs := &syncBuffer{}
	clock := newFakeClock()
	manager, err := NewManager(ManagerOptions{Dir: store.dir, Logger: slog.New(slog.NewJSONHandler(logs, nil)), Clock: clock})
	if err != nil {
		t.Fatal(err)
	}
	writeRawDocument(t, store, "{broken")
	for range 5 {
		manager.reconcile()
	}
	if got := strings.Count(logs.String(), "key store unavailable"); got != 1 {
		t.Fatalf("logged %d times: %s", got, logs)
	}
	clock.advance(30 * time.Second)
	manager.reconcile()
	if got := strings.Count(logs.String(), "key store unavailable"); got != 2 {
		t.Fatalf("logged %d times after the interval", got)
	}
}

func TestRecoveryRestoresAuthentication(t *testing.T) {
	store := newTestStore(t)
	created := mustCreate(t, store, "bot")
	good := readFile(t, store.documentPath())
	manager, logs := startManager(t, store, nil)

	writeRawDocument(t, store, "{broken")
	waitFor(t, 2*time.Second, "unavailable state", func() bool { return !manager.Capture().Available() })
	writeRawDocument(t, store, good)
	waitFor(t, 2*time.Second, "recovery", func() bool { return manager.Capture().Available() })

	found := false
	for entry := range manager.Capture().Entries() {
		found = found || entry.Identity.KeyID() == created.Metadata.ID
	}
	if !found {
		t.Fatal("the recovered generation must hold the key")
	}
	waitFor(t, time.Second, "recovery log", func() bool { return strings.Contains(logs.String(), "key store recovered") })
	if got := strings.Count(logs.String(), "key store recovered"); got != 1 {
		t.Fatalf("recovery must be logged once, got %d", got)
	}
}

func TestEmptyStoreNeverAllowsAnonymousAccess(t *testing.T) {
	store := newTestStore(t)
	if err := ensurePrivateDir(store.dir); err != nil {
		t.Fatal(err)
	}
	writeRawDocument(t, store, docJSON())
	manager, err := NewManager(ManagerOptions{Dir: store.dir})
	if err != nil {
		t.Fatal(err)
	}
	generation := manager.Capture()
	if !generation.Available() {
		t.Fatal("a valid empty document is available")
	}
	for range generation.Entries() {
		t.Fatal("it holds nothing to match")
	}
}

func TestManagerRequiresReadableStoreAtStartup(t *testing.T) {
	store := newTestStore(t)
	if _, err := NewManager(ManagerOptions{Dir: store.dir}); !errors.Is(err, ErrStoreMissing) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := os.Stat(store.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("startup must not create anything")
	}
	mustCreate(t, store, "bot")
	writeRawDocument(t, store, "{broken")
	if _, err := NewManager(ManagerOptions{Dir: store.dir}); !errors.Is(err, ErrStoreCorrupt) {
		t.Fatalf("corrupt: %v", err)
	}
}

func TestManagerFlushesUsageOnShutdown(t *testing.T) {
	store := newTestStore(t)
	created := mustCreate(t, store, "bot")
	manager, err := NewManager(ManagerOptions{Dir: store.dir, newWatcher: newSilentWatcher})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan error, 1)
	go func() { stopped <- manager.Run(ctx) }()
	at := created.Metadata.CreatedAt.Add(time.Hour)
	manager.RecordUse(created.Metadata.ID, at)
	cancel()
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("manager did not stop")
	}
	if got := lastUsed(t, store, created.Metadata.ID); !got.Equal(at) {
		t.Fatalf("last used %v", got)
	}
}

func TestDocumentEventFilter(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]bool{
		filepath.Join(dir, "keys.json"):          true,
		filepath.Join(dir, "keys.lock"):          false,
		filepath.Join(dir, ".keys-k_00.tmp"):     false,
		filepath.Join(dir, "other.json"):         false,
		filepath.Join(dir, "keys.json.bak"):      false,
		filepath.Join(dir, "nested", "file.txt"): false,
	}
	for name, want := range cases {
		if got := isDocumentEvent(fsnotify.Event{Name: name, Op: fsnotify.Create}); got != want {
			t.Fatalf("%s: got %v", name, got)
		}
	}
	if isDocumentEvent(fsnotify.Event{Name: filepath.Join(dir, "keys.json"), Op: fsnotify.Chmod}) {
		t.Fatal("a chmod is left to reconciliation")
	}
}
