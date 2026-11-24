//go:build distributed

package postgres

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

func replica(t *testing.T, dsn string) *Store {
	t.Helper()
	s, err := Open(context.Background(), Options{DSN: dsn, MaxConnections: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.SetOwner(Owner{ID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", Fence: 1})
	return s
}

func loopOccurrence(key string, at time.Time) storage.AlertOccurrence {
	return storage.AlertOccurrence{ID: storage.OccurrenceID(key), DedupeKey: key, Trigger: "loop_cut", OccurredAt: at, App: "bot", Scope: "app", Payload: []byte(`{"type":"loop_cut"}`)}
}

func TestConcurrentClaimsAcrossReplicas(t *testing.T) {
	a, dsn := openTest(t)
	b := replica(t, dsn)
	ctx := context.Background()
	now := time.Now().UTC()
	for range 60 {
		e := events.New(events.ConfigReloadFailed, now)
		e.Payload.Result = "rejected"
		if err := a.Publish(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	var mu sync.Mutex
	seen := map[string]int{}
	var wg sync.WaitGroup
	for _, s := range []*Store{a, b, a, b} {
		wg.Go(func() {
			for {
				inputs, err := s.ClaimAlertInputs(ctx, now, 30*time.Second, 7)
				if err != nil {
					t.Error(err)
					return
				}
				if len(inputs) == 0 {
					return
				}
				mu.Lock()
				for _, in := range inputs {
					seen[in.EventID]++
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if len(seen) != 60 {
		t.Fatalf("claimed %d distinct inputs, want 60", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("input %s claimed %d times", id, n)
		}
	}
}

func TestOccurrenceRaceDeduplicates(t *testing.T) {
	a, dsn := openTest(t)
	b := replica(t, dsn)
	ctx := context.Background()
	now := time.Now().UTC()
	occurrence := loopOccurrence("loop|bot|race", now)
	channels := []storage.AlertChannelRef{{Name: "ops", Type: "webhook"}, {Name: "chat", Type: "slack"}}
	var wg sync.WaitGroup
	for i := range 20 {
		s := a
		if i%2 == 1 {
			s = b
		}
		wg.Go(func() {
			if err := s.RecordAlertOccurrences(ctx, []storage.AlertOccurrence{occurrence}, channels, now); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if count(t, a, `SELECT count(*) FROM alert_occurrences`) != 1 || count(t, a, `SELECT count(*) FROM alert_deliveries`) != 2 {
		t.Fatal("racing replicas duplicated the occurrence or its deliveries")
	}
}

func TestDeliveryOrderPerChannel(t *testing.T) {
	a, dsn := openTest(t)
	b := replica(t, dsn)
	ctx := context.Background()
	now := time.Now().UTC()
	channel := []storage.AlertChannelRef{{Name: "ops", Type: "webhook"}}
	var ids []string
	for i := range 3 {
		at := now.Add(time.Duration(i) * time.Millisecond)
		o := loopOccurrence("loop|bot|"+string(rune('a'+i)), at)
		if err := a.RecordAlertOccurrences(ctx, []storage.AlertOccurrence{o}, channel, at); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, o.ID)
	}
	at := now.Add(time.Second)
	for i, id := range ids {
		var wg sync.WaitGroup
		var mu sync.Mutex
		var claimed []storage.AlertDelivery
		for _, s := range []*Store{a, b} {
			wg.Go(func() {
				got, err := s.ClaimAlertDeliveries(ctx, at, 30*time.Second, 10)
				if err != nil {
					t.Error(err)
				}
				mu.Lock()
				claimed = append(claimed, got...)
				mu.Unlock()
			})
		}
		wg.Wait()
		if len(claimed) != 1 || claimed[0].OccurrenceID != id {
			t.Fatalf("round %d: claimed %v, want only %s", i, claimed, id)
		}
		if err := b.FinishAlertDelivery(ctx, claimed[0].ID, claimed[0].ClaimToken, storage.AlertResult{Outcome: storage.AlertDelivered, Status: 204, Error: "success", Sent: true}, at); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPostgresUsageFiveMinuteExactlyOnce(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	r := fixture(1)
	if _, err := s.AdmitReserved(ctx, r, reservation(r)); err != nil {
		t.Fatal(err)
	}
	done := completed(r)
	if _, err := s.CompleteSettled(ctx, done, settlement(done)); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.Commit(ctx, []storage.Record{done}); err != nil {
			t.Fatal(err)
		}
	}
	buckets, err := s.UsageBuckets(ctx, done.FinishedAt.Add(-time.Hour), done.FinishedAt.Add(time.Hour))
	if err != nil || len(buckets) != 1 || buckets[0].Cost != 49 {
		t.Fatalf("buckets %v %v", buckets, err)
	}
	if count(t, s, `SELECT count(*) FROM alert_inputs WHERE event_type='request_completed'`) != 1 {
		t.Fatal("completion input not written once with its event")
	}
	budgets, err := s.AlertBudgets(ctx, "bot", "app", "", false, r.StartedAt)
	if err != nil || len(budgets) != 1 || budgets[0].Spent != 49 {
		t.Fatalf("budgets %v %v", budgets, err)
	}
}

func TestPostgresMigrationConcurrentStart(t *testing.T) {
	_, dsn := openTest(t)
	var wg sync.WaitGroup
	for _, s := range []*Store{replica(t, dsn), replica(t, dsn), replica(t, dsn), replica(t, dsn)} {
		wg.Go(func() {
			if err := s.Migrate(context.Background(), 30*time.Second); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	s := replica(t, dsn)
	if count(t, s, `SELECT count(*) FROM pg_trigger WHERE tgname IN ('tr_alert_inputs','tr_usage_5m')`) != 2 {
		t.Fatal("alert triggers missing or duplicated")
	}
	if err := s.RequireCurrent(context.Background()); err != nil {
		t.Fatal(err)
	}
}
