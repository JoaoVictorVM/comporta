package metering

import (
	"context"
	"errors"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/proxy"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// gatedCommit blocks a batch inside Commit until released.
type gatedCommit struct {
	Store
	started chan struct{}
	release chan struct{}
}

func (s *gatedCommit) Commit(ctx context.Context, r []storage.Record) error {
	if s.started != nil {
		close(s.started)
		s.started = nil
		<-s.release
	}
	return s.Store.Commit(ctx, r)
}

func userRows(t *testing.T, store interface {
	Daily(context.Context, storage.Filter) ([]storage.Daily, error)
}, hash string) int {
	t.Helper()
	rows, err := store.Daily(context.Background(), storage.Filter{AppName: "bot", UserHash: hash})
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

func TestForgetWaitsForCompletionQueue(t *testing.T) {
	s, store, snapshot, e := setup(t)
	provider, _ := snapshot.Provider("test")
	admission, rejection := s.Admit(context.Background(), snapshot, e, "test", provider)
	if rejection != nil {
		t.Fatal(rejection)
	}
	admission.Complete(proxy.AttemptResult{Operation: proxy.OperationGeneration, UsageComplete: true, Usage: proxy.Usage{Input: known(10), Output: known(3)}})
	subject := []storage.SubjectRef{{App: "bot", Hash: e.Identity.Hash().Hex()}}
	if !s.HasPending(subject) {
		t.Fatal("the queued completion is not visible")
	}
	err := s.Maintain(context.Background(), func(ctx context.Context) error {
		if s.HasPending(subject) {
			t.Fatal("the queue was not drained before the deletion")
		}
		if userRows(t, store, subject[0].Hash) != 1 {
			t.Fatal("the completion was not materialized before the deletion")
		}
		counts, err := store.ForgetSubjects(ctx, subject)
		if err != nil || counts.Requests != 1 || counts.UsageDaily != 1 {
			t.Fatalf("deletion: %+v %v", counts, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if userRows(t, store, subject[0].Hash) != 0 {
		t.Fatal("the user came back")
	}
	app, err := store.Daily(context.Background(), storage.Filter{AppName: "bot"})
	if err != nil || len(app) != 1 || app[0].Requests != 1 {
		t.Fatalf("the app total changed: %+v %v", app, err)
	}
}

func TestForgetFlushFailureDeletesNothing(t *testing.T) {
	s, store, snapshot, e := setup(t)
	wrapped := &commitFailure{Store: store}
	wrapped.fail.Store(true)
	s.store = wrapped
	provider, _ := snapshot.Provider("test")
	admission, _ := s.Admit(context.Background(), snapshot, e, "test", provider)
	admission.Complete(proxy.AttemptResult{Operation: proxy.OperationGeneration})
	called := false
	if err := s.Maintain(context.Background(), func(context.Context) error { called = true; return nil }); err == nil || called {
		t.Fatalf("a failed flush must not run the deletion: %v %v", err, called)
	}
	if len(s.pending) != 1 {
		t.Fatal("the failed flush lost the queued record")
	}
	sentinel := errors.New("deletion refused")
	wrapped.fail.Store(false)
	if err := s.Maintain(context.Background(), func(context.Context) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("the deletion result must come back: %v", err)
	}
}

func TestForgetNoLateBatchResurrection(t *testing.T) {
	s, store, snapshot, e := setup(t)
	gate := &gatedCommit{Store: store, started: make(chan struct{}), release: make(chan struct{})}
	started := gate.started
	s.store = gate
	provider, _ := snapshot.Provider("test")
	admission, _ := s.Admit(context.Background(), snapshot, e, "test", provider)
	admission.Complete(proxy.AttemptResult{Operation: proxy.OperationGeneration, UsageComplete: true, Usage: proxy.Usage{Input: known(10), Output: known(3)}})
	flushed := make(chan error, 1)
	go func() { flushed <- s.Flush(context.Background()) }()
	<-started
	subject := []storage.SubjectRef{{App: "bot", Hash: e.Identity.Hash().Hex()}}
	entered := make(chan struct{})
	maintained := make(chan error, 1)
	go func() {
		maintained <- s.Maintain(context.Background(), func(ctx context.Context) error {
			close(entered)
			_, err := store.ForgetSubjects(ctx, subject)
			return err
		})
	}()
	select {
	case <-entered:
		t.Fatal("the deletion ran while a batch was being committed")
	default:
	}
	close(gate.release)
	if err := <-flushed; err != nil {
		t.Fatal(err)
	}
	if err := <-maintained; err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if userRows(t, store, subject[0].Hash) != 0 {
		t.Fatal("a batch in flight brought the user back")
	}
}
