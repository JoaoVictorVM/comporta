package sqlite

import (
	"context"
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/useridentity"
)

func TestMigrationFailureRollsBackWholeSet(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	err := migrateSet(ctx, s.db, append(migrations(), migration{3, `CREATE TABLE staged(value INTEGER) STRICT;`}, migration{4, `THIS IS NOT SQL`}))
	if err == nil {
		t.Fatal("migration accepted invalid SQL")
	}
	var n int
	if err = s.db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name='staged'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("partial schema survived: %d %v", n, err)
	}
	if err = s.db.QueryRow(`SELECT max(version) FROM schema_migrations`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("partial version survived: %d %v", n, err)
	}
}

func TestSyntheticPreviousSchemaMigration(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	r := fixture()
	if err := s.Admit(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := migrateSet(ctx, s.db, append(migrations(), migration{3, `CREATE TABLE next_release(value INTEGER) STRICT;`})); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.db.QueryRow(`SELECT count(*) FROM metering_journal`).Scan(&n); err != nil || n != 1 {
		t.Fatal("migration changed journal")
	}
	if err := migrate(ctx, s.db); err == nil {
		t.Fatal("future version accepted")
	}
}

func TestMigrationChecksumMismatchRefused(t *testing.T) {
	s := testStore(t)
	if _, err := s.db.Exec(`UPDATE schema_migrations SET checksum=?`, fmt.Sprintf("%x", sha256.Sum256([]byte("different")))); err != nil {
		t.Fatal(err)
	}
	if err := migrate(context.Background(), s.db); err == nil {
		t.Fatal("checksum accepted")
	}
}

// schemaOneStore builds a database exactly as the previous release left it,
// with usage, a journal entry and a configuration event.
func schemaOneStore(t *testing.T) string {
	t.Helper()
	migrateTarget = 1
	defer func() { migrateTarget = 0 }()
	dir := filepath.Join(t.TempDir(), "private")
	s, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	done := fixture()
	if err = s.Admit(ctx, done); err != nil {
		t.Fatal(err)
	}
	if err = s.Commit(ctx, []storage.Record{done}); err != nil {
		t.Fatal(err)
	}
	pending := fixture()
	pending.RequestID = "fedcba9876543210fedcba9876543210"
	if err = s.Admit(ctx, pending); err != nil {
		t.Fatal(err)
	}
	e := events.New(events.ConfigChanged, time.Now())
	e.Payload.Generation, e.Payload.Result = 1, config.ResultInitial
	if err = s.Publish(ctx, e); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestMigrateSchemaOneToTwoPreservesUsage(t *testing.T) {
	dir := schemaOneStore(t)
	s, err := Open(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v, err := checkSchema(context.Background(), s.db); err != nil || v != 2 {
		t.Fatalf("version %d %v", v, err)
	}
	for query, want := range map[string]int{
		`SELECT count(*) FROM requests`:                                      1,
		`SELECT count(*) FROM usage_daily`:                                   2,
		`SELECT count(*) FROM metering_journal`:                              1,
		`SELECT count(*) FROM events`:                                        2,
		`SELECT count(*) FROM config_changes`:                                1,
		`SELECT count(*) FROM schema_migrations WHERE checksum<>''`:          2,
		`SELECT count(*) FROM sqlite_schema WHERE name LIKE '%_v2'`:          0,
		`SELECT count(*) FROM pragma_foreign_key_check`:                      0,
		`SELECT count(*) FROM sqlite_schema WHERE name='ix_events_app_type'`: 1,
	} {
		if got := count(t, s, query); got != want {
			t.Fatalf("%s = %d, want %d", query, got, want)
		}
	}
	if _, err = s.db.Exec(`DELETE FROM events WHERE type='config_changed'`); err != nil {
		t.Fatal(err)
	}
	if count(t, s, `SELECT count(*) FROM config_changes`) != 0 {
		t.Fatal("config_changes lost its cascade to events")
	}
	degraded := events.New(events.LimitStateUnavailable, time.Now())
	degraded.Payload.Severity = "warning"
	if err = s.Publish(context.Background(), degraded); err != nil {
		t.Fatalf("new event type refused after migration: %v", err)
	}
	if err = s.Recover(context.Background(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if rows, err := s.Daily(context.Background(), storage.Filter{AppName: "bot"}); err != nil || len(rows) != 1 || rows[0].Cost != 149 {
		t.Fatalf("usage after upgrade: %+v %v", rows, err)
	}
}

func TestReadOnlyIdentityInspectionAcceptsKnownPrefix(t *testing.T) {
	dir := schemaOneStore(t)
	s, err := Prepare(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	state, err := s.Inspect(context.Background())
	if err != nil || state != useridentity.HistoryPresent {
		t.Fatalf("schema 0001 inspection: %v %v", state, err)
	}
	var version int
	if err = s.db.QueryRow(`SELECT max(version) FROM schema_migrations`).Scan(&version); err != nil || version != 1 {
		t.Fatal("inspection migrated the database")
	}
}

func TestLimitsMigrationRollback(t *testing.T) {
	dir := schemaOneStore(t)
	migrateTarget = 1
	s, err := Open(context.Background(), dir)
	migrateTarget = 0
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = migrateSet(context.Background(), s.db, append(migrations(), migration{3, `THIS IS NOT SQL`})); err == nil {
		t.Fatal("broken migration accepted")
	}
	if count(t, s, `SELECT max(version) FROM schema_migrations`) != 1 || count(t, s, `SELECT count(*) FROM sqlite_schema WHERE name LIKE 'limit_%'`) != 0 {
		t.Fatal("migration 0002 survived a later failure")
	}
	if count(t, s, `SELECT count(*) FROM config_changes`) != 1 || count(t, s, `SELECT count(*) FROM events`) != 2 {
		t.Fatal("rollback lost events")
	}
}

func TestFutureSchemaRejected(t *testing.T) {
	s := testStore(t)
	if _, err := s.db.Exec(`INSERT INTO schema_migrations VALUES(3,'unknown',0)`); err != nil {
		t.Fatal(err)
	}
	if _, err := checkSchema(context.Background(), s.db); err == nil {
		t.Fatal("future schema inspected as supported")
	}
	if err := migrate(context.Background(), s.db); err == nil {
		t.Fatal("future schema migrated")
	}
}
