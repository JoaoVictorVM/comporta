package sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

func alertOccurrence(key string, at time.Time) storage.AlertOccurrence {
	return storage.AlertOccurrence{ID: storage.OccurrenceID(key), DedupeKey: key, Trigger: "loop_cut", OccurredAt: at, App: "bot", Scope: "app", Payload: []byte(`{"type":"loop_cut"}`)}
}

var twoChannels = []storage.AlertChannelRef{{Name: "ops", Type: "webhook"}, {Name: "chat", Type: "discord"}}

func TestAlertInputAtomicWithEvent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	materialize(t, s, fixture())
	if n := count(t, s, `SELECT count(*) FROM alert_inputs i JOIN events e ON e.event_id=i.event_id WHERE i.state='pending' AND e.type='request_completed'`); n != 1 {
		t.Fatalf("completion input %d", n)
	}
	paused := true
	pause := events.New(events.Paused, time.Now())
	pause.Payload.Paused, pause.Payload.Scope = &paused, "global"
	if err := s.Publish(ctx, pause); err != nil {
		t.Fatal(err)
	}
	failed := events.New(events.ConfigReloadFailed, time.Now())
	failed.Payload.Result = "rejected"
	if err := s.Publish(ctx, failed); err != nil {
		t.Fatal(err)
	}
	if err := s.Publish(ctx, failed); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT count(*) FROM alert_inputs`); n != 2 {
		t.Fatalf("inputs %d, want completion and reload only", n)
	}
	// A write that fails leaves neither the event nor its input.
	if _, err := s.db.Exec(`PRAGMA query_only=ON`); err != nil {
		t.Fatal(err)
	}
	other := events.New(events.ConfigReloadFailed, time.Now())
	other.Payload.Result = "rejected"
	if err := s.Publish(ctx, other); err == nil {
		t.Fatal("read-only publish acknowledged")
	}
	if _, err := s.db.Exec(`PRAGMA query_only=OFF`); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT count(*) FROM alert_inputs WHERE event_id=?`, other.ID); n != 0 {
		t.Fatal("input written without its event")
	}
}

func TestOccurrenceAndDeliveriesAtomic(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for range 2 {
		e := events.New(events.ConfigReloadFailed, now)
		e.Payload.Result = "rejected"
		if err := s.Publish(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	inputs, err := s.ClaimAlertInputs(ctx, now, 30*time.Second, 100)
	if err != nil || len(inputs) != 2 || inputs[0].Event == nil || inputs[0].Event.Type != events.ConfigReloadFailed {
		t.Fatalf("claim %v %v", inputs, err)
	}
	occurrence := alertOccurrence("reload|global|q1", now)
	occurrence.Trigger, occurrence.App, occurrence.Scope = "config_reload_failed", "", "global"
	if err = s.CompleteAlertInput(ctx, inputs[0].EventID, "ffffffffffffffffffffffffffffffff", nil, nil, now); !errors.Is(err, storage.ErrClaimLost) {
		t.Fatalf("foreign token accepted: %v", err)
	}
	invalid := occurrence
	invalid.Trigger = "unknown"
	if err = s.CompleteAlertInput(ctx, inputs[0].EventID, inputs[0].ClaimToken, []storage.AlertOccurrence{occurrence, invalid}, twoChannels, now); !errors.Is(err, storage.ErrInvalid) {
		t.Fatalf("invalid occurrence accepted: %v", err)
	}
	if count(t, s, `SELECT count(*) FROM alert_occurrences`) != 0 || count(t, s, `SELECT count(*) FROM alert_inputs WHERE state='processed'`) != 0 {
		t.Fatal("partial write")
	}
	for _, in := range inputs {
		if err = s.CompleteAlertInput(ctx, in.EventID, in.ClaimToken, []storage.AlertOccurrence{occurrence}, twoChannels, now); err != nil {
			t.Fatal(err)
		}
	}
	if count(t, s, `SELECT count(*) FROM alert_occurrences`) != 1 || count(t, s, `SELECT count(*) FROM alert_deliveries WHERE state='pending' AND attempt_count=0`) != 2 ||
		count(t, s, `SELECT count(*) FROM alert_inputs WHERE state='processed'`) != 2 {
		t.Fatal("occurrence not deduplicated or deliveries not expanded once")
	}
	// A repeated key never gains a channel added later.
	if err = s.RecordAlertOccurrences(ctx, []storage.AlertOccurrence{occurrence}, append(twoChannels, storage.AlertChannelRef{Name: "late", Type: "slack"}), now); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT count(*) FROM alert_deliveries`); n != 2 {
		t.Fatalf("deliveries %d after repeat", n)
	}
}

func TestClaimExpiryAndToken(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	first, second := alertOccurrence("loop|bot|1", now), alertOccurrence("loop|bot|2", now.Add(time.Second))
	if err := s.RecordAlertOccurrences(ctx, []storage.AlertOccurrence{first}, twoChannels[:1], now); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordAlertOccurrences(ctx, []storage.AlertOccurrence{second}, twoChannels[:1], now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	at := now.Add(2 * time.Second)
	claimed, err := s.ClaimAlertDeliveries(ctx, at, 30*time.Second, 10)
	if err != nil || len(claimed) != 1 || claimed[0].OccurrenceID != first.ID || string(claimed[0].Occurrence.Payload) != string(first.Payload) {
		t.Fatalf("first claim %v %v", claimed, err)
	}
	if again, err := s.ClaimAlertDeliveries(ctx, at, 30*time.Second, 10); err != nil || len(again) != 0 {
		t.Fatal("second delivery claimed while the channel is busy")
	}
	later := at.Add(31 * time.Second)
	taken, err := s.ClaimAlertDeliveries(ctx, later, 30*time.Second, 10)
	if err != nil || len(taken) != 1 || taken[0].ID != claimed[0].ID || taken[0].ClaimToken == claimed[0].ClaimToken {
		t.Fatal("expired claim not taken over")
	}
	if err = s.FinishAlertDelivery(ctx, claimed[0].ID, claimed[0].ClaimToken, storage.AlertResult{Outcome: storage.AlertDelivered, Status: 200, Error: "success", Sent: true}, later); !errors.Is(err, storage.ErrClaimLost) {
		t.Fatalf("stale token finished delivery: %v", err)
	}
	// Four calls at most: three retries, then a terminal failure.
	token, id := taken[0].ClaimToken, taken[0].ID
	clock := later
	for attempt := 1; attempt <= storage.MaxAlertAttempts; attempt++ {
		retry := storage.AlertResult{Outcome: storage.AlertRetry, Status: 503, Error: "server_error", RetryAt: clock.Add(time.Second), Sent: true}
		if err = s.FinishAlertDelivery(ctx, id, token, retry, clock); err != nil {
			t.Fatal(err)
		}
		clock = clock.Add(2 * time.Second)
		if attempt == storage.MaxAlertAttempts {
			break
		}
		next, err := s.ClaimAlertDeliveries(ctx, clock, 30*time.Second, 10)
		if err != nil || len(next) != 1 || next[0].ID != id || next[0].Attempts != attempt {
			t.Fatalf("retry %d not claimable: %v %v", attempt, next, err)
		}
		token = next[0].ClaimToken
	}
	if n := count(t, s, `SELECT count(*) FROM alert_deliveries WHERE delivery_id=? AND state='failed' AND attempt_count=4 AND last_error='server_error' AND finished_at IS NOT NULL`, id); n != 1 {
		t.Fatal("delivery not failed after four calls")
	}
	next, err := s.ClaimAlertDeliveries(ctx, clock, 30*time.Second, 10)
	if err != nil || len(next) != 1 || next[0].OccurrenceID != second.ID {
		t.Fatal("next delivery of the channel not released")
	}
	if err = s.ReleaseAlertDelivery(ctx, next[0].ID, next[0].ClaimToken); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT count(*) FROM alert_deliveries WHERE delivery_id=? AND state='pending' AND attempt_count=0`, next[0].ID); n != 1 {
		t.Fatal("release consumed an attempt")
	}
}

func TestAlertRetention(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	old := time.Now().UTC().AddDate(0, 0, -40)
	done, open := alertOccurrence("loop|bot|old", old), alertOccurrence("loop|bot|open", old)
	budget := alertOccurrence("budget|bot|month", old)
	end := time.Now().UTC().Add(24 * time.Hour)
	budget.Trigger, budget.Dimension, budget.WindowEnd, budget.ThresholdPercent = "budget_threshold", "usd_per_month", &end, 80
	for _, o := range []storage.AlertOccurrence{done, open, budget} {
		if err := s.RecordAlertOccurrences(ctx, []storage.AlertOccurrence{o}, twoChannels[:1], old); err != nil {
			t.Fatal(err)
		}
	}
	for _, o := range []storage.AlertOccurrence{done, budget} {
		if _, err := s.db.Exec(`UPDATE alert_deliveries SET state='delivered',delivered_at=?,finished_at=? WHERE occurrence_id=?`, old.UnixMicro(), old.UnixMicro(), o.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`INSERT INTO usage_5m VALUES(?,?,?,?),(?,?,?,?)`, (old.UnixMicro()/300_000_000)*300_000_000, "bot", 5, 0, (time.Now().UnixMicro()/300_000_000)*300_000_000, "bot", 7, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.RetainAlerts(ctx, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if n := count(t, s, `SELECT count(*) FROM alert_occurrences WHERE occurrence_id=?`, done.ID); n != 0 {
		t.Fatal("finished occurrence kept")
	}
	if count(t, s, `SELECT count(*) FROM alert_occurrences WHERE occurrence_id=?`, open.ID) != 1 || count(t, s, `SELECT count(*) FROM alert_deliveries WHERE state='pending'`) != 1 {
		t.Fatal("pending delivery removed")
	}
	if count(t, s, `SELECT count(*) FROM alert_occurrences WHERE occurrence_id=?`, budget.ID) != 1 {
		t.Fatal("occurrence of an open window removed")
	}
	if n := count(t, s, `SELECT count(*) FROM usage_5m`); n != 1 {
		t.Fatalf("usage buckets %d", n)
	}
}

func TestUsageFiveMinuteExactlyOnce(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r := fixture()
	materialize(t, s, r)
	done := completed(r)
	if err := s.Commit(ctx, []storage.Record{done}); err != nil {
		t.Fatal(err)
	}
	blocked := fixture()
	blocked.RequestID, blocked.AttemptIndex, blocked.Outcome, blocked.Cost, blocked.ReservedCost, blocked.RequestCount = fmt.Sprintf("%032x", 9), 0, "blocked", 0, 0, 0
	if err := s.Commit(ctx, []storage.Record{blocked}); err != nil {
		t.Fatal(err)
	}
	from := r.FinishedAt.Add(-time.Hour)
	buckets, err := s.UsageBuckets(ctx, from, r.FinishedAt.Add(time.Hour))
	if err != nil || len(buckets) != 1 || buckets[0].Cost != 49 || buckets[0].App != "bot" || buckets[0].Start.UnixMicro()%300_000_000 != 0 {
		t.Fatalf("buckets %v %v", buckets, err)
	}
}

func TestAlertMigrationBackfillsRetainedRequests(t *testing.T) {
	migrateTarget = 5
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
	if n := count(t, s, `SELECT count(*) FROM usage_5m WHERE app_name='bot' AND cost_microusd=49`); n != 1 {
		t.Fatalf("backfill rows %d", n)
	}
	if n := count(t, s, `SELECT count(*) FROM alert_inputs`); n != 0 {
		t.Fatal("historical events enqueued for alerts")
	}
}

func TestAlertBudgetsFoldLedgerAfterCheckpoint(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	m := limitManager(t, s)
	r := fixture()
	h, err := admitLimited(m, s, r)
	if err != nil {
		t.Fatal(err)
	}
	if err = settleCompleted(m, s, h, completed(r)); err != nil {
		t.Fatal(err)
	}
	used, _ := budget(t, m)
	check := func(stage string) {
		t.Helper()
		app, err := s.AlertBudgets(ctx, "bot", "app", "", false, time.Now())
		found := false
		for _, b := range app {
			found = found || (b.Dimension == "usd_per_day" && b.Spent == used)
		}
		if err != nil || !found || used != 49 {
			t.Fatalf("%s: app budgets %v %v, want %d", stage, app, err, used)
		}
	}
	check("before checkpoint")
	if err = m.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	check("after checkpoint")
	if outside, err := s.AlertBudgets(ctx, "bot", "app", "", false, time.Now().AddDate(0, 2, 0)); err != nil || len(outside) != 0 {
		t.Fatal("budget outside its window")
	}
}
