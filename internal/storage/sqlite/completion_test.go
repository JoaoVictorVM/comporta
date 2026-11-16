package sqlite

import (
	"context"
	"fmt"
	"sync"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/storage"
)

func TestConcurrentCompletionsAcknowledgeDurableResults(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	records := make([]storage.Record, 100)
	for i := range records {
		r := fixture()
		r.RequestID = fmt.Sprintf("%032x", i+1)
		records[i] = r
		if err := s.Admit(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	var workers sync.WaitGroup
	for _, r := range records {
		workers.Go(func() {
			if err := s.Complete(ctx, r); err != nil {
				t.Error(err)
			}
		})
	}
	workers.Wait()
	pending, err := s.Pending(ctx)
	if err != nil || len(pending) != len(records) {
		t.Fatal("acknowledged results missing")
	}
	for _, p := range pending {
		if p.Result == nil || p.Result.Cost != 49 {
			t.Fatal("completion acknowledged before durable result")
		}
	}
	if err = s.Commit(ctx, records); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Daily(ctx, storage.Filter{AppName: "bot"})
	if err != nil || len(rows) != 1 || rows[0].Requests != 100 || rows[0].Cost != 4900 {
		t.Fatal("completion batch changed totals")
	}
}

func TestCompletionCommitFailureRetainsAdmission(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r := fixture()
	if err := s.Admit(ctx, r); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`PRAGMA query_only=ON`); err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(ctx, r); err == nil {
		t.Fatal("failed completion acknowledged")
	}
	pending, err := s.Pending(ctx)
	if err != nil || len(pending) != 1 || pending[0].Result != nil {
		t.Fatal("failed completion lost or changed admission")
	}
	if _, err = s.db.Exec(`PRAGMA query_only=OFF`); err != nil {
		t.Fatal(err)
	}
	if err = s.Complete(ctx, r); err != nil {
		t.Fatal(err)
	}
}

func TestCachedCompletionAndEventsIdempotent(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r := fixture()
	if err := s.Admit(ctx, r); err != nil {
		t.Fatal(err)
	}
	zero := int64(0)
	r.Outcome, r.Cost, r.UsageMissing = "cached", 0, false
	r.Input, r.Output, r.CacheWrite, r.CacheRead, r.UpstreamStatus = &zero, &zero, &zero, &zero, nil
	if err := s.Complete(ctx, r); err != nil {
		t.Fatal(err)
	}
	// The worker may retry a batch whose commit was already durable, and a
	// restart recovers the same result from the journal.
	if err := s.Commit(ctx, []storage.Record{r}); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(ctx, []storage.Record{r}); err != nil {
		t.Fatal(err)
	}
	if err := s.Recover(ctx, r.FinishedAt); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	rows, err := s.db.Query(`SELECT type FROM events WHERE request_id=?`, r.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			t.Fatal(err)
		}
		counts[kind]++
	}
	_ = rows.Close()
	if counts["request_completed"] != 1 || counts["cached"] != 1 || len(counts) != 2 {
		t.Fatalf("events of a hit: %v", counts)
	}
	daily, err := s.Daily(ctx, storage.Filter{AppName: "bot"})
	if err != nil || len(daily) != 1 || daily[0].Requests != 1 || daily[0].Cost != 0 || daily[0].Input != 0 {
		t.Fatalf("daily after a hit: %+v %v", daily, err)
	}
}
