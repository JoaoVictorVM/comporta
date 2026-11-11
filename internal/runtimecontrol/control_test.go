package runtimecontrol

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/storage/sqlite"
)

func openStore(t *testing.T) *sqlite.Store {
	t.Helper()
	store, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "private"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func loaded(t *testing.T, store storage.PauseStore) *Control {
	t.Helper()
	control := New(store)
	if err := control.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	return control
}

var at = time.Date(2026, 11, 8, 6, 12, 44, 0, time.UTC)

func apply(t *testing.T, control *Control, change storage.PauseChange) PauseResult {
	t.Helper()
	change.At = at
	result, err := control.Apply(context.Background(), change)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

// gatedStore lets a test hold a pause between its commit and its publication.
type gatedStore struct {
	storage.PauseStore
	committed chan struct{}
	proceed   chan struct{}
	fail      error
}

func (g *gatedStore) SavePause(ctx context.Context, change storage.PauseChange) (storage.PauseState, bool, error) {
	if g.fail != nil {
		return storage.PauseState{}, false, g.fail
	}
	state, changed, err := g.PauseStore.SavePause(ctx, change)
	if g.committed != nil {
		close(g.committed)
		<-g.proceed
	}
	return state, changed, err
}

func TestUnloadedControlFailsClosed(t *testing.T) {
	control := New(openStore(t))
	if _, err := control.Admit(Subject{App: "bot"}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("admission before loading: %v", err)
	}
}

func TestPauseAdmissionLinearization(t *testing.T) {
	gate := &gatedStore{PauseStore: openStore(t), committed: make(chan struct{}), proceed: make(chan struct{})}
	control := loaded(t, gate)
	held, err := control.Admit(Subject{App: "bot", Hash: "11111111111111111111111111111111"})
	if err != nil {
		t.Fatal(err)
	}
	acknowledged := make(chan struct{})
	go func() {
		defer close(acknowledged)
		apply(t, control, storage.PauseChange{App: "bot", Paused: true})
	}()
	<-gate.committed
	// Before the acknowledgement the projection may still admit: the operator
	// has not been told the app is paused yet.
	if ticket, err := control.Admit(Subject{App: "bot"}); err != nil {
		t.Fatalf("an admission before the acknowledgement: %v", err)
	} else {
		ticket.Release()
	}
	close(gate.proceed)
	<-acknowledged
	var wg sync.WaitGroup
	refused := make(chan error, 64)
	for range 64 {
		wg.Go(func() {
			_, err := control.Admit(Subject{App: "bot"})
			refused <- err
		})
	}
	wg.Wait()
	close(refused)
	for err := range refused {
		if !errors.Is(err, ErrPaused) {
			t.Fatalf("an admission passed after the acknowledgement: %v", err)
		}
	}
	if _, err := control.Admit(Subject{App: "other"}); err != nil {
		t.Fatalf("another app must keep working: %v", err)
	}
	// What was admitted before keeps running and ends normally.
	held.Release()
	held.Release()
}

func TestGlobalPauseCoversReloadedApp(t *testing.T) {
	control := loaded(t, openStore(t))
	apply(t, control, storage.PauseChange{All: true, Paused: true})
	if _, err := control.Admit(Subject{App: "added-by-reload"}); !errors.Is(err, ErrPaused) {
		t.Fatalf("an app added after pause --all: %v", err)
	}
}

func TestResumeAllClearsOverrides(t *testing.T) {
	store := openStore(t)
	control := loaded(t, store)
	apply(t, control, storage.PauseChange{App: "bot", Paused: true})
	apply(t, control, storage.PauseChange{App: "removed-app", Paused: true})
	apply(t, control, storage.PauseChange{All: true, Paused: true})
	result := apply(t, control, storage.PauseChange{All: true, Paused: false})
	if !result.Changed || result.State.Global || len(result.State.Apps) != 0 {
		t.Fatalf("resume --all left %+v", result.State)
	}
	for _, app := range []string{"bot", "removed-app", "other"} {
		if _, err := control.Admit(Subject{App: app}); err != nil {
			t.Fatalf("%s after resume --all: %v", app, err)
		}
	}
	if again := apply(t, control, storage.PauseChange{All: true, Paused: false}); again.Changed {
		t.Fatal("a repeated resume --all reported a change")
	}
	restarted := loaded(t, store)
	if state := restarted.State(); state.Global || len(state.Apps) != 0 {
		t.Fatalf("persisted state after resume --all: %+v", state)
	}
}

func TestResumeAppKeepsGlobalPause(t *testing.T) {
	control := loaded(t, openStore(t))
	apply(t, control, storage.PauseChange{App: "bot", Paused: true})
	apply(t, control, storage.PauseChange{All: true, Paused: true})
	result := apply(t, control, storage.PauseChange{App: "bot", Paused: false})
	if !result.Changed || !result.State.Global {
		t.Fatalf("resume of one app: %+v", result)
	}
	if _, err := control.Admit(Subject{App: "bot"}); !errors.Is(err, ErrPaused) {
		t.Fatal("the global pause must keep the app paused")
	}
}

func TestPauseRollbackKeepsMemory(t *testing.T) {
	gate := &gatedStore{PauseStore: openStore(t)}
	control := loaded(t, gate)
	apply(t, control, storage.PauseChange{App: "bot", Paused: true})
	gate.fail = storage.ErrUnavailable
	if _, err := control.Apply(context.Background(), storage.PauseChange{App: "bot", Paused: false, At: at}); err == nil {
		t.Fatal("a failed commit was acknowledged")
	}
	if _, err := control.Admit(Subject{App: "bot"}); !errors.Is(err, ErrPaused) {
		t.Fatal("a failed resume changed the projection")
	}
	if _, err := control.Apply(context.Background(), storage.PauseChange{App: "other", Paused: true, At: at}); err == nil {
		t.Fatal("a failed pause was acknowledged")
	}
	if _, err := control.Admit(Subject{App: "other"}); err != nil {
		t.Fatal("a failed pause changed the projection")
	}
}
