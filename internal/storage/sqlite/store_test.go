package sqlite

import (
	"context"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"path/filepath"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "private"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func fixture() storage.Record {
	now := time.Now().UTC()
	n := int64(10)
	return storage.Record{RequestID: "0123456789abcdef0123456789abcdef", AttemptIndex: 1, StartedAt: now, FinishedAt: now, AppName: "bot", UserHash: "11111111111111111111111111111111", Operation: "generation", Outcome: "ok", Input: &n, Cost: 49, ReservedCost: 100, RequestCount: 1, Day: now.Format("2006-01-02"), Timezone: "UTC"}
}

func TestJournalRecoveryAndRetention(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r := fixture()
	if err := s.Admit(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.Complete(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.Recover(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.Recover(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.Retain(ctx, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	d, err := s.Daily(ctx, storage.Filter{AppName: "bot"})
	if err != nil || len(d) != 1 || d[0].Cost != 49 || d[0].Requests != 1 {
		t.Fatalf("daily=%+v err=%v", d, err)
	}
	var count int
	if err = s.db.QueryRow(`SELECT count(*) FROM requests`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("count=%d err=%v", count, err)
	}
}

func TestAdmissionOnlyRecoversConservativeCost(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r := fixture()
	if err := s.Admit(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.Recover(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	d, err := s.Daily(ctx, storage.Filter{AppName: "bot"})
	if err != nil || len(d) != 1 || d[0].Cost != 100 || d[0].Recovered != 1 {
		t.Fatalf("%+v %v", d, err)
	}
}
