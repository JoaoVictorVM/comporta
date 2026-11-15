package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

const fallbackRequest = "abcdefabcdefabcdefabcdefabcdef01"

var fallbackModels = []string{"", "model-a", "model-b", "model-c"}

// fallbackAttempt is the admission of one attempt of the same logical request.
func fallbackAttempt(index int, started time.Time) (storage.Record, storage.Reservation) {
	r := fixture()
	r.RequestID, r.AttemptIndex = fallbackRequest, index
	r.StartedAt, r.FinishedAt = started.Add(time.Duration(index)*time.Millisecond), time.Time{}
	r.Input, r.Cost = nil, 0
	r.ModelRequested, r.ModelUsed = fallbackModels[1], fallbackModels[index]
	r.Quote = storage.Quote{EstimatedInput: 40, EstimatedOutput: 60, ReservedCost: 100}
	if index > 1 {
		r.Outcome, r.FallbackFrom, r.FallbackCause = "fallback", fallbackModels[index-1], "529"
	}
	day := started.Truncate(24 * time.Hour)
	res := storage.Reservation{
		RequestID: r.RequestID, AttemptIndex: index, AppName: r.AppName, Subject: r.UserHash, AdmittedAt: r.StartedAt, State: storage.ReservationReserved,
		Quote: storage.LimitQuote{Operation: "generation", EstimatedTokens: 100, ReservedCost: 100, Generation: 1},
		Items: []storage.LimitItem{{Scope: "app", Dimension: "usd_per_day", WindowStart: day.UnixMicro(), WindowEnd: day.Add(24 * time.Hour).UnixMicro(), Timezone: "UTC", Reserved: 100}},
		Rate:  &storage.RateEvent{RequestID: r.RequestID, AttemptIndex: index, AppName: r.AppName, Subject: r.UserHash, AdmittedAt: r.StartedAt, EstimatedTokens: 100, Active: true},
	}
	return r, res
}

func fallbackOperation(first storage.Record) storage.Operation {
	return storage.Operation{RequestID: first.RequestID, AppName: first.AppName, Subject: first.UserHash, Generation: 1, StartedAt: first.StartedAt}
}

// failedAttempt is the attempt's own result: an overloaded provider, usage
// unknown, charged at its reservation.
func failedAttempt(r storage.Record) (storage.Record, storage.Settlement) {
	status := 529
	r.FinishedAt = r.StartedAt
	r.UpstreamStatus, r.DownstreamStatus = &status, nil
	r.ErrorClass, r.ErrorCode = "overload", "provider_overloaded"
	r.Cost, r.UsageMissing = r.ReservedCost, true
	tokens := int64(100)
	return r, storage.Settlement{RequestID: r.RequestID, AttemptIndex: r.AttemptIndex, Transition: storage.TransitionSettle, At: r.FinishedAt, Settled: []int64{int64(r.Cost)}, RateTokens: &tokens}
}

func succeededAttempt(r storage.Record) (storage.Record, storage.Settlement) {
	in, out, status := int64(20), int64(10), 200
	r.FinishedAt = r.StartedAt
	r.Input, r.Output, r.Cost = &in, &out, 49
	r.UpstreamStatus, r.DownstreamStatus = &status, &status
	tokens := int64(30)
	return r, storage.Settlement{RequestID: r.RequestID, AttemptIndex: r.AttemptIndex, Transition: storage.TransitionSettle, At: r.FinishedAt, Settled: []int64{49}, RateTokens: &tokens}
}

// twoAttempts admits and settles attempt 1, then admits attempt 2.
func twoAttempts(t *testing.T, s *Store) (storage.Record, storage.Record, storage.Reservation) {
	t.Helper()
	ctx := context.Background()
	started := time.Now().UTC()
	first, res1 := fallbackAttempt(1, started)
	if _, err := s.AdmitReserved(ctx, first, res1); err != nil {
		t.Fatal(err)
	}
	done, st := failedAttempt(first)
	if _, err := s.CompleteSettled(ctx, done, st); err != nil {
		t.Fatal(err)
	}
	second, res2 := fallbackAttempt(2, started)
	if _, err := s.AdmitContinuation(ctx, second, res2, fallbackOperation(first)); err != nil {
		t.Fatal(err)
	}
	return done, second, res2
}

func TestFallbackTerminalExactlyOnce(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	first, second, _ := twoAttempts(t, s)
	final, st := succeededAttempt(second)
	if _, err := s.CompleteSettled(ctx, final, st); err != nil {
		t.Fatal(err)
	}
	intermediate := first
	intermediate.RequestCount = 0
	// The intermediate attempt cannot be committed as terminal while a later
	// attempt exists.
	if err := s.Commit(ctx, []storage.Record{first}); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("terminal intermediate: %v", err)
	}
	if err := s.Commit(ctx, []storage.Record{intermediate, final}); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT count(*) FROM requests WHERE request_id=? AND request_count=1`, fallbackRequest); n != 1 {
		t.Fatalf("terminal rows %d", n)
	}
	daily, err := s.Daily(ctx, storage.Filter{AppName: "bot"})
	if err != nil || len(daily) != 1 || daily[0].Requests != 1 || daily[0].Attempts != 2 || daily[0].Cost != 149 {
		t.Fatalf("daily %+v %v", daily, err)
	}
	if n := count(t, s, `SELECT count(*) FROM request_operations WHERE request_id=? AND state='closed' AND materialized=1 AND terminal_attempt_index=2 AND terminal_status=200`, fallbackRequest); n != 1 {
		t.Fatalf("session not closed: %d", n)
	}
	page, err := s.ReadEvents(ctx, events.Filter{App: "bot", Types: []events.Type{events.Fallback}})
	if err != nil || len(page.Events) != 1 {
		t.Fatalf("fallback events %+v %v", page.Events, err)
	}
	e := page.Events[0]
	if e.ValidateFallback() != nil || e.Payload.FromModel != "model-a" || e.Payload.ToModel != "model-b" || e.Payload.Cause != "529" || e.Payload.AttemptIndex != 2 || e.UserHash != "" {
		t.Fatalf("fallback event %+v", e)
	}
	if n := count(t, s, `SELECT count(*) FROM metering_journal`); n != 0 {
		t.Fatalf("journal residue %d", n)
	}
}

func TestFallbackBlockedContinuationClosesPrevious(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	started := time.Now().UTC()
	first, res1 := fallbackAttempt(1, started)
	if _, err := s.AdmitReserved(ctx, first, res1); err != nil {
		t.Fatal(err)
	}
	second, res2 := fallbackAttempt(2, started)
	// The previous reservation is still reserved: nothing may be admitted.
	if _, err := s.AdmitContinuation(ctx, second, res2, fallbackOperation(first)); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("continuation before settlement: %v", err)
	}
	done, st := failedAttempt(first)
	if _, err := s.CompleteSettled(ctx, done, st); err != nil {
		t.Fatal(err)
	}
	// The next attempt was refused locally: the previous one ends the request,
	// with the refusal as the terminal answer and no late attempt 0.
	status := 429
	done.TerminalErrorCode, done.TerminalStatus = "budget_exceeded", &status
	if err := s.Commit(ctx, []storage.Record{done}); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT count(*) FROM requests WHERE request_id=?`, fallbackRequest); n != 1 {
		t.Fatalf("rows %d", n)
	}
	if n := count(t, s, `SELECT count(*) FROM requests WHERE attempt_index=0`); n != 0 {
		t.Fatal("late attempt 0")
	}
	if n := count(t, s, `SELECT count(*) FROM request_operations`); n != 0 {
		t.Fatal("session for a single attempt")
	}
	var stored storage.Record
	var data string
	if err := s.db.QueryRow(`SELECT record_json FROM requests WHERE request_id=?`, fallbackRequest).Scan(&data); err != nil {
		t.Fatal(err)
	}
	if err := decodeRecord(data, &stored); err != nil || stored.TerminalErrorCode != "budget_exceeded" || stored.ErrorCode != "provider_overloaded" {
		t.Fatalf("record %+v %v", stored, err)
	}
}

func TestFallbackReplayIsIdempotent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	first, second, res2 := twoAttempts(t, s)
	seq := count(t, s, `SELECT max(seq) FROM limit_ledger WHERE attempt_index=2`)
	again, err := s.AdmitContinuation(ctx, second, res2, fallbackOperation(first))
	if err != nil || again != int64(seq) {
		t.Fatalf("replay %d %v", again, err)
	}
	changed := second
	changed.ModelUsed = "model-c"
	if _, err := s.AdmitContinuation(ctx, changed, res2, fallbackOperation(first)); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("changed replay %v", err)
	}
	for _, query := range []string{
		`SELECT count(*) FROM metering_journal WHERE attempt_index=2`,
		`SELECT count(*) FROM limit_reservations WHERE attempt_index=2`,
		`SELECT count(*) FROM limit_attempt_rate_events WHERE attempt_index=2`,
		`SELECT count(*) FROM events WHERE type='fallback'`,
	} {
		if n := count(t, s, query); n != 1 {
			t.Fatalf("%s: %d", query, n)
		}
	}
	final, st := succeededAttempt(second)
	if _, err := s.CompleteSettled(ctx, final, st); err != nil {
		t.Fatal(err)
	}
	intermediate := first
	intermediate.RequestCount = 0
	for range 2 {
		if err := s.Commit(ctx, []storage.Record{intermediate, final}); err != nil {
			t.Fatal(err)
		}
	}
	daily, err := s.Daily(ctx, storage.Filter{AppName: "bot"})
	if err != nil || len(daily) != 1 || daily[0].Requests != 1 || daily[0].Attempts != 2 {
		t.Fatalf("daily %+v %v", daily, err)
	}
	// A third attempt cannot skip an index nor follow a closed session.
	third, res3 := fallbackAttempt(3, first.StartedAt)
	if _, err := s.AdmitContinuation(ctx, third, res3, fallbackOperation(first)); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("continuation after close %v", err)
	}
}

func TestFallbackCrashBoundaries(t *testing.T) {
	ctx := context.Background()
	check := func(t *testing.T, s *Store, wantCost int64) {
		t.Helper()
		if err := s.Recover(ctx, time.Now()); err != nil {
			t.Fatal(err)
		}
		if err := s.Recover(ctx, time.Now()); err != nil {
			t.Fatal(err)
		}
		if n := count(t, s, `SELECT count(*) FROM requests WHERE request_id=? AND request_count=1 AND attempt_index=2`, fallbackRequest); n != 1 {
			t.Fatalf("terminal attempt %d", n)
		}
		if n := count(t, s, `SELECT count(*) FROM requests WHERE request_id=? AND request_count=0 AND attempt_index=1`, fallbackRequest); n != 1 {
			t.Fatalf("intermediate attempt %d", n)
		}
		daily, err := s.Daily(ctx, storage.Filter{AppName: "bot"})
		if err != nil || len(daily) != 1 || daily[0].Requests != 1 || daily[0].Attempts != 2 || int64(daily[0].Cost) != wantCost {
			t.Fatalf("daily %+v %v", daily, err)
		}
		if n := count(t, s, `SELECT count(*) FROM request_operations WHERE state='closed' AND materialized=1`); n != 1 {
			t.Fatal("session left active")
		}
	}
	t.Run("next attempt admitted, never completed", func(t *testing.T) {
		s := testStore(t)
		twoAttempts(t, s)
		// Unknown result: the reservation is charged in full, never zero.
		check(t, s, 200)
	})
	t.Run("both attempts completed, none materialized", func(t *testing.T) {
		s := testStore(t)
		_, second, _ := twoAttempts(t, s)
		final, st := succeededAttempt(second)
		if _, err := s.CompleteSettled(ctx, final, st); err != nil {
			t.Fatal(err)
		}
		check(t, s, 149)
	})
	t.Run("intermediate already materialized", func(t *testing.T) {
		s := testStore(t)
		first, _, _ := twoAttempts(t, s)
		first.RequestCount = 0
		if err := s.Commit(ctx, []storage.Record{first}); err != nil {
			t.Fatal(err)
		}
		check(t, s, 200)
	})
	t.Run("settled before the next admission", func(t *testing.T) {
		s := testStore(t)
		first, res := fallbackAttempt(1, time.Now().UTC())
		if _, err := s.AdmitReserved(ctx, first, res); err != nil {
			t.Fatal(err)
		}
		done, st := failedAttempt(first)
		if _, err := s.CompleteSettled(ctx, done, st); err != nil {
			t.Fatal(err)
		}
		if err := s.Recover(ctx, time.Now()); err != nil {
			t.Fatal(err)
		}
		// Without a session the only attempt is the terminal one.
		if n := count(t, s, `SELECT count(*) FROM requests WHERE request_count=1 AND attempt_index=1`); n != 1 {
			t.Fatal("attempt not terminal")
		}
	})
}

func TestFallbackMigrationLegacyJournal(t *testing.T) {
	migrateTarget = 3
	t.Cleanup(func() { migrateTarget = 0 })
	dir := filepath.Join(t.TempDir(), "private")
	s, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	pending := limitRecord(9)
	m := limitManager(t, s)
	if _, err = admitLimited(m, s, pending); err != nil {
		t.Fatal(err)
	}
	s.Close()
	migrateTarget = 0
	s, err = Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v, err := checkSchema(ctx, s.db); err != nil || v != 4 {
		t.Fatalf("version %d %v", v, err)
	}
	if err = s.Recover(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT count(*) FROM requests WHERE request_count=1 AND recovered=1`); n != 1 {
		t.Fatalf("legacy journal not recovered as one request: %d", n)
	}
	if n := count(t, s, `SELECT count(*) FROM request_operations`); n != 0 {
		t.Fatal("legacy request gained a session")
	}
	// Recovery of the limits keeps a single charge for the legacy reservation.
	m = limitManager(t, s)
	used, reserved := budget(t, m)
	if used != 100 || reserved != 0 {
		t.Fatalf("budget used %d reserved %d", used, reserved)
	}
}

func TestFallbackRecordValidation(t *testing.T) {
	r, _ := fallbackAttempt(2, time.Now().UTC())
	if err := validate(r); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*storage.Record){
		"cause without origin":   func(r *storage.Record) { r.FallbackFrom = "" },
		"cause outside the set":  func(r *storage.Record) { r.FallbackCause = "400" },
		"fallback on attempt 1":  func(r *storage.Record) { r.AttemptIndex = 1 },
		"transport outcome":      func(r *storage.Record) { r.Outcome = "ok" },
		"model with spaces":      func(r *storage.Record) { r.FallbackFrom = "model a" },
		"terminal status range":  func(r *storage.Record) { s := 99; r.TerminalStatus = &s },
		"terminal on attempt 0+": func(r *storage.Record) { r.RequestCount = 0; r.TerminalErrorCode = "fallback_exhausted" },
		"terminal code charset":  func(r *storage.Record) { r.TerminalErrorCode = "Bad Code" },
	} {
		changed := r
		change(&changed)
		if err := validate(changed); !errors.Is(err, storage.ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}
