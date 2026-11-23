//go:build distributed

package postgres

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

func TestConcurrentMigrateOnce(t *testing.T) {
	dsn := testkit.Postgres(t)
	var wg sync.WaitGroup
	errs := make([]error, 6)
	for i := range errs {
		wg.Go(func() {
			s, err := Open(context.Background(), Options{DSN: dsn, MaxConnections: 2})
			if err != nil {
				errs[i] = err
				return
			}
			defer s.Close()
			errs[i] = s.Migrate(context.Background(), 30*time.Second)
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	s, err := Open(context.Background(), Options{DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var rows int
	if err := s.db.QueryRow(`SELECT count(*) FROM schema_migrations`).Scan(&rows); err != nil || rows != len(migrations()) {
		t.Fatalf("expected one row per migration, got %d (%v)", rows, err)
	}
	if err := s.RequireCurrent(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationChecksumMismatch(t *testing.T) {
	s, _ := openTest(t)
	if _, err := s.db.Exec(`UPDATE schema_migrations SET checksum='tampered' WHERE version=1`); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(context.Background(), time.Second); !errors.Is(err, ErrSchema) {
		t.Fatalf("a changed checksum must stop the start, got %v", err)
	}
	// The database is preserved: the tables written before are still there.
	if count(t, s, `SELECT count(*) FROM information_schema.tables WHERE table_name='requests'`) != 1 {
		t.Fatal("refused migration changed the database")
	}
}

func TestFutureSchemaRejected(t *testing.T) {
	s, _ := openTest(t)
	if _, err := s.db.Exec(`INSERT INTO schema_migrations VALUES(99,'future',0)`); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(context.Background(), time.Second); !errors.Is(err, ErrSchema) {
		t.Fatalf("a future version must stop the start, got %v", err)
	}
	if err := s.RequireCurrent(context.Background()); !errors.Is(err, ErrSchema) {
		t.Fatalf("require current: %v", err)
	}
}

func TestMigrationLockWaitIsBounded(t *testing.T) {
	s, _ := openTest(t)
	conn, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `SELECT pg_advisory_lock($1)`, migrationLock); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := s.Migrate(context.Background(), 500*time.Millisecond); !errors.Is(err, ErrMigrationBusy) {
		t.Fatalf("expected a bounded wait, got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("wait was not bounded")
	}
}

func TestPanelMigrationFromPreviousVersion(t *testing.T) {
	dsn := testkit.Postgres(t)
	s, err := Open(context.Background(), Options{DSN: dsn, MaxConnections: 4})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	conn, err := s.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateLocked(ctx, conn, migrations()[:2]); err != nil {
		t.Fatal(err)
	}
	conn.Close()
	if _, err := s.db.Exec(`INSERT INTO usage_daily VALUES('2026-11-18','UTC','bot','app','',false,1,1,10,5,0,0,0,0,49)`); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx, 30*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := s.RequireCurrent(ctx); err != nil {
		t.Fatal(err)
	}
	if count(t, s, `SELECT count(*) FROM usage_daily WHERE cost_microusd=49 AND cache_saved_microusd=0 AND downgrade_saved_microusd=0`) != 1 {
		t.Fatal("history not kept with zero savings")
	}
	if _, err := s.db.Exec(`UPDATE usage_daily SET downgrade_saved_microusd=-1`); err == nil {
		t.Fatal("a negative saving was accepted")
	}
}
