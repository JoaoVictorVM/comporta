package keys

import (
	"cmp"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"
)

const (
	documentFile = "keys.json"
	lockFile     = "keys.lock"

	defaultLockTimeout = 5 * time.Second
	lockPollInterval   = 20 * time.Millisecond

	readAttempts   = 5
	readRetryDelay = 50 * time.Millisecond

	maxCollisionRetries = 16
)

// fileOps are the steps of a commit, replaceable so that tests can fail each one.
type fileOps struct {
	create  func(path string) (*os.File, error)
	write   func(file *os.File, data []byte) error
	sync    func(file *os.File) error
	replace func(from, to string) error
	syncDir func(dir string) error
}

func defaultFileOps() fileOps {
	return fileOps{
		create:  createPrivateFile,
		write:   func(file *os.File, data []byte) error { _, err := file.Write(data); return err },
		sync:    func(file *os.File) error { return file.Sync() },
		replace: replaceFile,
		syncDir: syncDir,
	}
}

// Store is the only writer of keys.json. Each change holds an operating system
// lock while it rereads, validates, applies and atomically replaces the document,
// so that command line processes and the service never lose each other's updates.
type Store struct {
	dir         string
	clock       Clock
	entropy     io.Reader
	lockTimeout time.Duration
	ops         fileOps
}

// NewStore returns a store for keys.json inside the data directory.
func NewStore(dir string) *Store {
	return &Store{
		dir:         dir,
		clock:       systemClock{},
		entropy:     rand.Reader,
		lockTimeout: defaultLockTimeout,
		ops:         defaultFileOps(),
	}
}

func (s *Store) documentPath() string { return filepath.Join(s.dir, documentFile) }
func (s *Store) lockPath() string     { return filepath.Join(s.dir, lockFile) }

// readRaw returns the document bytes. It retries sharing conflicts, which on
// Windows can appear for an instant while a commit replaces the file; without the
// retry that instant would look like an unavailable store.
func (s *Store) readRaw() ([]byte, error) {
	var last error
	for attempt := range readAttempts {
		if attempt > 0 {
			time.Sleep(readRetryDelay)
		}
		data, err := s.readOnce()
		if err == nil || !isTransientOpenError(errors.Unwrap(err)) {
			return data, err
		}
		last = err
	}
	return nil, last
}

type ioFailure struct{ cause error }

func (e *ioFailure) Error() string { return ErrStoreIO.Error() }
func (e *ioFailure) Is(target error) bool {
	return target == ErrStoreIO
}
func (e *ioFailure) Unwrap() error { return e.cause }

func (s *Store) readOnce() ([]byte, error) {
	file, err := openPrivate(s.documentPath())
	if err != nil {
		return nil, classifyRead(err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, MaxDocumentBytes+1))
	if err != nil {
		return nil, &ioFailure{cause: err}
	}
	return data, nil
}

func classifyRead(err error) error {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return ErrStoreMissing
	case errors.Is(err, ErrStoreInsecure):
		return ErrStoreInsecure
	}
	return &ioFailure{cause: err}
}

func (s *Store) read() (*document, error) {
	data, err := s.readRaw()
	if err != nil {
		return nil, err
	}
	return parseDocument(data)
}

// lock waits for the exclusive lock. The lock file is never removed: it must stay
// the same file for every process, and the system releases it when its holder
// closes the handle or dies, so no stale-lock protocol is needed.
func (s *Store) lock(ctx context.Context) (func(), error) {
	file, err := openLockFile(s.lockPath())
	if err != nil {
		if errors.Is(err, ErrStoreInsecure) {
			return nil, err
		}
		return nil, &ioFailure{cause: err}
	}
	timeout := time.NewTimer(s.lockTimeout)
	defer timeout.Stop()
	ticker := time.NewTicker(lockPollInterval)
	defer ticker.Stop()
	for {
		locked, err := tryLock(file)
		if err != nil {
			_ = file.Close()
			return nil, &ioFailure{cause: err}
		}
		if locked {
			return func() { _ = file.Close() }, nil
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-timeout.C:
			_ = file.Close()
			return nil, ErrLockTimeout
		case <-ticker.C:
		}
	}
}

// commit publishes through a private temporary file in the same directory, so a
// failure before the replacement leaves the previous document intact. After the
// replacement the change is visible, and a failed durability step can no longer
// be reported as a plain failure: undoing it could bring a revoked key back.
func (s *Store) commit(doc *document) error {
	if err := doc.nextRevision(); err != nil {
		return err
	}
	data, err := doc.encode()
	if err != nil {
		return &ioFailure{cause: err}
	}
	suffix, err := generateID(s.entropy)
	if err != nil {
		return err
	}
	temporary := filepath.Join(s.dir, fmt.Sprintf(".keys-%s.tmp", suffix))
	file, err := s.ops.create(temporary)
	if err != nil {
		return &ioFailure{cause: err}
	}
	abandon := func(cause error) error {
		_ = file.Close()
		_ = os.Remove(temporary)
		return &ioFailure{cause: cause}
	}
	if err := s.ops.write(file, data); err != nil {
		return abandon(err)
	}
	if err := s.ops.sync(file); err != nil {
		return abandon(err)
	}
	if err := file.Close(); err != nil {
		return abandon(err)
	}
	if err := s.ops.replace(temporary, s.documentPath()); err != nil {
		_ = os.Remove(temporary)
		return &ioFailure{cause: err}
	}
	if err := s.ops.syncDir(s.dir); err != nil {
		return ErrIndeterminate
	}
	return nil
}

// Create adds a key for an app whose name the caller already validated. The
// returned value is the only copy that will ever exist.
func (s *Store) Create(ctx context.Context, app string) (Created, error) {
	if !validAppName(app) {
		return Created{}, errInvalidApp
	}
	if err := ensurePrivateDir(s.dir); err != nil {
		if errors.Is(err, ErrStoreInsecure) {
			return Created{}, err
		}
		return Created{}, &ioFailure{cause: err}
	}
	unlock, err := s.lock(ctx)
	if err != nil {
		return Created{}, err
	}
	defer unlock()

	doc, err := s.read()
	switch {
	case errors.Is(err, ErrStoreMissing):
		doc = &document{}
	case err != nil:
		return Created{}, err
	}
	if doc.activeCount(app) >= MaxActiveKeysPerApp {
		return Created{}, &ActiveLimitError{App: app}
	}
	if len(doc.keys) >= MaxRecords {
		return Created{}, ErrStoreFull
	}
	key, r, err := s.newRecord(doc, app)
	if err != nil {
		return Created{}, err
	}
	doc.keys = append(doc.keys, r)
	if err := s.commit(doc); err != nil {
		return Created{}, err
	}
	return Created{Key: key, Metadata: r.metadata()}, nil
}

// Rotate adds a key without revoking the others, so that the operator controls the
// window in which both are valid. It has the same contract as Create.
func (s *Store) Rotate(ctx context.Context, app string) (Created, error) {
	return s.Create(ctx, app)
}

func (s *Store) newRecord(doc *document, app string) (Key, record, error) {
	for range maxCollisionRetries {
		key, err := generateKey(s.entropy)
		if err != nil {
			return Key{}, record{}, err
		}
		id, err := generateID(s.entropy)
		if err != nil {
			return Key{}, record{}, err
		}
		digest := digestHex(key.Digest())
		if doc.find(id) != nil || doc.hasDigest(digest) {
			continue
		}
		return key, record{id: id, app: app, sha256: digest, createdAt: s.clock.Now().UTC()}, nil
	}
	return Key{}, record{}, ErrEntropy
}

// List returns public metadata ordered by app, creation and ID. It writes nothing,
// and a store that does not exist yet is simply empty.
func (s *Store) List() ([]Metadata, error) {
	doc, err := s.read()
	if errors.Is(err, ErrStoreMissing) {
		return []Metadata{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := make([]Metadata, len(doc.keys))
	for i := range doc.keys {
		out[i] = doc.keys[i].metadata()
	}
	slices.SortFunc(out, func(a, b Metadata) int {
		return cmp.Or(cmp.Compare(a.App, b.App), a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
	})
	return out, nil
}

// Revoke marks a key as revoked and keeps its record, which is what lets later
// attempts with that key be told apart from unknown ones. It reports whether this
// call changed anything; revoking twice succeeds without a new commit.
func (s *Store) Revoke(ctx context.Context, id string) (bool, error) {
	if !validID(id) {
		return false, ErrKeyNotFound
	}
	if _, err := s.read(); err != nil {
		if errors.Is(err, ErrStoreMissing) {
			return false, ErrKeyNotFound
		}
		return false, err
	}
	unlock, err := s.lock(ctx)
	if err != nil {
		return false, err
	}
	defer unlock()

	doc, err := s.read()
	if err != nil {
		return false, err
	}
	target := doc.find(id)
	if target == nil {
		return false, ErrKeyNotFound
	}
	if !target.active() {
		return false, nil
	}
	target.revokedAt = latest(s.clock.Now().UTC(), target.createdAt)
	if err := s.commit(doc); err != nil {
		return false, err
	}
	return true, nil
}

// mergeLastUsed applies recorded times under the lock on top of whatever the
// document says now. A key removed or revoked meanwhile stays that way, and times
// only move forward. Nothing is written if nothing changes.
func (s *Store) mergeLastUsed(ctx context.Context, used map[string]time.Time) error {
	if _, err := s.read(); err != nil {
		return err
	}
	unlock, err := s.lock(ctx)
	if err != nil {
		return err
	}
	defer unlock()

	doc, err := s.read()
	if err != nil {
		return err
	}
	changed := false
	for i := range doc.keys {
		r := &doc.keys[i]
		at, ok := used[r.id]
		if !ok || !r.active() {
			continue
		}
		// The clock may have moved back since the key was created.
		at = latest(at.UTC(), r.createdAt)
		if at.After(r.lastUsedAt) {
			r.lastUsedAt = at
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return s.commit(doc)
}

func latest(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}
