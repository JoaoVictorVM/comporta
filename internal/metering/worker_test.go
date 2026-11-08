package metering

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/proxy"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

type commitFailure struct {
	Store
	fail  atomic.Bool
	calls atomic.Int64
}

func (s *commitFailure) Commit(ctx context.Context, r []storage.Record) error {
	s.calls.Add(1)
	if s.fail.Load() {
		return storage.ErrUnavailable
	}
	return s.Store.Commit(ctx, r)
}

func TestCommitFailureKeepsBatchAndSlots(t *testing.T) {
	s, store, snapshot, e := setup(t)
	wrapped := &commitFailure{Store: store}
	wrapped.fail.Store(true)
	s.store = wrapped
	p, _ := snapshot.Provider("test")
	complete, rejection := s.Admit(context.Background(), snapshot, e, "test", p)
	if rejection != nil {
		t.Fatal(rejection)
	}
	complete(proxy.AttemptResult{Operation: proxy.OperationGeneration})
	if err := s.Flush(context.Background()); err == nil {
		t.Fatal("fault ignored")
	}
	if len(s.slots) != 1 || len(s.pending) != 1 {
		t.Fatal("failed batch released capacity")
	}
	wrapped.fail.Store(false)
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := store.Daily(context.Background(), storage.Filter{AppName: "bot"})
	if err != nil || len(rows) != 1 || rows[0].Requests != 1 {
		t.Fatal("retry double aggregated")
	}
}

type controlledTimer struct {
	ticks  chan time.Time
	resets chan time.Duration
}

func (t *controlledTimer) C() <-chan time.Time   { return t.ticks }
func (t *controlledTimer) Reset(d time.Duration) { t.resets <- d }
func (t *controlledTimer) Stop()                 {}

func TestBatchEveryTwoSecondsAndRetryBackoff(t *testing.T) {
	s, store, snapshot, e := setup(t)
	p, _ := snapshot.Provider("test")
	complete, reject := s.Admit(context.Background(), snapshot, e, "test", p)
	if reject != nil {
		t.Fatal(reject)
	}
	complete(proxy.AttemptResult{Operation: proxy.OperationGeneration})
	wrapped := &commitFailure{Store: store}
	wrapped.fail.Store(true)
	s.store = wrapped
	created := make(chan *controlledTimer, 2)
	s.newTimer = func(d time.Duration) timer {
		tick := &controlledTimer{ticks: make(chan time.Time, 1), resets: make(chan time.Duration, 4)}
		tick.resets <- d
		created <- tick
		return tick
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	batch, maintenance := <-created, <-created
	if <-batch.resets != 2*time.Second || <-maintenance.resets != time.Hour {
		t.Fatal("incorrect schedules")
	}
	if wrapped.calls.Load() != 0 {
		t.Fatal("flushed before timer")
	}
	batch.ticks <- time.Now()
	select {
	case d := <-batch.resets:
		if d != time.Second {
			t.Fatal(d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker stalled")
	}
	s.wake <- struct{}{}
	time.Sleep(20 * time.Millisecond)
	if wrapped.calls.Load() != 1 {
		t.Fatal("wake bypassed retry backoff")
	}
	wrapped.fail.Store(false)
	batch.ticks <- time.Now()
	select {
	case <-batch.resets:
	case <-time.After(5 * time.Second):
		t.Fatal("retry stalled")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop")
	}
	if len(s.slots) != 0 {
		t.Fatal("committed slots retained")
	}
}

func TestStorageErrorsLoggedAtMostEvery30Seconds(t *testing.T) {
	s, _, _, _ := setup(t)
	var logs bytes.Buffer
	s.logger = slog.New(slog.NewJSONHandler(&logs, nil))
	now := time.Unix(100, 0)
	s.now = func() time.Time { return now }
	for range 100 {
		s.warnStorage(storage.ErrUnavailable)
	}
	now = now.Add(29 * time.Second)
	s.warnStorage(storage.ErrUnavailable)
	if strings.Count(logs.String(), "usage persistence unavailable") != 1 {
		t.Fatal("unbounded storage logs")
	}
	now = now.Add(time.Second)
	s.warnStorage(storage.ErrUnavailable)
	if strings.Count(logs.String(), "usage persistence unavailable") != 2 {
		t.Fatal("diagnostic not renewed")
	}
}

type memoryStore struct {
	Store
	mu        sync.Mutex
	admitted  map[string]storage.Record
	committed map[string]storage.Record
}

func (s *memoryStore) Admit(_ context.Context, r storage.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.admitted[r.RequestID] = r
	return nil
}
func (s *memoryStore) Complete(context.Context, storage.Record) error { return nil }
func (s *memoryStore) Commit(_ context.Context, rs []storage.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range rs {
		s.committed[r.RequestID] = r
		delete(s.admitted, r.RequestID)
	}
	return nil
}

func TestConcurrentAdmissionCannotOverbookAndDrainLosesNothing(t *testing.T) {
	s, realStore, snapshot, base := setup(t)
	memory := &memoryStore{Store: realStore, admitted: map[string]storage.Record{}, committed: map[string]storage.Record{}}
	s.store = memory
	p, _ := snapshot.Provider("test")
	handles := make([]func(proxy.AttemptResult), 50000)
	var workers sync.WaitGroup
	var index atomic.Int64
	for range 32 {
		workers.Go(func() {
			for {
				i := int(index.Add(1)) - 1
				if i >= len(handles) {
					return
				}
				e := *base
				e.RequestID = fmt.Sprintf("%032x", i+1)
				handle, reject := s.Admit(context.Background(), snapshot, &e, "test", p)
				if reject != nil {
					t.Error("admission rejected before capacity")
					return
				}
				handles[i] = handle
			}
		})
	}
	workers.Wait()
	extra := *base
	extra.RequestID = fmt.Sprintf("%032x", 50001)
	if _, reject := s.Admit(context.Background(), snapshot, &extra, "test", p); reject == nil || reject.Status != 503 {
		t.Fatal("capacity overbooked")
	}
	for _, complete := range handles {
		if complete == nil {
			t.Fatal("missing handle")
		}
		complete(proxy.AttemptResult{Operation: proxy.OperationGeneration})
	}
	if len(s.slots) != 50000 {
		t.Fatal("completion released capacity before commit")
	}
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(memory.admitted) != 0 || len(memory.committed) != 50000 || len(s.slots) != 0 {
		t.Fatal("drain lost admitted records")
	}
}
