package keys

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func newTestTracker(t *testing.T, store *Store) (*usageTracker, *bytes.Buffer) {
	t.Helper()
	var logs bytes.Buffer
	return newUsageTracker(store, store.clock, slog.New(slog.NewJSONHandler(&logs, nil))), &logs
}

func lastUsed(t *testing.T, store *Store, id string) time.Time {
	t.Helper()
	entries, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.ID == id {
			return entry.LastUsedAt
		}
	}
	t.Fatalf("key %s not listed", id)
	return time.Time{}
}

func TestLastUsedMergesConcurrentUpdates(t *testing.T) {
	store := newTestStore(t)
	created := mustCreate(t, store, "bot")
	id := created.Metadata.ID
	base := created.Metadata.CreatedAt
	tracker, _ := newTestTracker(t, store)

	tracker.record(id, base.Add(2*time.Hour))
	tracker.record(id, base.Add(time.Hour))
	if err := tracker.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := lastUsed(t, store, id); !got.Equal(base.Add(2 * time.Hour)) {
		t.Fatalf("the latest time must win, got %v", got)
	}

	other, _ := newTestTracker(t, store)
	other.record(id, base.Add(time.Minute))
	if err := other.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := lastUsed(t, store, id); !got.Equal(base.Add(2 * time.Hour)) {
		t.Fatalf("an older time from another process must not win, got %v", got)
	}

	second := mustCreate(t, store, "bot")
	tracker.record(id, base.Add(3*time.Hour))
	tracker.record(second.Metadata.ID, second.Metadata.CreatedAt.Add(time.Hour))
	if err := tracker.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(tracker.pending) != 0 {
		t.Fatalf("confirmed entries must leave: %v", tracker.pending)
	}
}

func TestNewerUseDuringFlushStaysPending(t *testing.T) {
	store := newTestStore(t)
	created := mustCreate(t, store, "bot")
	id := created.Metadata.ID
	base := created.Metadata.CreatedAt
	tracker, _ := newTestTracker(t, store)

	tracker.record(id, base.Add(time.Hour))
	captured := tracker.capture()
	tracker.record(id, base.Add(2*time.Hour))
	tracker.confirm(captured)
	if at, ok := tracker.pending[id]; !ok || !at.Equal(base.Add(2*time.Hour)) {
		t.Fatalf("a concurrent success must not be discarded: %v", tracker.pending)
	}
}

func TestLastUsedFlushNeverResurrectsRevokedKey(t *testing.T) {
	store := newTestStore(t)
	created := mustCreate(t, store, "bot")
	id := created.Metadata.ID
	if _, err := store.Revoke(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, store.documentPath())

	tracker, _ := newTestTracker(t, store)
	tracker.record(id, created.Metadata.CreatedAt.Add(time.Hour))
	tracker.record("k_ffffffffffffffffffffffffffffffff", created.Metadata.CreatedAt.Add(time.Hour))
	if err := tracker.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if after := readFile(t, store.documentPath()); after != before {
		t.Fatal("nothing may change for a revoked or unknown key")
	}
	entries, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Active() {
		t.Fatalf("entries %+v", entries)
	}
	if len(tracker.pending) != 0 {
		t.Fatal("entries with nothing to do are confirmed")
	}
}

func TestLastUsedRetriesWithoutFailingAuthentication(t *testing.T) {
	store := newTestStore(t)
	created := mustCreate(t, store, "bot")
	id := created.Metadata.ID
	at := created.Metadata.CreatedAt.Add(time.Hour)
	tracker, logs := newTestTracker(t, store)
	tracker.record(id, at)

	working := store.ops.replace
	store.ops.replace = func(string, string) error { return errors.New("injected") }
	for range 3 {
		if err := tracker.flush(context.Background()); err == nil {
			t.Fatal("the failure must be reported to the caller")
		}
	}
	if _, ok := tracker.pending[id]; !ok {
		t.Fatal("the entry must stay pending")
	}
	if got := strings.Count(logs.String(), "could not be saved"); got != 1 {
		t.Fatalf("logs must be rate limited, got %d: %s", got, logs)
	}

	store.ops.replace = working
	if err := tracker.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := lastUsed(t, store, id); !got.Equal(at) {
		t.Fatalf("retry lost the time: %v", got)
	}
}

func TestLastUsedClampedToCreation(t *testing.T) {
	store := newTestStore(t)
	created := mustCreate(t, store, "bot")
	tracker, _ := newTestTracker(t, store)
	tracker.record(created.Metadata.ID, created.Metadata.CreatedAt.Add(-time.Hour))
	if err := tracker.flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(); err != nil {
		t.Fatalf("a clock moved back must not corrupt the document: %v", err)
	}
	if got := lastUsed(t, store, created.Metadata.ID); !got.Equal(created.Metadata.CreatedAt) {
		t.Fatalf("last used %v", got)
	}
}

func TestUsageRunFlushesOnCancellation(t *testing.T) {
	store := newTestStore(t)
	created := mustCreate(t, store, "bot")
	tracker, _ := newTestTracker(t, store)
	at := created.Metadata.CreatedAt.Add(time.Hour)
	tracker.record(created.Metadata.ID, at)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { tracker.run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("run did not stop")
	}
	if got := lastUsed(t, store, created.Metadata.ID); !got.Equal(at) {
		t.Fatalf("the final flush lost the time: %v", got)
	}
}
