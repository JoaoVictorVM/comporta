//go:build distributed

package postgres

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

func TestMain(m *testing.M) { os.Exit(testkit.RunWithDatabases(m)) }

func openTest(t testing.TB) (*Store, string) {
	t.Helper()
	dsn := testkit.Postgres(t)
	s, err := Open(context.Background(), Options{DSN: dsn, MaxConnections: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Migrate(context.Background(), 30*time.Second); err != nil {
		t.Fatal(err)
	}
	s.SetOwner(Owner{ID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Fence: 1})
	return s, dsn
}

func fixture(n int) storage.Record {
	now := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC).Add(time.Duration(n) * time.Millisecond)
	return storage.Record{RequestID: fmt.Sprintf("%032x", n), AttemptIndex: 1, StartedAt: now, AppName: "bot", UserHash: "11111111111111111111111111111111",
		Operation: "generation", Outcome: "ok", ReservedCost: 100, RequestCount: 1, Day: now.Format("2006-01-02"), Timezone: "UTC",
		Quote: storage.Quote{EstimatedInput: 40, EstimatedOutput: 60, ReservedCost: 100}}
}

func completed(r storage.Record) storage.Record {
	in, out := int64(20), int64(10)
	r.FinishedAt = r.StartedAt.Add(time.Second)
	r.Input, r.Output, r.Cost = &in, &out, 49
	return r
}

func reservation(r storage.Record) storage.Reservation {
	day := time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)
	return storage.Reservation{RequestID: r.RequestID, AttemptIndex: r.AttemptIndex, AppName: r.AppName, Subject: r.UserHash, AdmittedAt: r.StartedAt, State: storage.ReservationReserved,
		Quote: storage.LimitQuote{Operation: "generation", EstimatedTokens: 100, ReservedCost: 100},
		Items: []storage.LimitItem{
			{Scope: "app", Dimension: "usd_per_day", WindowStart: day.UnixMicro(), WindowEnd: day.AddDate(0, 0, 1).UnixMicro(), Timezone: "UTC", Reserved: 100},
			{Scope: "user", Subject: r.UserHash, Dimension: "usd_per_day", WindowStart: day.UnixMicro(), WindowEnd: day.AddDate(0, 0, 1).UnixMicro(), Timezone: "UTC", Reserved: 100},
		},
		Rate: &storage.RateEvent{RequestID: r.RequestID, AttemptIndex: 1, AppName: r.AppName, Subject: r.UserHash, AdmittedAt: r.StartedAt, EstimatedTokens: 100, Active: true}}
}

func settlement(r storage.Record) storage.Settlement {
	tokens := int64(30)
	return storage.Settlement{RequestID: r.RequestID, AttemptIndex: r.AttemptIndex, Transition: storage.TransitionSettle, At: r.StartedAt.Add(time.Second), Settled: []int64{49, 49}, RateTokens: &tokens}
}

func count(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
