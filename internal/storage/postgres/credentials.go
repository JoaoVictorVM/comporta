package postgres

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"hash/fnv"
	"slices"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/auth"
	"github.com/JoaoVictorVM/comporta/internal/keys"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// KeyStore keeps app keys in the shared database. Only digests are stored;
// the value of a new key is returned once to the caller.
type KeyStore struct{ s *Store }

// Keys returns the key store of the shared database.
func (s *Store) Keys() *KeyStore { return &KeyStore{s: s} }

var (
	_ keys.Lifecycle     = (*KeyStore)(nil)
	_ keys.SharedBackend = (*KeyStore)(nil)
)

func appLockKey(app string) int64 {
	h := fnv.New64a()
	h.Write([]byte("keys:" + app))
	return int64(h.Sum64())
}

func (k *KeyStore) Create(ctx context.Context, app string) (keys.Created, error) {
	if !keys.ValidAppName(app) {
		return keys.Created{}, keys.ErrInvalidKey
	}
	var created keys.Created
	err := k.s.inTx(ctx, func(tx *sql.Tx) (func(), error) {
		// Serializes issuance per app: a row CHECK cannot bound two active keys.
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, appLockKey(app)); err != nil {
			return nil, databaseError(err)
		}
		var active, total int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE app_name=$1 AND revoked_at IS NULL), count(*) FROM app_keys`, app).Scan(&active, &total); err != nil {
			return nil, databaseError(err)
		}
		if active >= keys.MaxActiveKeysPerApp {
			return nil, &keys.ActiveLimitError{App: app}
		}
		if total >= keys.MaxRecords {
			return nil, keys.ErrStoreFull
		}
		key, id, err := keys.NewSecret()
		if err != nil {
			return nil, err
		}
		digest := key.Digest()
		now := time.Now().UTC()
		if _, err := tx.ExecContext(ctx, `INSERT INTO app_keys(key_id,app_name,digest,prefix,created_at) VALUES($1,$2,$3,$4,$5)`, id, app, digest[:], id, micros(now)); err != nil {
			return nil, databaseError(err)
		}
		created = keys.Created{Key: key, Metadata: keys.Metadata{ID: id, App: app, CreatedAt: now}}
		return nil, nil
	})
	if err != nil {
		var limit *keys.ActiveLimitError
		if errors.As(err, &limit) || errors.Is(err, keys.ErrStoreFull) || errors.Is(err, keys.ErrEntropy) {
			return keys.Created{}, err
		}
		return keys.Created{}, keys.ErrStoreIO
	}
	return created, nil
}

func (k *KeyStore) Rotate(ctx context.Context, app string) (keys.Created, error) {
	return k.Create(ctx, app)
}

func (k *KeyStore) List() ([]keys.Metadata, error) {
	records, err := k.LoadKeys(context.Background())
	if err != nil {
		return nil, keys.ErrStoreIO
	}
	out := make([]keys.Metadata, 0, len(records))
	for _, r := range records {
		out = append(out, keys.MetadataOf(r))
	}
	slices.SortFunc(out, func(a, b keys.Metadata) int {
		switch {
		case a.App != b.App:
			if a.App < b.App {
				return -1
			}
			return 1
		case !a.CreatedAt.Equal(b.CreatedAt):
			return a.CreatedAt.Compare(b.CreatedAt)
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		}
		return 0
	})
	return out, nil
}

func (k *KeyStore) Revoke(ctx context.Context, id string) (bool, error) {
	if !keys.ValidID(id) {
		return false, keys.ErrKeyNotFound
	}
	result, err := k.s.db.ExecContext(ctx, `UPDATE app_keys SET revoked_at=$1 WHERE key_id=$2 AND revoked_at IS NULL`, nowMicros(), id)
	if err != nil {
		return false, keys.ErrIndeterminate
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, keys.ErrIndeterminate
	}
	if n == 1 {
		return true, nil
	}
	var exists bool
	if err := k.s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM app_keys WHERE key_id=$1)`, id).Scan(&exists); err != nil {
		return false, keys.ErrStoreIO
	}
	if !exists {
		return false, keys.ErrKeyNotFound
	}
	return false, nil
}

func optionalTime(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return time.UnixMicro(v.Int64).UTC()
}

// LoadKeys reads every key record, revoked ones included.
func (k *KeyStore) LoadKeys(ctx context.Context) ([]keys.SharedRecord, error) {
	rows, err := k.s.db.QueryContext(ctx, `SELECT key_id,app_name,digest,created_at,last_used_at,revoked_at FROM app_keys ORDER BY key_id`)
	if err != nil {
		return nil, storage.ErrUnavailable
	}
	defer rows.Close()
	var out []keys.SharedRecord
	for rows.Next() {
		var r keys.SharedRecord
		var digest []byte
		var created int64
		var used, revoked sql.NullInt64
		if err = rows.Scan(&r.ID, &r.App, &digest, &created, &used, &revoked); err != nil || len(digest) != sha256.Size {
			return nil, storage.ErrUnavailable
		}
		copy(r.Digest[:], digest)
		r.CreatedAt, r.LastUsedAt, r.RevokedAt = time.UnixMicro(created).UTC(), optionalTime(used), optionalTime(revoked)
		out = append(out, r)
	}
	return out, rows.Err()
}

// TouchKeys advances last use; it never moves it backwards.
func (k *KeyStore) TouchKeys(ctx context.Context, used map[string]time.Time) error {
	for id, at := range used {
		if _, err := k.s.db.ExecContext(ctx, `UPDATE app_keys SET last_used_at=greatest(coalesce(last_used_at,0),$1) WHERE key_id=$2`, micros(at), id); err != nil {
			return storage.ErrUnavailable
		}
	}
	return nil
}

// ErrNoCredential means the admin token was never created for this cluster.
var ErrNoCredential = errors.New("admin credential not initialized")

func (s *Store) LoadAdminCredential(ctx context.Context) ([32]byte, int64, error) {
	var out [32]byte
	var digest []byte
	var epoch int64
	err := s.db.QueryRowContext(ctx, `SELECT digest,epoch FROM admin_credentials WHERE id=1`).Scan(&digest, &epoch)
	if errors.Is(err, sql.ErrNoRows) {
		return out, 0, ErrNoCredential
	}
	if err != nil || len(digest) != 32 {
		return out, 0, storage.ErrUnavailable
	}
	copy(out[:], digest)
	return out, epoch, nil
}

// ResetAdminCredential stores a new digest, raises the epoch and deletes every
// session, in one transaction. initialOnly refuses when a credential exists.
func (s *Store) ResetAdminCredential(ctx context.Context, digest [32]byte, initialOnly bool) (int64, error) {
	var epoch int64
	err := s.inTx(ctx, func(tx *sql.Tx) (func(), error) {
		if _, err := tx.ExecContext(ctx, `LOCK TABLE admin_credentials IN EXCLUSIVE MODE`); err != nil {
			return nil, databaseError(err)
		}
		err := tx.QueryRowContext(ctx, `SELECT epoch FROM admin_credentials WHERE id=1`).Scan(&epoch)
		switch {
		case err == nil && initialOnly:
			return nil, storage.ErrConflict
		case errors.Is(err, sql.ErrNoRows):
			epoch = 0
		case err != nil:
			return nil, databaseError(err)
		}
		epoch++
		if _, err := tx.ExecContext(ctx, `INSERT INTO admin_credentials VALUES(1,$1,$2,$3) ON CONFLICT(id) DO UPDATE SET digest=excluded.digest,epoch=excluded.epoch,updated_at=excluded.updated_at`, digest[:], epoch, nowMicros()); err != nil {
			return nil, databaseError(err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM admin_sessions`); err != nil {
			return nil, databaseError(err)
		}
		return nil, nil
	})
	return epoch, err
}

// MaxAdminSessions bounds live sessions for the whole cluster.
const MaxAdminSessions = 1024

// ErrSessionsFull means the cluster reached its session capacity.
var ErrSessionsFull = errors.New("admin session capacity reached")

// CreateSession stores the digest of a session identifier.
func (s *Store) CreateSession(ctx context.Context, digest [32]byte, epoch int64, expires time.Time) error {
	return s.inTx(ctx, func(tx *sql.Tx) (func(), error) {
		if _, err := tx.ExecContext(ctx, `LOCK TABLE admin_sessions IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return nil, databaseError(err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM admin_sessions WHERE expires_at<=$1`, nowMicros()); err != nil {
			return nil, databaseError(err)
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM admin_sessions`).Scan(&n); err != nil {
			return nil, databaseError(err)
		}
		if n >= MaxAdminSessions {
			return nil, ErrSessionsFull
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO admin_sessions VALUES($1,$2,$3)`, digest[:], epoch, micros(expires)); err != nil {
			return nil, databaseError(err)
		}
		return nil, nil
	})
}

// SessionValid checks a session digest against the current credential epoch.
func (s *Store) SessionValid(ctx context.Context, digest [32]byte) (bool, error) {
	var valid bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM admin_sessions se JOIN admin_credentials c ON c.id=1 AND c.epoch=se.credential_epoch WHERE se.digest=$1 AND se.expires_at>$2)`, digest[:], nowMicros()).Scan(&valid)
	if err != nil {
		return false, storage.ErrUnavailable
	}
	return valid, nil
}

// SessionExpiry returns the absolute end of a live session of the current
// credential epoch.
func (s *Store) SessionExpiry(ctx context.Context, digest [32]byte) (time.Time, bool, error) {
	var expires int64
	err := s.db.QueryRowContext(ctx, `SELECT se.expires_at FROM admin_sessions se JOIN admin_credentials c ON c.id=1 AND c.epoch=se.credential_epoch WHERE se.digest=$1 AND se.expires_at>$2`, digest[:], nowMicros()).Scan(&expires)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, storage.ErrUnavailable
	}
	return time.UnixMicro(expires).UTC(), true, nil
}

// DeleteSession removes one session.
func (s *Store) DeleteSession(ctx context.Context, digest [32]byte) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM admin_sessions WHERE digest=$1`, digest[:]); err != nil {
		return storage.ErrUnavailable
	}
	return nil
}

// MaxAuthOrigins bounds the tracked origins per surface.
const MaxAuthOrigins = 100_000

// ErrAuthCapacity means the shared guard cannot track a new origin.
var ErrAuthCapacity = errors.New("authentication guard capacity reached")

// AuthWindows is the shared count of failed authentications.
type AuthWindows struct{ s *Store }

// AuthWindows returns the shared authentication guard storage.
func (s *Store) AuthWindows() *AuthWindows { return &AuthWindows{s: s} }

var _ auth.SharedWindows = (*AuthWindows)(nil)

// Update reads and replaces the window of one origin under a row lock, so
// concurrent failures on several replicas are all counted.
func (a *AuthWindows) Update(ctx context.Context, surface string, origin [32]byte, expires time.Time, update func(auth.SharedWindow) (auth.SharedWindow, bool)) (auth.SharedWindow, error) {
	var out auth.SharedWindow
	err := a.s.inTx(ctx, func(tx *sql.Tx) (func(), error) {
		// Serializes the first failure of an origin, which has no row to lock yet.
		if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, originLockKey(surface, origin)); err != nil {
			return nil, databaseError(err)
		}
		var data string
		var blocked sql.NullInt64
		var current auth.SharedWindow
		err := tx.QueryRowContext(ctx, `SELECT failures::text,blocked_until FROM auth_windows WHERE surface=$1 AND origin_digest=$2 AND expires_at>$3`, surface, origin[:], nowMicros()).Scan(&data, &blocked)
		exists := err == nil
		switch {
		case exists:
			if json.Unmarshal([]byte(data), &current.Failures) != nil {
				return nil, storage.ErrInvalid
			}
			current.BlockedUntil = blocked.Int64
		case !errors.Is(err, sql.ErrNoRows):
			return nil, databaseError(err)
		}
		next, write := update(current)
		out = next
		if !write {
			return nil, nil
		}
		if !exists {
			var n int
			if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM auth_windows WHERE surface=$1 AND expires_at>$2`, surface, nowMicros()).Scan(&n); err != nil {
				return nil, databaseError(err)
			}
			if n >= MaxAuthOrigins {
				return nil, ErrAuthCapacity
			}
		}
		if next.Failures == nil {
			next.Failures = []int64{}
		}
		failures, _ := json.Marshal(next.Failures)
		var until any
		if next.BlockedUntil > 0 {
			until = next.BlockedUntil
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO auth_windows VALUES($1,$2,$3,$4,$5) ON CONFLICT(surface,origin_digest) DO UPDATE SET failures=excluded.failures,blocked_until=excluded.blocked_until,expires_at=excluded.expires_at`,
			surface, origin[:], string(failures), until, micros(expires)); err != nil {
			return nil, databaseError(err)
		}
		return nil, nil
	})
	return out, err
}

func originLockKey(surface string, origin [32]byte) int64 {
	h := fnv.New64a()
	h.Write([]byte("auth:" + surface))
	h.Write(origin[:])
	return int64(h.Sum64())
}

// Read returns the window of one origin without changing it.
func (a *AuthWindows) Read(ctx context.Context, surface string, origin [32]byte) (auth.SharedWindow, error) {
	var out auth.SharedWindow
	var data string
	var blocked sql.NullInt64
	err := a.s.db.QueryRowContext(ctx, `SELECT failures::text,blocked_until FROM auth_windows WHERE surface=$1 AND origin_digest=$2 AND expires_at>$3`, surface, origin[:], nowMicros()).Scan(&data, &blocked)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil || json.Unmarshal([]byte(data), &out.Failures) != nil {
		return out, storage.ErrUnavailable
	}
	out.BlockedUntil = blocked.Int64
	return out, nil
}
