package sqlite

import (
	"context"
	"errors"
	"math"
	"path/filepath"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/storage"
)

func TestAggregateOverflowRollsBackWithoutWrapping(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r := fixture()
	r.Cost = math.MaxInt64
	r.ReservedCost = math.MaxInt64
	if err := s.Admit(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(ctx, []storage.Record{r}); err != nil {
		t.Fatal(err)
	}
	r.RequestID = "22222222222222222222222222222222"
	r.Cost = 1
	r.ReservedCost = 1
	if err := s.Admit(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(ctx, []storage.Record{r}); !errors.Is(err, storage.ErrInvalid) {
		t.Fatalf("overflow classification: %v", err)
	}
	rows, err := s.Daily(ctx, storage.Filter{AppName: "bot"})
	if err != nil || len(rows) != 1 || rows[0].Cost != math.MaxInt64 || rows[0].Requests != 1 {
		t.Fatal("overflow changed totals")
	}
	pending, err := s.Pending(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatal("overflow lost admission")
	}
}

func TestWALAndFullSyncEveryConnection(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	for range 3 {
		conn, err := s.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range []struct {
			query string
			want  int
		}{{"PRAGMA synchronous", 2}, {"PRAGMA foreign_keys", 1}, {"PRAGMA busy_timeout", 1000}} {
			var n int
			if err = conn.QueryRowContext(ctx, p.query).Scan(&n); err != nil || n != p.want {
				t.Fatalf("pragma %s: %d %v", p.query, n, err)
			}
		}
		conn.Close()
		s.db.SetMaxIdleConns(0)
	}
	var version string
	if err := s.db.QueryRow(`SELECT sqlite_version()`).Scan(&version); err != nil || version != "3.54.1" {
		t.Fatalf("unexpected pinned SQLite: %s %v", version, err)
	}
	for range 4 {
		conn, err := s.readDB.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		for _, p := range []struct {
			query string
			want  int
		}{{"PRAGMA synchronous", 2}, {"PRAGMA foreign_keys", 1}, {"PRAGMA busy_timeout", 1000}, {"PRAGMA query_only", 1}} {
			var n int
			if err = conn.QueryRowContext(ctx, p.query).Scan(&n); err != nil || n != p.want {
				t.Fatalf("reader pragma %s: %d %v", p.query, n, err)
			}
		}
	}
}

func TestPrivateDatabaseArtifacts(t *testing.T) {
	s := testStore(t)
	r := fixture()
	if err := s.Admit(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"usage.sqlite", "usage.sqlite-wal", "usage.sqlite-shm", "usage.lock"} {
		f, err := openPrivate(filepath.Join(s.dir, name))
		if err != nil {
			t.Fatalf("artifact %s is not private: %v", name, err)
		}
		f.Close()
	}
}

func TestSQLInjectionValuesAreData(t *testing.T) {
	s := testStore(t)
	r := fixture()
	r.ModelRequested = `x'); DROP TABLE requests; --`
	ctx := context.Background()
	if err := s.Admit(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(ctx, []storage.Record{r}); err != nil {
		t.Fatal(err)
	}
	var got string
	if err := s.db.QueryRow(`SELECT model_requested FROM requests`).Scan(&got); err != nil || got != r.ModelRequested {
		t.Fatal("bound value changed SQL structure")
	}
}

func TestRetentionExactCutoffAndJournalNeverExpires(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r := fixture()
	if err := s.Admit(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(ctx, []storage.Record{r}); err != nil {
		t.Fatal(err)
	}
	if err := s.Retain(ctx, r.FinishedAt); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM requests`).Scan(&count); err != nil || count != 1 {
		t.Fatal("cutoff was inclusive")
	}
	r.RequestID = "22222222222222222222222222222222"
	if err := s.Admit(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.Retain(ctx, r.FinishedAt.AddDate(1, 0, 0)); err != nil {
		t.Fatal(err)
	}
	pending, err := s.Pending(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatal("retention deleted pending admission")
	}
}
