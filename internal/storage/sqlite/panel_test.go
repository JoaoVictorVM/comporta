package sqlite

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// panelRecord is a finished attempt at a fixed offset from base.
func panelRecord(n int, base time.Time, app, provider, model, outcome, class string, latencyMS int64) storage.Record {
	r := fixture()
	r.RequestID = fmt.Sprintf("%032x", n)
	r.StartedAt = base.Add(time.Duration(n) * time.Second)
	r.FinishedAt = r.StartedAt.Add(time.Duration(latencyMS) * time.Millisecond)
	r.AppName, r.Provider, r.ModelUsed, r.ModelRequested, r.Outcome, r.ErrorClass = app, provider, model, model, outcome, class
	r.LatencyUS = latencyMS * 1000
	r.Day = r.StartedAt.Format(time.DateOnly)
	if outcome == "downgraded" {
		r.PolicyOutcome, r.PolicyLimitName, r.PolicyScope, r.PolicyReason = "downgraded", "apps."+app+".budget.usd_per_day", "app", "spent"
		r.DowngradeSaved = 300
	}
	if outcome == "cached" {
		r.Cost, r.CacheSaved = 0, r.ReservedCost
	}
	return r
}

func commitPanel(t *testing.T, s *Store, records ...storage.Record) {
	t.Helper()
	ctx := context.Background()
	for _, r := range records {
		if err := s.Admit(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Commit(ctx, records); err != nil {
		t.Fatal(err)
	}
}

func TestPanelQueriesSQLite(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	base := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Second)
	var records []storage.Record
	for n := 1; n <= 20; n++ {
		class := ""
		if n%5 == 0 {
			class = "overload"
		}
		if n == 7 {
			class = "invalid_request"
		}
		records = append(records, panelRecord(n, base, "bot", "anthropic", "claude-a", "ok", class, int64(n*10)))
	}
	records = append(records, panelRecord(21, base, "bot", "anthropic", "claude-b", "downgraded", "", 50), panelRecord(22, base, "web", "openai", "gpt-x", "cached", "", 1))
	commitPanel(t, s, records...)

	page, err := s.PanelRequests(ctx, storage.PanelRequestFilter{Limit: 5})
	if err != nil || len(page.Records) != 5 || page.Next == nil || page.Records[0].RequestID != fmt.Sprintf("%032x", 22) {
		t.Fatalf("first page: %v %+v", err, page.Next)
	}
	seen := map[string]bool{}
	cursor := page.Next
	for _, r := range page.Records {
		seen[r.RequestID] = true
	}
	for cursor != nil {
		next, err := s.PanelRequests(ctx, storage.PanelRequestFilter{Limit: 5, Before: cursor})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range next.Records {
			if seen[r.RequestID] {
				t.Fatal("cursor repeated a row")
			}
			seen[r.RequestID] = true
		}
		cursor = next.Next
	}
	if len(seen) != 22 {
		t.Fatalf("pages covered %d rows", len(seen))
	}
	for _, f := range []storage.PanelRequestFilter{{App: "web"}, {Outcome: "cached"}, {Model: "gpt-x"}, {App: "web", UserHash: records[21].UserHash}} {
		page, err := s.PanelRequests(ctx, f)
		if err != nil || len(page.Records) != 1 || page.Records[0].AppName != "web" {
			t.Fatalf("filter %+v: %v %d", f, err, len(page.Records))
		}
	}
	if _, err := s.PanelRequests(ctx, storage.PanelRequestFilter{UserHash: records[0].UserHash}); err == nil {
		t.Fatal("a user hash without its app was accepted")
	}
	if _, err := s.PanelRequests(ctx, storage.PanelRequestFilter{Outcome: "deleted"}); err == nil {
		t.Fatal("an unknown outcome was accepted")
	}
	if r, ok, err := s.PanelRequest(ctx, records[20].RequestID, 1); err != nil || !ok || r.DowngradeSaved != 300 {
		t.Fatalf("detail: %v %v %+v", err, ok, r.DowngradeSaved)
	}
	if _, ok, err := s.PanelRequest(ctx, fmt.Sprintf("%032x", 99), 1); err != nil || ok {
		t.Fatal("a missing request was found")
	}

	mix, err := s.PanelModelMix(ctx, "bot", base.Add(-time.Hour), time.Now().Add(time.Hour))
	if err != nil || len(mix) != 2 || mix[0].Model != "claude-a" || mix[0].Attempts != 20 || mix[1].Attempts != 1 {
		t.Fatalf("model mix: %v %+v", err, mix)
	}
	top, err := s.PanelTopUsers(ctx, "bot", "2000-01-01", "2999-12-31")
	if err != nil || len(top) != 1 || top[0].Hash != records[0].UserHash || top[0].Requests != 21 {
		t.Fatalf("top users: %v %+v", err, top)
	}
	daily, err := s.QueryDaily(ctx, storage.UsageSelector{Scope: "app", From: "2000-01-01", Through: "2999-12-31"})
	if err != nil {
		t.Fatal(err)
	}
	var cacheSaved, downgradeSaved int64
	for _, d := range daily {
		cacheSaved += int64(d.CacheSaved)
		downgradeSaved += int64(d.DowngradeSaved)
	}
	if cacheSaved != 100 || downgradeSaved != 300 {
		t.Fatalf("daily savings: cache %d downgrade %d", cacheSaved, downgradeSaved)
	}

	stats, buckets, err := s.PanelProviders(ctx, base.Add(-time.Minute), time.Now().Add(time.Minute))
	if err != nil || len(stats) != 1 {
		t.Fatalf("providers: %v %+v", err, stats)
	}
	a := stats[0]
	// 21 attempts reached anthropic: 4 overloads count against health, the
	// client error does not; the cache hit never reached openai.
	if a.Provider != "anthropic" || a.Attempts != 21 || a.Failures != 4 || a.Successes != 16 {
		t.Fatalf("anthropic stats: %+v", a)
	}
	// Latencies 10..200 ms plus one of 50 ms: nearest rank of 21 values.
	if a.P50LatencyUS != 100_000 || a.P95LatencyUS != 190_000 {
		t.Fatalf("percentiles: p50 %d p95 %d", a.P50LatencyUS, a.P95LatencyUS)
	}
	var total int64
	for _, b := range buckets {
		if b.Provider != "anthropic" || b.Index < 0 || b.Index >= storage.PanelBuckets {
			t.Fatalf("bucket: %+v", b)
		}
		total += b.Attempts
	}
	if total != 21 {
		t.Fatalf("buckets hold %d attempts", total)
	}
}

func TestPanelEventsNewestFirst(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	base := time.Now().UTC().Add(-time.Hour)
	for n := 0; n < 7; n++ {
		e := events.New(events.ConfigChanged, base.Add(time.Duration(n)*time.Minute))
		e.Payload.Generation, e.Payload.Result = int64(n+1), "applied"
		if err := s.Publish(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.PanelEvents(ctx, storage.PanelEventFilter{Types: []events.Type{events.ConfigChanged}, Limit: 3})
	if err != nil || len(page.Events) != 3 || page.Next == nil || page.Events[0].Payload.Generation != 7 {
		t.Fatalf("first page: %v %+v", err, page)
	}
	page, err = s.PanelEvents(ctx, storage.PanelEventFilter{Types: []events.Type{events.ConfigChanged}, Limit: 3, Before: page.Next})
	if err != nil || len(page.Events) != 3 || page.Events[0].Payload.Generation != 4 {
		t.Fatalf("second page: %v %+v", err, page)
	}
	if page, err = s.PanelEvents(ctx, storage.PanelEventFilter{Types: []events.Type{events.Paused}}); err != nil || len(page.Events) != 0 {
		t.Fatal("type filter ignored")
	}
}

func TestPanelWatermark(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	first, err := s.PanelWatermark(ctx)
	if err != nil {
		t.Fatal(err)
	}
	commitPanel(t, s, panelRecord(1, time.Now().UTC().Add(-time.Minute), "bot", "anthropic", "claude-a", "ok", "", 10))
	second, err := s.PanelWatermark(ctx)
	if err != nil || second == first {
		t.Fatalf("watermark did not move: %+v %+v %v", first, second, err)
	}
	// A late write with an older timestamp still moves it.
	commitPanel(t, s, panelRecord(2, time.Now().UTC().Add(-5*time.Minute), "bot", "anthropic", "claude-a", "ok", "", 10))
	third, err := s.PanelWatermark(ctx)
	if err != nil || third == second {
		t.Fatalf("late write not seen: %+v %+v", second, third)
	}
}

// TestPanelQueryPlans keeps the filtered lists on an index instead of a scan
// of the whole table.
func TestPanelQueryPlans(t *testing.T) {
	s := testStore(t)
	top := storage.RecordCursor{StartedAt: time.UnixMicro(1<<63 - 1)}
	for name, f := range map[string]storage.PanelRequestFilter{
		"outcome": {Outcome: "cached"},
		"model":   {Model: "claude-a"},
		"recent":  {},
	} {
		query, args := panelRequestsQuery(f, top, storage.PanelPageSize)
		rows, err := s.db.Query(`EXPLAIN QUERY PLAN `+query, args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		rows.Close()
		text := strings.Join(plan, "; ")
		if !strings.Contains(text, "USING INDEX") || strings.Contains(text, "TEMP B-TREE") {
			t.Fatalf("%s plan does not use an index in order: %s", name, text)
		}
	}
}

func TestMigration0005PreservesHistoryAndDefaultsSavings(t *testing.T) {
	migrateTarget = 4
	dir := filepath.Join(t.TempDir(), "private")
	s, err := Open(context.Background(), dir)
	if err != nil {
		migrateTarget = 0
		t.Fatal(err)
	}
	ctx := context.Background()
	old := fixture()
	if err = s.Admit(ctx, old); err != nil {
		t.Fatal(err)
	}
	if err = s.Commit(ctx, []storage.Record{old}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	migrateTarget = 0
	s, err = Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v, err := checkSchema(ctx, s.db); err != nil || v != 5 {
		t.Fatalf("version %d %v", v, err)
	}
	if n := count(t, s, `SELECT count(*) FROM requests WHERE cache_saved_microusd=0 AND downgrade_saved_microusd=0`); n != 1 {
		t.Fatalf("history not kept with zero savings: %d", n)
	}
	if n := count(t, s, `SELECT count(*) FROM usage_daily WHERE cache_saved_microusd=0 AND downgrade_saved_microusd=0 AND cost_microusd=49`); n != 2 {
		t.Fatalf("aggregates not kept: %d", n)
	}
	if _, err = s.db.Exec(`UPDATE usage_daily SET cache_saved_microusd=-1`); err == nil {
		t.Fatal("a negative saving was accepted")
	}
	r, ok, err := s.PanelRequest(ctx, old.RequestID, 1)
	if err != nil || !ok || r.CacheSaved != 0 || r.DowngradeSaved != 0 {
		t.Fatalf("old record: %v %v", err, ok)
	}
}
