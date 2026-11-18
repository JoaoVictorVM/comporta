package admin

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// Verifier checks the admin token presented by a request.
type Verifier interface {
	Verify(token string) bool
}

// SessionStore keeps the digests of panel sessions.
type SessionStore interface {
	Create() (string, time.Time, error)
	Valid(id string) bool
	Delete(id string)
}

var (
	_ Verifier     = (*Credential)(nil)
	_ SessionStore = (*Sessions)(nil)
)

// SharedCredentials is the database side of the distributed admin credential.
type SharedCredentials interface {
	LoadAdminCredential(ctx context.Context) (digest [32]byte, epoch int64, err error)
	ResetAdminCredential(ctx context.Context, digest [32]byte, initialOnly bool) (int64, error)
	CreateSession(ctx context.Context, digest [32]byte, epoch int64, expires time.Time) error
	SessionValid(ctx context.Context, digest [32]byte) (bool, error)
	DeleteSession(ctx context.Context, digest [32]byte) error
}

// ErrNoSharedCredential means the cluster has no admin token yet; it is
// created with "comporta admin reset-token", which prints it once.
var ErrNoSharedCredential = errors.New("admin credential not initialized; run \"comporta admin reset-token\" once for the cluster")

// SharedCredential verifies the admin token against the digest every replica
// reads from the database. The digest is cached for at most one second, so a
// rotation on one replica is enforced by all within that bound; an unreadable
// database refuses the token.
type SharedCredential struct {
	store SharedCredentials

	mu      sync.Mutex
	digest  [32]byte
	epoch   int64
	fetched time.Time
	ok      bool
}

// OpenSharedCredential loads the credential before the listeners open.
func OpenSharedCredential(ctx context.Context, store SharedCredentials) (*SharedCredential, error) {
	c := &SharedCredential{store: store}
	if !c.refresh(ctx) {
		return nil, ErrNoSharedCredential
	}
	return c, nil
}

func (c *SharedCredential) refresh(ctx context.Context) bool {
	digest, epoch, err := c.store.LoadAdminCredential(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.fetched = time.Now()
	c.ok = err == nil
	c.digest, c.epoch = digest, epoch
	return c.ok
}

func (c *SharedCredential) current() ([32]byte, int64, bool) {
	c.mu.Lock()
	fresh := time.Since(c.fetched) < time.Second
	digest, epoch, ok := c.digest, c.epoch, c.ok
	c.mu.Unlock()
	if fresh {
		return digest, epoch, ok
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	c.refresh(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.digest, c.epoch, c.ok
}

func (c *SharedCredential) Verify(token string) bool {
	if !tokenPattern.MatchString(token) {
		return false
	}
	digest, _, ok := c.current()
	if !ok {
		return false
	}
	presented := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(presented[:], digest[:]) == 1
}

// ResetSharedToken creates the cluster token, or rotates it, raising the epoch
// that ends every session. The value is returned once and never stored.
func ResetSharedToken(ctx context.Context, store SharedCredentials, initialOnly bool) (string, error) {
	var raw [tokenBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", errors.New("the system random source failed")
	}
	token := hex.EncodeToString(raw[:])
	if _, err := store.ResetAdminCredential(ctx, sha256.Sum256([]byte(token)), initialOnly); err != nil {
		return "", err
	}
	return token, nil
}

// SharedSessions keeps panel sessions in the database, so a cookie issued by
// one replica is valid on all of them without sticky sessions.
type SharedSessions struct {
	store      SharedCredentials
	credential *SharedCredential
	now        func() time.Time
}

func NewSharedSessions(store SharedCredentials, credential *SharedCredential) *SharedSessions {
	return &SharedSessions{store: store, credential: credential, now: time.Now}
}

func (s *SharedSessions) Create() (string, time.Time, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", time.Time{}, err
	}
	id := hex.EncodeToString(raw[:])
	_, epoch, ok := s.credential.current()
	if !ok {
		return "", time.Time{}, ErrSessionsFull
	}
	expires := s.now().Add(SessionLifetime)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.store.CreateSession(ctx, sha256.Sum256([]byte(id)), epoch, expires); err != nil {
		return "", time.Time{}, ErrSessionsFull
	}
	return id, expires, nil
}

func (s *SharedSessions) Valid(id string) bool {
	if !sessionPattern.MatchString(id) {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ok, err := s.store.SessionValid(ctx, sha256.Sum256([]byte(id)))
	return err == nil && ok
}

func (s *SharedSessions) Delete(id string) {
	if !sessionPattern.MatchString(id) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = s.store.DeleteSession(ctx, sha256.Sum256([]byte(id)))
}
