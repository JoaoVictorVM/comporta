package sqlite

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/limits"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

func limitPolicy() limits.Policy {
	return limits.Policy{App: "bot", Timezone: "UTC", FailMode: config.FailClosed, AppSet: limits.Set{Budget: config.Budget{USDPerDay: 1_000_000, TokensPerDay: 1_000_000}}}
}

func limitRecord(n int) storage.Record {
	r := fixture()
	r.RequestID = fmt.Sprintf("%032x", n)
	r.Input = nil
	r.Cost = 0
	r.FinishedAt = time.Time{}
	r.Quote = storage.Quote{EstimatedInput: 40, EstimatedOutput: 60, ReservedCost: 100}
	return r
}

func completed(r storage.Record) storage.Record {
	in, out := int64(20), int64(10)
	r.FinishedAt = r.StartedAt
	r.Input, r.Output, r.Cost = &in, &out, 49
	return r
}

func limitRequest(r storage.Record) limits.Request {
	return limits.Request{RequestID: r.RequestID, AttemptIndex: r.AttemptIndex, Subject: limits.Subject{App: r.AppName, Hash: r.UserHash}, Operation: limits.OperationGeneration, EstimatedTokens: 100, ReservedCost: r.ReservedCost}
}

func limitManager(t testing.TB, store storage.LimitStore) *limits.Manager {
	t.Helper()
	m := limits.New(limits.Options{Store: store})
	if err := m.Recover(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	return m
}

func admitLimited(m *limits.Manager, s *Store, r storage.Record) (*limits.Handle, error) {
	h, d, err := m.Reserve(limitPolicy(), limitRequest(r), func(res storage.Reservation) (int64, error) {
		return s.AdmitReserved(context.Background(), r, res)
	})
	if err == nil && !d.Allowed {
		err = errors.New("refused")
	}
	return h, err
}

func settleCompleted(m *limits.Manager, s *Store, h *limits.Handle, r storage.Record) error {
	return m.Settle(h, limits.Usage{Cost: 49, Tokens: 30, Complete: true}, func(st storage.Settlement) (int64, error) {
		return s.CompleteSettled(context.Background(), r, st)
	})
}

func budget(t *testing.T, m *limits.Manager) (int64, int64) {
	t.Helper()
	for _, v := range m.State(limitPolicy(), nil).Limits {
		if v.Dimension == limits.USDPerDay {
			return v.Used, v.Reserved
		}
	}
	t.Fatal("no usd_per_day budget in state")
	return 0, 0
}

func count(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestAdmissionJournalAndReserveCommitTogether(t *testing.T) {
	s := testStore(t)
	m := limitManager(t, s)
	if _, err := admitLimited(m, s, limitRecord(1)); err != nil {
		t.Fatal(err)
	}
	for query, want := range map[string]int{
		`SELECT count(*) FROM metering_journal`:        1,
		`SELECT count(*) FROM limit_reservations`:      1,
		`SELECT count(*) FROM limit_reservation_items`: 8,
		`SELECT count(*) FROM limit_rate_events`:       1,
		`SELECT count(*) FROM limit_ledger`:            1,
	} {
		if got := count(t, s, query); got != want {
			t.Fatalf("%s = %d, want %d", query, got, want)
		}
	}
	// A journal entry that conflicts aborts the whole admission: no reservation
	// is written and memory does not move.
	conflicting := limitRecord(2)
	conflicting.ModelUsed = "other"
	if err := s.Admit(context.Background(), conflicting); err != nil {
		t.Fatal(err)
	}
	if _, err := admitLimited(m, s, limitRecord(2)); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("conflict not reported: %v", err)
	}
	if count(t, s, `SELECT count(*) FROM limit_reservations`) != 1 || count(t, s, `SELECT count(*) FROM limit_ledger`) != 1 {
		t.Fatal("partial admission persisted")
	}
	if _, reserved := budget(t, m); reserved != 100 {
		t.Fatalf("memory moved without a commit: %d", reserved)
	}
}

func TestCompleteAndSettlementCommitTogether(t *testing.T) {
	s := testStore(t)
	m := limitManager(t, s)
	r := limitRecord(1)
	h, err := admitLimited(m, s, r)
	if err != nil {
		t.Fatal(err)
	}
	if err = settleCompleted(m, s, h, completed(r)); err != nil {
		t.Fatal(err)
	}
	if count(t, s, `SELECT count(*) FROM metering_journal WHERE state='completed'`) != 1 || count(t, s, `SELECT count(*) FROM limit_reservations WHERE state='settled'`) != 1 {
		t.Fatal("result and settlement not written together")
	}
	r2 := limitRecord(2)
	h2, err := admitLimited(m, s, r2)
	if err != nil {
		t.Fatal(err)
	}
	bad := completed(r2)
	bad.ModelUsed = "changed"
	if err = settleCompleted(m, s, h2, bad); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("conflicting result accepted: %v", err)
	}
	if count(t, s, `SELECT count(*) FROM metering_journal WHERE request_id=? AND result_json IS NULL`, r2.RequestID) != 1 || count(t, s, `SELECT count(*) FROM limit_reservations WHERE request_id=? AND state='reserved'`, r2.RequestID) != 1 {
		t.Fatal("a failed completion left half of its transaction")
	}
	if spent, reserved := budget(t, m); spent != 49 || reserved != 100 {
		t.Fatalf("failed settlement must keep the reservation: spent=%d reserved=%d", spent, reserved)
	}
	if err = m.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if spent, reserved := budget(t, m); spent != 98 || reserved != 0 {
		t.Fatalf("retried settlement: spent=%d reserved=%d", spent, reserved)
	}
}

func TestCommitAmbiguityUsesIdempotencyKey(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r := limitRecord(1)
	now := time.Now().UTC()
	res := storage.Reservation{RequestID: r.RequestID, AttemptIndex: 1, AppName: "bot", Subject: r.UserHash, AdmittedAt: now, State: storage.ReservationReserved,
		Quote: storage.LimitQuote{Operation: "generation", EstimatedTokens: 100, ReservedCost: 100},
		Items: []storage.LimitItem{{Scope: "app", Dimension: "usd_per_day", WindowStart: 1, WindowEnd: 2, Timezone: "UTC", Reserved: 100}},
		Rate:  &storage.RateEvent{RequestID: r.RequestID, AppName: "bot", Subject: r.UserHash, AdmittedAt: now, EstimatedTokens: 100, Active: true}}
	first, err := s.AdmitReserved(ctx, r, res)
	if err != nil {
		t.Fatal(err)
	}
	// The outcome of an ambiguous commit is settled by asking again with the
	// same key: the same data returns the original sequence.
	again, err := s.AdmitReserved(ctx, r, res)
	if err != nil || again != first || s.journalCount != 1 {
		t.Fatalf("retry was not idempotent: %d %d %v", first, again, err)
	}
	changed := res
	changed.Quote.ReservedCost = 101
	if _, err = s.AdmitReserved(ctx, r, changed); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("different data under the same key: %v", err)
	}
	tokens := int64(5)
	st := storage.Settlement{RequestID: r.RequestID, AttemptIndex: 1, Transition: storage.TransitionSettle, At: now, Settled: []int64{70}, RateTokens: &tokens}
	settled, err := s.SettleReservation(ctx, st)
	if err != nil {
		t.Fatal(err)
	}
	if repeat, err := s.SettleReservation(ctx, st); err != nil || repeat != settled {
		t.Fatalf("settlement retry: %d %v", repeat, err)
	}
	other := st
	other.Settled = []int64{1}
	if _, err = s.SettleReservation(ctx, other); !errors.Is(err, storage.ErrConflict) {
		t.Fatal("a different settlement replaced the first")
	}
	unknown := st
	unknown.RequestID = fmt.Sprintf("%032x", 99)
	if _, err = s.SettleReservation(ctx, unknown); !errors.Is(err, storage.ErrConflict) {
		t.Fatal("settlement of an unknown handle accepted")
	}
}

// failingLimits fails every settlement after the first failAfter ones, and the
// first saveFailures checkpoints.
type failingLimits struct {
	*Store
	failAfter    int
	settled      int
	saveFailures int
}

func (f *failingLimits) SettleReservation(ctx context.Context, st storage.Settlement) (int64, error) {
	if f.failAfter > 0 && f.settled >= f.failAfter {
		return 0, storage.ErrUnavailable
	}
	f.settled++
	return f.Store.SettleReservation(ctx, st)
}

func (f *failingLimits) SaveCheckpoint(ctx context.Context, cp storage.LimitCheckpoint) error {
	if f.saveFailures != 0 {
		f.saveFailures--
		return storage.ErrUnavailable
	}
	return f.Store.SaveCheckpoint(ctx, cp)
}

func TestRetryDoesNotDoubleBudget(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	m := limitManager(t, s)
	r := limitRecord(1)
	h, err := admitLimited(m, s, r)
	if err != nil {
		t.Fatal(err)
	}
	failed := m.Settle(h, limits.Usage{Cost: 49, Tokens: 30, Complete: true}, func(storage.Settlement) (int64, error) { return 0, storage.ErrUnavailable })
	if failed == nil || m.Degraded() {
		t.Fatal("failed settlement not reported, or degraded before its retry")
	}
	// The usage batch flushes the record on its own path; it must not settle.
	if err = s.Commit(ctx, []storage.Record{completed(r)}); err != nil {
		t.Fatal(err)
	}
	if err = m.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err = m.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if spent, reserved := budget(t, m); spent != 49 || reserved != 0 || m.Degraded() {
		t.Fatalf("retry: spent=%d reserved=%d", spent, reserved)
	}
	restarted := limitManager(t, s)
	if spent, reserved := budget(t, restarted); spent != 49 || reserved != 0 {
		t.Fatalf("restart: spent=%d reserved=%d", spent, reserved)
	}
	if rows, err := s.Daily(ctx, storage.Filter{AppName: "bot"}); err != nil || len(rows) != 1 || rows[0].Cost != 49 {
		t.Fatalf("usage totals: %+v %v", rows, err)
	}
	if count(t, s, `SELECT count(*) FROM limit_reservations`) != 0 || count(t, s, `SELECT count(*) FROM limit_ledger`) != 0 {
		t.Fatal("checkpoint did not compact a finished reservation whose journal is gone")
	}
}

func TestBlockedRecordIsHistoryNotConsumption(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r := limitRecord(1)
	r.AttemptIndex, r.Outcome, r.RequestCount, r.ReservedCost = 0, "blocked", 0, 0
	r.FinishedAt = r.StartedAt
	r.ErrorClass, r.ErrorCode, r.BlockReason = "rate_limit", "budget_reserved", "reservation"
	if err := s.Commit(ctx, []storage.Record{r, r}); err != nil {
		t.Fatal(err)
	}
	if count(t, s, `SELECT count(*) FROM requests WHERE outcome='blocked' AND attempt_index=0`) != 1 || count(t, s, `SELECT count(*) FROM usage_daily`) != 0 {
		t.Fatal("blocked record counted as consumption")
	}
	if count(t, s, `SELECT count(*) FROM events WHERE type='blocked' AND json_extract(payload_json,'$.reason')='reservation'`) != 1 {
		t.Fatal("block reason not recorded")
	}
	spent := r
	spent.RequestID = fmt.Sprintf("%032x", 2)
	spent.ErrorCode, spent.BlockReason = "budget_exceeded", "spent"
	if err := s.Commit(ctx, []storage.Record{spent}); err != nil {
		t.Fatal(err)
	}
	if count(t, s, `SELECT count(DISTINCT json_extract(record_json,'$.BlockReason')) FROM requests`) != 2 {
		t.Fatal("reservation and spent blocks are not distinguishable")
	}
}
