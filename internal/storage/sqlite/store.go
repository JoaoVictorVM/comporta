package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	_ "modernc.org/sqlite"
	"net/url"
	"os"
	"path/filepath"
	"sync"
)

var errInsecure = errors.New("usage data directory is not private")

type Store struct {
	Bus          events.Bus
	db           *sql.DB
	readDB       *sql.DB
	lock         *os.File
	mu           sync.Mutex
	dir          string
	initialized  bool
	closeOnce    sync.Once
	closeErr     error
	journalCount int
}

func Open(ctx context.Context, dir string) (*Store, error) {
	s, err := Prepare(ctx, dir)
	if err != nil {
		return nil, err
	}
	if err = s.Initialize(ctx); err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// Prepare holds the instance lock while identity continuity is checked. It never
// creates a database or migrates an existing one before that decision.
func Prepare(ctx context.Context, dir string) (*Store, error) {
	if err := ensurePrivateDir(dir); err != nil {
		return nil, errInsecure
	}
	if err := checkDirectory(dir); err != nil {
		return nil, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, errInsecure
	}
	lock, err := openLockFile(filepath.Join(dir, "usage.lock"))
	if err != nil {
		return nil, errInsecure
	}
	ok, err := tryLock(lock)
	if err != nil || !ok {
		lock.Close()
		return nil, errors.New("usage directory is already in use")
	}
	s := &Store{lock: lock, dir: dir}
	for _, name := range []string{"usage.sqlite", "usage.sqlite-wal", "usage.sqlite-shm"} {
		path := filepath.Join(dir, name)
		file, err := openPrivate(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			s.Close()
			return nil, errInsecure
		}
		file.Close()
	}
	return s, nil
}

func (s *Store) Initialize(ctx context.Context) error {
	if s.initialized {
		return nil
	}
	if s.db != nil {
		if err := s.db.Close(); err != nil {
			return storage.ErrUnavailable
		}
		s.db = nil
	}
	path := filepath.Join(s.dir, "usage.sqlite")
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		f, e := createPrivateFile(path)
		if e != nil {
			return errInsecure
		}
		f.Close()
	}
	uriPath := filepath.ToSlash(path)
	if filepath.VolumeName(path) != "" {
		uriPath = "/" + uriPath
	}
	u := url.URL{Scheme: "file", Path: uriPath}
	q := u.Query()
	q.Set("mode", "rw")
	for _, p := range []string{"journal_mode(WAL)", "synchronous(FULL)", "foreign_keys(ON)", "busy_timeout(1000)"} {
		q.Add("_pragma", p)
	}
	u.RawQuery = q.Encode()
	var err error
	s.db, err = sql.Open("sqlite", u.String())
	if err != nil {
		s.Close()
		return storage.ErrUnavailable
	}
	s.db.SetMaxOpenConns(1)
	var mode string
	var syncMode, foreign, busy int
	if err = s.db.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&mode); err == nil {
		err = s.db.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&syncMode)
	}
	if err == nil {
		err = s.db.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&foreign)
	}
	if err == nil {
		err = s.db.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busy)
	}
	if err != nil || mode != "wal" || syncMode != 2 || foreign != 1 || busy != 1000 {
		s.Close()
		return storage.ErrUnavailable
	}
	if err = migrate(ctx, s.db); err != nil {
		s.Close()
		return err
	}
	reader, err := openReader(ctx, s.dir, true)
	if err != nil {
		return err
	}
	s.readDB = reader.db
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM metering_journal`).Scan(&s.journalCount); err != nil {
		return storage.ErrUnavailable
	}
	s.initialized = true
	return nil
}

func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		if s.readDB != nil {
			s.closeErr = s.readDB.Close()
		}
		if s.db != nil {
			s.closeErr = errors.Join(s.closeErr, s.db.Close())
		}
		if s.lock != nil {
			s.closeErr = errors.Join(s.closeErr, s.lock.Close())
		}
	})
	return s.closeErr
}

func (s *Store) Checkpoint(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var busy, log, done int
	if err := s.db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &log, &done); err != nil || busy != 0 {
		return storage.ErrUnavailable
	}
	return nil
}
