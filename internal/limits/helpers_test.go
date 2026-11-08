package limits

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

var errInjected = errors.New("injected failure")

// memStore is an in-memory LimitStore with the same transition rules as the
// SQLite store, used to test the algorithm without disk latency.
type memStore struct {
	mu           sync.Mutex
	seq          int64
	reservations map[handleKey]*storage.Reservation
	ledger       []storage.LedgerEntry
	rates        map[string]*storage.RateEvent
	checkpoint   *storage.LimitCheckpoint
	buckets      map[bucketKey]storage.BudgetBucket
	daily        []storage.Daily
	missing      []storage.MissingUsage
	results      map[handleKey]*storage.Record
	failCommit   bool
	failSettle   bool
	failSave     bool
	saves        int
}

func newMemStore() *memStore {
	return &memStore{reservations: map[handleKey]*storage.Reservation{}, rates: map[string]*storage.RateEvent{}, buckets: map[bucketKey]storage.BudgetBucket{}, results: map[handleKey]*storage.Record{}}
}

func (s *memStore) commit(r storage.Reservation) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failCommit {
		return 0, errInjected
	}
	key := handleKey{r.RequestID, r.AttemptIndex}
	if _, ok := s.reservations[key]; ok {
		return 0, storage.ErrConflict
	}
	copied := r
	copied.Items = append([]storage.LimitItem(nil), r.Items...)
	s.reservations[key] = &copied
	if r.Rate != nil {
		rate := *r.Rate
		s.rates[r.RequestID] = &rate
	}
	s.seq++
	entry := storage.LedgerEntry{Seq: s.seq, RequestID: r.RequestID, AttemptIndex: r.AttemptIndex, Transition: storage.TransitionAdmit, OccurredAt: r.AdmittedAt, AppName: r.AppName}
	for _, item := range r.Items {
		entry.Deltas = append(entry.Deltas, delta(item, 0, item.Reserved))
	}
	s.ledger = append(s.ledger, entry)
	return s.seq, nil
}

func delta(item storage.LimitItem, spent, reserved int64) storage.LimitDelta {
	return storage.LimitDelta{Scope: item.Scope, Subject: item.Subject, Anonymous: item.Anonymous, Dimension: item.Dimension, WindowStart: item.WindowStart, WindowEnd: item.WindowEnd, Spent: spent, Reserved: reserved}
}

func (s *memStore) AdmitReserved(_ context.Context, _ storage.Record, r storage.Reservation) (int64, error) {
	return s.commit(r)
}

func (s *memStore) CompleteSettled(ctx context.Context, _ storage.Record, st storage.Settlement) (int64, error) {
	return s.SettleReservation(ctx, st)
}

func (s *memStore) SettleReservation(_ context.Context, st storage.Settlement) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failSettle {
		return 0, errInjected
	}
	r := s.reservations[handleKey{st.RequestID, st.AttemptIndex}]
	if r == nil || len(st.Settled) != len(r.Items) {
		return 0, storage.ErrConflict
	}
	if r.State != storage.ReservationReserved {
		for _, e := range s.ledger {
			if e.RequestID == st.RequestID && e.AttemptIndex == st.AttemptIndex && e.Transition == st.Transition {
				return e.Seq, nil
			}
		}
		return 0, storage.ErrConflict
	}
	s.seq++
	entry := storage.LedgerEntry{Seq: s.seq, RequestID: r.RequestID, AttemptIndex: r.AttemptIndex, Transition: st.Transition, OccurredAt: st.At, AppName: r.AppName}
	for i, item := range r.Items {
		settled := st.Settled[i]
		r.Items[i].Settled = &settled
		entry.Deltas = append(entry.Deltas, delta(item, settled, -item.Reserved))
	}
	s.ledger = append(s.ledger, entry)
	r.State = storage.ReservationSettled
	if st.Transition == storage.TransitionRelease {
		r.State = storage.ReservationReleased
	}
	if rate := s.rates[r.RequestID]; rate != nil && st.RateTokens != nil {
		tokens := *st.RateTokens
		rate.ActualTokens = &tokens
		rate.Active = false
	}
	return s.seq, nil
}

func (s *memStore) LoadLimits(_ context.Context, since time.Time) (storage.LimitState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var state storage.LimitState
	if s.checkpoint != nil {
		cp := *s.checkpoint
		cp.Buckets = nil
		for _, b := range s.buckets {
			cp.Buckets = append(cp.Buckets, b)
		}
		state.Checkpoint = &cp
	}
	for _, e := range s.ledger {
		if state.Checkpoint == nil || e.Seq > state.Checkpoint.CoveredSeq {
			state.Ledger = append(state.Ledger, e)
		}
	}
	for key, r := range s.reservations {
		if r.State == storage.ReservationReserved {
			state.Open = append(state.Open, storage.OpenReservation{Reservation: *r, Result: s.results[key]})
		}
	}
	sort.Slice(state.Open, func(i, j int) bool { return state.Open[i].Reservation.RequestID < state.Open[j].Reservation.RequestID })
	for _, e := range s.rates {
		if e.Active || e.AdmittedAt.After(since) {
			state.RateEvents = append(state.RateEvents, *e)
		}
	}
	sort.Slice(state.RateEvents, func(i, j int) bool { return state.RateEvents[i].AdmittedAt.Before(state.RateEvents[j].AdmittedAt) })
	state.Daily, state.Missing = s.daily, s.missing
	return state, nil
}

func (s *memStore) SaveCheckpoint(_ context.Context, cp storage.LimitCheckpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failSave {
		return errInjected
	}
	s.saves++
	for _, b := range cp.Removed {
		delete(s.buckets, bucketKey{b.AppName, Scope(b.Scope), b.Subject, b.Anonymous, Dimension(b.Dimension), b.WindowStart, b.WindowEnd})
	}
	for _, b := range cp.Buckets {
		s.buckets[bucketKey{b.AppName, Scope(b.Scope), b.Subject, b.Anonymous, Dimension(b.Dimension), b.WindowStart, b.WindowEnd}] = b
	}
	stored := cp
	stored.Buckets, stored.Removed = nil, nil
	s.checkpoint = &stored
	return nil
}

func (s *memStore) set(f func(*memStore)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f(s)
}

type recordingPublisher struct {
	mu     sync.Mutex
	events []events.Event
}

func (p *recordingPublisher) Publish(_ context.Context, e events.Event) error {
	if err := e.Validate(); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, e)
	return nil
}

func (p *recordingPublisher) types() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, e := range p.events {
		out = append(out, string(e.Type))
	}
	return out
}

type env struct {
	t       *testing.T
	clock   *FakeClock
	store   *memStore
	manager *Manager
	policy  Policy
	next    int
}

var start = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func newEnv(t *testing.T, policy Policy) *env {
	t.Helper()
	clock := NewFakeClock(start)
	store := newMemStore()
	if policy.App == "" {
		policy.App = "bot"
	}
	if policy.Timezone == "" {
		policy.Timezone = "UTC"
	}
	if policy.FailMode == "" {
		policy.FailMode = config.FailClosed
	}
	return &env{t: t, clock: clock, store: store, policy: policy, manager: New(Options{Store: store, Clock: clock, Logger: slog.New(slog.DiscardHandler)})}
}

func user(app string, n int) Subject {
	return Subject{App: app, Hash: fmt.Sprintf("%032x", n+1)}
}

func (e *env) request(s Subject, tokens int64, cost config.Microusd) Request {
	e.next++
	return Request{RequestID: fmt.Sprintf("%032x", e.next+1<<20), AttemptIndex: 1, Subject: s, Operation: OperationGeneration, EstimatedTokens: tokens, ReservedCost: cost}
}

func (e *env) reserve(r Request) (*Handle, Decision, error) {
	return e.manager.Reserve(e.policy, r, e.store.commit)
}

func (e *env) mustReserve(r Request) *Handle {
	e.t.Helper()
	h, d, err := e.reserve(r)
	if err != nil || !d.Allowed || h == nil {
		e.t.Fatalf("admission refused: %+v %v", d.Primary, err)
	}
	return h
}

func (e *env) mustReject(r Request) Decision {
	e.t.Helper()
	h, d, err := e.reserve(r)
	if err != nil || d.Allowed || h != nil || d.Primary == nil {
		e.t.Fatalf("admission not refused: %+v %v", d, err)
	}
	return d
}

func (e *env) settle(h *Handle, cost config.Microusd, tokens int64) {
	e.t.Helper()
	if err := e.manager.Settle(h, Usage{Cost: cost, Tokens: tokens, Complete: true}, nil); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) view(s *Subject, name string) LimitView {
	e.t.Helper()
	for _, v := range e.manager.State(e.policy, s).Limits {
		if v.LimitName == name {
			return v
		}
	}
	e.t.Fatalf("limit %s not in state", name)
	return LimitView{}
}

// restart builds a new manager on the same store, as a process restart would.
func (e *env) restart() {
	e.t.Helper()
	e.manager = New(Options{Store: e.store, Clock: e.clock, Logger: slog.New(slog.DiscardHandler)})
	if err := e.manager.Recover(context.Background(), nil); err != nil {
		e.t.Fatal(err)
	}
}
