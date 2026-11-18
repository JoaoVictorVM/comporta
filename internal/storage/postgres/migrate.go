package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"time"
)

//go:embed migrations/0001_usage.sql
var usageSchema string

//go:embed migrations/0002_distributed.sql
var distributedSchema string

// ErrSchema reports a future version or a changed checksum. The database is
// left as it was.
var ErrSchema = errors.New("incompatible database version or migration checksum")

// ErrMigrationBusy means another process kept the migration lock for longer
// than the wait allowed.
var ErrMigrationBusy = errors.New("migration lock not acquired in time")

type migration struct {
	version int
	sql     string
}

// migrations is the ordered registry. An applied file is never edited.
func migrations() []migration {
	return []migration{{1, usageSchema}, {2, distributedSchema}}
}

func checksum(sql string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(sql))) }

// migrationLock is a fixed advisory key: every replica of every cluster that
// shares the database serializes its migration on it.
const migrationLock = 0x636f6d706f727461

// Migrate applies the pending migrations once, even when several replicas
// start together. It waits at most wait for the lock; each migration runs in
// its own transaction with its bookkeeping row.
func (s *Store) Migrate(ctx context.Context, wait time.Duration) error {
	if wait <= 0 {
		wait = 60 * time.Second
	}
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return ErrMigrationUnavailable
	}
	defer conn.Close()
	deadline := time.Now().Add(wait)
	for {
		var acquired bool
		if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, migrationLock).Scan(&acquired); err != nil {
			return ErrMigrationUnavailable
		}
		if acquired {
			break
		}
		if time.Now().After(deadline) {
			return ErrMigrationBusy
		}
		select {
		case <-ctx.Done():
			return ErrMigrationUnavailable
		case <-time.After(250 * time.Millisecond):
		}
	}
	defer conn.ExecContext(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationLock)
	return migrateLocked(ctx, conn, migrations())
}

// ErrMigrationUnavailable hides driver details behind a fixed message.
var ErrMigrationUnavailable = errors.New("migration failed: database unavailable; database left unchanged")

func migrateLocked(ctx context.Context, conn *sql.Conn, set []migration) error {
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations(version integer PRIMARY KEY, checksum text NOT NULL, applied_at bigint NOT NULL)`); err != nil {
		return ErrMigrationUnavailable
	}
	applied, err := appliedVersion(ctx, conn, set)
	if err != nil {
		return err
	}
	for _, m := range set[applied:] {
		tx, err := conn.BeginTx(ctx, nil)
		if err != nil {
			return ErrMigrationUnavailable
		}
		if _, err = tx.ExecContext(ctx, m.sql); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %04d failed: constraint validation failed; database left unchanged", m.version)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations VALUES($1,$2,$3)`, m.version, checksum(m.sql), nowMicros()); err != nil {
			tx.Rollback()
			return ErrMigrationUnavailable
		}
		if err = tx.Commit(); err != nil {
			return ErrMigrationUnavailable
		}
	}
	return nil
}

func appliedVersion(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}, set []migration) (int, error) {
	rows, err := q.QueryContext(ctx, `SELECT version,checksum FROM schema_migrations ORDER BY version`)
	if err != nil {
		return 0, ErrMigrationUnavailable
	}
	defer rows.Close()
	applied := 0
	for rows.Next() {
		var version int
		var sum string
		if err = rows.Scan(&version, &sum); err != nil {
			return 0, ErrMigrationUnavailable
		}
		if version != applied+1 || version > len(set) || checksum(set[version-1].sql) != sum {
			return 0, ErrSchema
		}
		applied = version
	}
	if rows.Err() != nil {
		return 0, ErrMigrationUnavailable
	}
	return applied, nil
}

// SchemaVersion returns the applied version, or ErrSchema for a database this
// release cannot use.
func (s *Store) SchemaVersion(ctx context.Context) (int, error) {
	var exists bool
	if err := s.db.QueryRowContext(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		return 0, ErrMigrationUnavailable
	}
	if !exists {
		return 0, nil
	}
	return appliedVersion(ctx, s.db, migrations())
}

// RequireCurrent refuses a database that is not fully migrated.
func (s *Store) RequireCurrent(ctx context.Context) error {
	version, err := s.SchemaVersion(ctx)
	if err != nil {
		return err
	}
	if version != len(migrations()) {
		return ErrSchema
	}
	return nil
}
