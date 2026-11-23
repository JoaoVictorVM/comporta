//go:build distributed

package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/storage/sqlite"
)

// contractStore is what both backends must answer identically.
type contractStore interface {
	storage.LimitStore
	storage.CompletionStore
	storage.UsageReader
	storage.UsageQueryStore
}

func exercise(t *testing.T, s contractStore) ([]storage.Daily, storage.RecordPage) {
	t.Helper()
	ctx := context.Background()
	var records []storage.Record
	for n := 1; n <= 3; n++ {
		r := fixture(n)
		if _, err := s.AdmitReserved(ctx, r, reservation(r)); err != nil {
			t.Fatal(err)
		}
		done := completed(r)
		if _, err := s.CompleteSettled(ctx, done, settlement(done)); err != nil {
			t.Fatal(err)
		}
		records = append(records, done)
	}
	if err := s.Commit(ctx, records); err != nil {
		t.Fatal(err)
	}
	daily, err := s.QueryDaily(ctx, storage.UsageSelector{Scope: "app", From: "2026-11-01", Through: "2026-11-30"})
	if err != nil {
		t.Fatal(err)
	}
	page, err := s.Records(ctx, storage.RecordFilter{AppName: "bot", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	return daily, page
}

func TestStoreContract(t *testing.T) {
	pg, _ := openTest(t)
	lite, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "private"))
	if err != nil {
		t.Fatal(err)
	}
	defer lite.Close()
	pgDaily, pgPage := exercise(t, pg)
	liteDaily, litePage := exercise(t, lite)
	if !reflect.DeepEqual(pgDaily, liteDaily) {
		t.Fatalf("daily aggregates differ:\npostgres %+v\nsqlite   %+v", pgDaily, liteDaily)
	}
	a, _ := json.Marshal(pgPage)
	b, _ := json.Marshal(litePage)
	if string(a) != string(b) {
		t.Fatalf("records differ:\npostgres %s\nsqlite   %s", a, b)
	}
	if count(t, pg, `SELECT count(*) FROM metering_journal`) != 0 {
		t.Fatal("materialized records stayed in the journal")
	}
}

func TestCommitIdempotentAggregates(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	r := fixture(7)
	if _, err := s.AdmitReserved(ctx, r, reservation(r)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AdmitReserved(ctx, r, reservation(r)); err != nil {
		t.Fatalf("an identical repeat must confirm: %v", err)
	}
	changed := reservation(r)
	changed.Quote.ReservedCost = 101
	if _, err := s.AdmitReserved(ctx, r, changed); !errors.Is(err, storage.ErrConflict) {
		t.Fatalf("a different payload with the same key must conflict, got %v", err)
	}
	done := completed(r)
	seq1, err := s.CompleteSettled(ctx, done, settlement(done))
	if err != nil {
		t.Fatal(err)
	}
	seq2, err := s.CompleteSettled(ctx, done, settlement(done))
	if err != nil || seq1 != seq2 {
		t.Fatalf("repeated settlement must return the same sequence: %d %d %v", seq1, seq2, err)
	}
	for range 3 {
		if err := s.Commit(ctx, []storage.Record{done}); err != nil {
			t.Fatal(err)
		}
	}
	daily, err := s.Daily(ctx, storage.Filter{AppName: "bot"})
	if err != nil || len(daily) != 1 || daily[0].Requests != 1 || daily[0].Cost != 49 {
		t.Fatalf("aggregates counted more than once: %+v %v", daily, err)
	}
	if count(t, s, `SELECT count(*) FROM limit_ledger`) != 2 {
		t.Fatal("ledger must hold one admit and one settle")
	}
}

func TestUsageCursorAcrossReplicas(t *testing.T) {
	a, dsn := openTest(t)
	b, err := Open(context.Background(), Options{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	ctx := context.Background()
	var records []storage.Record
	for n := 1; n <= 5; n++ {
		r := fixture(n)
		if err := a.Admit(ctx, r); err != nil {
			t.Fatal(err)
		}
		records = append(records, completed(r))
	}
	if err := a.Commit(ctx, records); err != nil {
		t.Fatal(err)
	}
	first, err := a.Records(ctx, storage.RecordFilter{AppName: "bot", Limit: 2})
	if err != nil || first.Next == nil {
		t.Fatal(err)
	}
	second, err := b.Records(ctx, storage.RecordFilter{AppName: "bot", Limit: 10, After: first.Next})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Records)+len(second.Records) != 5 || second.Records[0].RequestID != records[2].RequestID {
		t.Fatal("a cursor from one replica must continue on another")
	}
}

func TestRetentionKeepsOpenObligations(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	open := fixture(1)
	if _, err := s.AdmitReserved(ctx, open, reservation(open)); err != nil {
		t.Fatal(err)
	}
	old := fixture(2)
	if err := s.Admit(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(ctx, []storage.Record{completed(old)}); err != nil {
		t.Fatal(err)
	}
	if err := s.Retain(ctx, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	if count(t, s, `SELECT count(*) FROM requests`) != 0 {
		t.Fatal("old details must go")
	}
	if count(t, s, `SELECT count(*) FROM usage_daily`) == 0 {
		t.Fatal("aggregates must stay")
	}
	if count(t, s, `SELECT count(*) FROM metering_journal`) != 1 || count(t, s, `SELECT count(*) FROM limit_reservations WHERE state='reserved'`) != 1 {
		t.Fatal("retention removed an open obligation")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := FoldCheckpoint(ctx, tx, time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC), nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if count(t, s, `SELECT count(*) FROM limit_reservations WHERE state='reserved'`) != 1 || count(t, s, `SELECT count(*) FROM limit_ledger`) != 1 {
		t.Fatal("a checkpoint compacted an open reservation")
	}
	var reserved int64
	if err := s.db.QueryRow(`SELECT coalesce(sum(reserved),0) FROM budget_state WHERE scope='app'`).Scan(&reserved); err != nil || reserved != 100 {
		t.Fatalf("checkpoint must keep the open reservation charged: %d %v", reserved, err)
	}
}

// panelStore is the read side of the panel both backends must answer alike.
type panelStore interface {
	storage.PanelStore
	storage.AdmissionStore
	storage.CompletionStore
}

func exercisePanel(t *testing.T, s panelStore) []any {
	t.Helper()
	ctx := context.Background()
	var records []storage.Record
	for n := 1; n <= 12; n++ {
		r := fixture(n)
		r.Provider, r.ModelUsed, r.ModelRequested = "anthropic", "claude-a", "claude-a"
		if n%4 == 0 {
			r.ErrorClass = "timeout"
		}
		if n == 6 {
			r.Outcome, r.PolicyOutcome, r.PolicyLimitName, r.PolicyScope, r.PolicyReason = "downgraded", "downgraded", "apps.bot.budget.usd_per_day", "app", "spent"
			r.ModelUsed, r.DowngradeSaved = "claude-b", 40
		}
		if err := s.Admit(ctx, r); err != nil {
			t.Fatal(err)
		}
		done := completed(r)
		done.LatencyUS = int64(n) * 1000
		if n == 9 {
			done.Outcome, done.Cost, done.CacheSaved = "cached", 0, done.ReservedCost
		}
		records = append(records, done)
	}
	if err := s.Commit(ctx, records); err != nil {
		t.Fatal(err)
	}
	from, until := time.Date(2026, 11, 18, 11, 30, 0, 0, time.UTC), time.Date(2026, 11, 18, 12, 30, 0, 0, time.UTC)
	requests, err := s.PanelRequests(ctx, storage.PanelRequestFilter{App: "bot", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.PanelRequests(ctx, storage.PanelRequestFilter{App: "bot", Limit: 5, Before: requests.Next})
	if err != nil {
		t.Fatal(err)
	}
	cached, err := s.PanelRequests(ctx, storage.PanelRequestFilter{Outcome: "cached", Model: "claude-a"})
	if err != nil {
		t.Fatal(err)
	}
	mix, err := s.PanelModelMix(ctx, "bot", from, until)
	if err != nil {
		t.Fatal(err)
	}
	top, err := s.PanelTopUsers(ctx, "bot", "2026-11-01", "2026-11-30")
	if err != nil {
		t.Fatal(err)
	}
	stats, buckets, err := s.PanelProviders(ctx, from, until)
	if err != nil {
		t.Fatal(err)
	}
	detail, found, err := s.PanelRequest(ctx, records[5].RequestID, 1)
	if err != nil || !found {
		t.Fatal(err)
	}
	daily, err := s.QueryDaily(ctx, storage.UsageSelector{Scope: "app", From: "2026-11-01", Through: "2026-11-30"})
	if err != nil {
		t.Fatal(err)
	}
	return []any{requests, next, cached, mix, top, stats, buckets, detail, daily}
}

func TestPanelStoreContract(t *testing.T) {
	pg, _ := openTest(t)
	lite, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "private"))
	if err != nil {
		t.Fatal(err)
	}
	defer lite.Close()
	a, _ := json.Marshal(exercisePanel(t, pg))
	b, _ := json.Marshal(exercisePanel(t, lite))
	if string(a) != string(b) {
		t.Fatalf("panel reads differ:\npostgres %s\nsqlite   %s", a, b)
	}
	w1, err := pg.PanelWatermark(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r := fixture(50)
	r.StartedAt = time.Now().UTC()
	r.Day = r.StartedAt.Format(time.DateOnly)
	if err := pg.Admit(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if err := pg.Commit(context.Background(), []storage.Record{completed(r)}); err != nil {
		t.Fatal(err)
	}
	w2, err := pg.PanelWatermark(context.Background())
	if err != nil || w1 == w2 {
		t.Fatalf("watermark did not move: %+v %+v %v", w1, w2, err)
	}
}
