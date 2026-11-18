package keys

import (
	"context"
	"crypto/rand"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Lifecycle is what the CLI does with keys, whatever holds them: keys.json on
// a single instance, or the shared database in distributed mode.
type Lifecycle interface {
	Create(ctx context.Context, app string) (Created, error)
	Rotate(ctx context.Context, app string) (Created, error)
	List() ([]Metadata, error)
	Revoke(ctx context.Context, id string) (bool, error)
}

var _ Lifecycle = (*Store)(nil)

// SharedRecord is one key as a shared backend stores it: the digest only.
type SharedRecord struct {
	ID         string
	App        string
	Digest     Digest
	CreatedAt  time.Time
	LastUsedAt time.Time
	RevokedAt  time.Time
}

// SharedBackend is the database side of distributed keys.
type SharedBackend interface {
	LoadKeys(ctx context.Context) ([]SharedRecord, error)
	TouchKeys(ctx context.Context, used map[string]time.Time) error
}

// NewSecret draws a key and its public identifier from the system random
// source, in the same formats as keys.json.
func NewSecret() (Key, string, error) {
	key, err := generateKey(rand.Reader)
	if err != nil {
		return Key{}, "", err
	}
	id, err := generateID(rand.Reader)
	if err != nil {
		return Key{}, "", err
	}
	return key, id, nil
}

// ValidAppName applies the key store's rule for app names.
func ValidAppName(name string) bool { return validAppName(name) }

// ValidID applies the key store's rule for key identifiers.
func ValidID(id string) bool { return validID(id) }

// MetadataOf is the public view of a shared record.
func MetadataOf(r SharedRecord) Metadata {
	return Metadata{ID: r.ID, App: r.App, CreatedAt: r.CreatedAt, LastUsedAt: r.LastUsedAt, RevokedAt: r.RevokedAt}
}

// SharedSource serves authentication from the shared database. It refreshes
// its generation every second, so a revocation on any replica reaches this one
// within that bound; a failed refresh makes the generation unavailable, which
// refuses every key instead of trusting a stale copy for long.
type SharedSource struct {
	backend SharedBackend
	logger  *slog.Logger
	state   atomic.Pointer[Generation]
	number  atomic.Int64
	staleAt time.Time

	mu   sync.Mutex
	used map[string]time.Time
}

// SharedRefresh is the bound on how stale an accepted key can be.
const SharedRefresh = time.Second

// NewSharedSource loads the keys once, so a start fails before the listeners
// open when the database cannot be read.
func NewSharedSource(ctx context.Context, backend SharedBackend, logger *slog.Logger) (*SharedSource, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	s := &SharedSource{backend: backend, logger: logger, used: map[string]time.Time{}}
	if err := s.refresh(ctx); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *SharedSource) refresh(ctx context.Context) error {
	records, err := s.backend.LoadKeys(ctx)
	if err != nil {
		return err
	}
	entries := make([]Entry, 0, len(records))
	for _, r := range records {
		entries = append(entries, Entry{Digest: r.Digest, Identity: Identity{appName: r.App, keyID: r.ID}, Revoked: !r.RevokedAt.IsZero()})
	}
	s.state.Store(&Generation{number: s.number.Add(1), available: true, entries: entries})
	return nil
}

// Capture returns the generation in force.
func (s *SharedSource) Capture() *Generation { return s.state.Load() }

// RecordUse buffers the last use of a key; Run writes it in batches.
func (s *SharedSource) RecordUse(keyID string, at time.Time) {
	s.mu.Lock()
	if at.After(s.used[keyID]) {
		s.used[keyID] = at
	}
	s.mu.Unlock()
}

// Run refreshes the keys every second and flushes last use every ten.
func (s *SharedSource) Run(ctx context.Context) error {
	refresh := time.NewTicker(SharedRefresh)
	defer refresh.Stop()
	flush := time.NewTicker(10 * time.Second)
	defer flush.Stop()
	failures := 0
	for {
		select {
		case <-ctx.Done():
			s.flush(context.Background())
			return nil
		case <-refresh.C:
			call, cancel := context.WithTimeout(ctx, SharedRefresh)
			err := s.refresh(call)
			cancel()
			if err != nil {
				failures++
				// Two missed refreshes leave a revocation unconfirmed for longer
				// than the bound, so authentication fails closed until one works.
				if failures >= 2 {
					s.state.Store(&Generation{number: s.number.Add(1)})
					s.logger.Error("app keys unavailable; authentication refused")
				}
				continue
			}
			failures = 0
		case <-flush.C:
			s.flush(ctx)
		}
	}
}

func (s *SharedSource) flush(ctx context.Context) {
	s.mu.Lock()
	used := s.used
	s.used = map[string]time.Time{}
	s.mu.Unlock()
	if len(used) == 0 {
		return
	}
	call, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := s.backend.TouchKeys(call, used); err != nil {
		s.mu.Lock()
		for id, at := range used {
			if at.After(s.used[id]) {
				s.used[id] = at
			}
		}
		s.mu.Unlock()
	}
}
