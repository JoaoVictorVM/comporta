package sqlite

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
)

func TestMigrationFailureRollsBackWholeSet(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	err := migrateSet(ctx, s.db, []migration{{1, schema}, {2, `CREATE TABLE staged(value INTEGER) STRICT;`}, {3, `THIS IS NOT SQL`}})
	if err == nil {
		t.Fatal("migration accepted invalid SQL")
	}
	var n int
	if err = s.db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE name='staged'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("partial schema survived: %d %v", n, err)
	}
	if err = s.db.QueryRow(`SELECT max(version) FROM schema_migrations`).Scan(&n); err != nil || n != 1 {
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
	if err := migrateSet(ctx, s.db, []migration{{1, schema}, {2, `CREATE TABLE next_release(value INTEGER) STRICT;`}}); err != nil {
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
