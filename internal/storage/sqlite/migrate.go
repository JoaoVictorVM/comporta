package sqlite

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
var schema string

var errSchema = errors.New("incompatible database version or migration checksum")

type migration struct {
	version int
	sql     string
}

func migrate(ctx context.Context, db *sql.DB) error {
	err := migrateSet(ctx, db, []migration{{1, schema}})
	if err == nil || errors.Is(err, errSchema) {
		return err
	}
	return errors.New("migration 0001 failed: database unavailable; database left unchanged")
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
