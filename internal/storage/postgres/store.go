// Package postgres implements the usage, limit and coordination contracts on a
// shared Postgres database. It is used only in distributed mode; SQLite stays
// the default backend of a single instance. Queries are parametrized and every
// value is validated by the shared storage rules before it is written.
package postgres

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// Store is one process's pool on the shared database. Bus notifies local
// subscribers only: every replica has its own.
type Store struct {
	Bus events.Bus

	db        *sql.DB
	closeOnce sync.Once
	closeErr  error

	owner atomic.Pointer[Owner]
	// journalLimit bounds unfinished journal entries for the whole cluster.
	journalLimit int64
}

// Owner identifies the process that admits: its instance and the fencing
// token of its lease. Journal entries and operations carry it, so a recovery
// knows which process left them.
type Owner struct {
	ID    string
	Fence int64
}

// Options configures a Store.
type Options struct {
	DSN            string
	MaxConnections int
}

// Open connects without migrating. The DSN is never logged or returned in an
// error.
func Open(ctx context.Context, options Options) (*Store, error) {
	db, err := sql.Open("pgx", options.DSN)
	if err != nil {
		return nil, storage.ErrUnavailable
	}
	conns := options.MaxConnections
	if conns <= 0 {
		conns = 10
	}
	db.SetMaxOpenConns(conns)
	db.SetMaxIdleConns(conns)
	db.SetConnMaxLifetime(30 * time.Minute)
	ping, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(ping); err != nil {
		db.Close()
		return nil, storage.ErrUnavailable
	}
	return &Store{db: db, journalLimit: 100_000}, nil
}

// DB exposes the pool to the coordinator, which runs its own transactions.
func (s *Store) DB() *sql.DB { return s.db }

// SetOwner records the identity written into new journal entries.
func (s *Store) SetOwner(o Owner) { s.owner.Store(&o) }

func (s *Store) currentOwner() Owner {
	if o := s.owner.Load(); o != nil {
		return *o
	}
	return Owner{}
}

func (s *Store) Close() error {
	s.closeOnce.Do(func() { s.closeErr = s.db.Close() })
	return s.closeErr
}

// Ping reports whether the database answers within the deadline.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.db.PingContext(ctx); err != nil {
		return storage.ErrUnavailable
	}
	return nil
}

// Checkpoint exists for the shutdown contract of the metering worker. Postgres
// makes every commit durable, so there is nothing left to flush.
func (s *Store) Checkpoint(context.Context) error { return nil }

// databaseError maps a driver error to a fixed category. Driver messages may
// quote values, so they never leave this function.
func databaseError(err error) error {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch {
		case pg.Code == "53100":
			return storage.ErrFull
		case len(pg.Code) == 5 && pg.Code[:2] == "23":
			return storage.ErrInvalid
		}
	}
	return storage.ErrUnavailable
}

// Retryable reports a serialization failure or deadlock: the metadata
// transaction may be repeated, never an upstream call.
func Retryable(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && (pg.Code == "40001" || pg.Code == "40P01" || pg.Code == "55P03")
}

type scopeKey struct{}

// Scope is a transaction opened by the coordinator. Store methods called with a
// context that carries it join the transaction instead of opening their own,
// and defer their bus notifications until the coordinator commits.
type Scope struct {
	Tx    *sql.Tx
	after []func()
}

// WithScope attaches a transaction to ctx.
func WithScope(ctx context.Context, scope *Scope) context.Context {
	return context.WithValue(ctx, scopeKey{}, scope)
}

// Committed runs the notifications deferred by the methods that joined.
func (sc *Scope) Committed() {
	for _, fn := range sc.after {
		fn()
	}
	sc.after = nil
}

func scopeFrom(ctx context.Context) *Scope {
	sc, _ := ctx.Value(scopeKey{}).(*Scope)
	return sc
}

// inTx runs fn in the caller's scope or in a transaction of its own. notify
// runs after the commit, whichever transaction it was.
func (s *Store) inTx(ctx context.Context, fn func(tx *sql.Tx) (notify func(), err error)) error {
	if sc := scopeFrom(ctx); sc != nil {
		notify, err := fn(sc.Tx)
		if err == nil && notify != nil {
			sc.after = append(sc.after, notify)
		}
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return storage.ErrUnavailable
	}
	defer tx.Rollback()
	notify, err := fn(tx)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return databaseError(err)
	}
	if notify != nil {
		notify()
	}
	return nil
}

func micros(t time.Time) int64 { return t.UTC().UnixMicro() }

func nowMicros() int64 { return time.Now().UTC().UnixMicro() }
