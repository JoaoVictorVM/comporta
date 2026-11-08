package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"net/url"
	"os"
	"path/filepath"
)

func OpenReader(ctx context.Context, dir string) (*Store, error) {
	return openReader(ctx, dir, false)
}

func openReader(ctx context.Context, dir string, owned bool) (*Store, error) {
	path := filepath.Join(dir, "usage.sqlite")
	f, err := openPrivate(path)
	if err != nil {
		return nil, storage.ErrUnavailable
	}
	f.Close()
	var lock *os.File
	if !owned {
		lock, err = openPrivate(filepath.Join(dir, "usage.lock"))
		if err != nil {
			return nil, storage.ErrUnavailable
		}
		acquired, err := tryLock(lock)
		if err != nil {
			lock.Close()
			return nil, storage.ErrUnavailable
		}
		if acquired {
			owned = true
		} else {
			lock.Close()
			lock = nil
		}
	}
	fail := func(err error) (*Store, error) {
		if lock != nil {
			lock.Close()
		}
		return nil, err
	}
	wal, shm := false, false
	for _, name := range []string{"usage.sqlite-wal", "usage.sqlite-shm"} {
		file, err := openPrivate(filepath.Join(dir, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fail(errInsecure)
		}
		file.Close()
		if name == "usage.sqlite-wal" {
			wal = true
		} else {
			shm = true
		}
	}
	// Immutable mode is safe only while our instance lock excludes a writer and
	// there is no WAL to ignore. Otherwise require existing sidecars, so a reader
	// cannot create them or mistake an incomplete history for an empty database.
	immutable := owned && !wal
	if !immutable && (!wal || !shm) {
		return fail(storage.ErrUnavailable)
	}
	uriPath := filepath.ToSlash(path)
	if filepath.VolumeName(path) != "" {
		uriPath = "/" + uriPath
	}
	u := url.URL{Scheme: "file", Path: uriPath, RawQuery: "mode=ro&_pragma=query_only(ON)&_pragma=busy_timeout(1000)&_pragma=foreign_keys(ON)&_pragma=synchronous(FULL)"}
	if immutable {
		u.RawQuery += "&immutable=1"
	}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return fail(storage.ErrUnavailable)
	}
	db.SetMaxOpenConns(4)
	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return fail(storage.ErrUnavailable)
	}
	if err = checkSchema(ctx, db); err != nil {
		db.Close()
		return fail(err)
	}
	return &Store{db: db, dir: dir, lock: lock}, nil
}
