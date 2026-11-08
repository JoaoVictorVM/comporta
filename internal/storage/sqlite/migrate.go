package sqlite

import (
	"context"
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"
)

//go:embed migrations/0001_usage.sql
var schema string

//go:embed migrations/0002_limits.sql
var limitsSchema string

var errSchema = errors.New("incompatible database version or migration checksum")

// migrations is the ordered registry. Applied files are never edited: a new
// change is a new version.
func migrations() []migration {
	return []migration{{1, schema}, {2, limitsSchema}}
}

// checkSchema accepts any intact prefix of the known migrations, so a database
// of an earlier release can be inspected read-only before it is migrated. It
// returns the applied version; a future version or a changed checksum fails.
func checkSchema(ctx context.Context, db *sql.DB) (int, error) {
	rows, err := db.QueryContext(ctx, `SELECT version,checksum FROM schema_migrations ORDER BY version`)
	if err != nil {
		return 0, errSchema
	}
	defer rows.Close()
	known := migrations()
	applied := 0
	for rows.Next() {
		var version int
		var checksum string
		if err = rows.Scan(&version, &checksum); err != nil || version != applied+1 || version > len(known) || checksum != fmt.Sprintf("%x", sha256.Sum256([]byte(known[version-1].sql))) {
			return 0, errSchema
		}
		applied = version
	}
	if rows.Err() != nil || applied == 0 {
		return 0, errSchema
	}
	return applied, nil
}

type migration struct {
	version int
	sql     string
}

// migrateTarget limits the applied versions; tests use it to build a database
// of an earlier release. Zero applies every known migration.
var migrateTarget = 0

func migrate(ctx context.Context, db *sql.DB) error {
	set := migrations()
	if migrateTarget > 0 {
		set = set[:migrateTarget]
	}
	err := migrateSet(ctx, db, set)
	if err == nil || errors.Is(err, errSchema) {
		return err
	}
	if strings.HasPrefix(err.Error(), "migration ") {
		return err
	}
	return errors.New("migration failed: database unavailable; database left unchanged")
}

func migrateSet(ctx context.Context, db *sql.DB, migrations []migration) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations(version INTEGER PRIMARY KEY, checksum TEXT NOT NULL, applied_at INTEGER NOT NULL) STRICT`); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT version,checksum FROM schema_migrations ORDER BY version`)
	if err != nil {
		return err
	}
	checksums := map[int]string{}
	for i, m := range migrations {
		if m.version != i+1 {
			return errors.New("invalid migration sequence")
		}
		checksums[m.version] = fmt.Sprintf("%x", sha256.Sum256([]byte(m.sql)))
	}
	applied := 0
	for rows.Next() {
		var version int
		var sum string
		if err = rows.Scan(&version, &sum); err != nil {
			rows.Close()
			return err
		}
		if version != applied+1 || checksums[version] != sum {
			rows.Close()
			return errSchema
		}
		applied = version
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, m := range migrations[applied:] {
		if _, err = tx.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("migration %04d failed: constraint validation failed; database left unchanged", m.version)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO schema_migrations VALUES(?,?,?)`, m.version, checksums[m.version], time.Now().UTC().UnixMicro()); err != nil {
			return errors.New("migration bookkeeping failed; database left unchanged")
		}
	}
	return tx.Commit()
}
