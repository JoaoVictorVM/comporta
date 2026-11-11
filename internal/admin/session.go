package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"sync"
	"time"
)

const (
	// SessionCookie carries the opaque session of the panel.
	SessionCookie = "comporta_admin_session"
	// SessionLifetime is absolute: a session is never renewed by use.
	SessionLifetime = 12 * time.Hour
	// MaxSessions bounds the memory held by sessions.
	MaxSessions = 1024
)

// ErrSessionsFull means the session table is at capacity.
var ErrSessionsFull = errors.New("admin session capacity reached")

var sessionPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Sessions keeps only digests of session identifiers, in memory: a restart or a
// token reset ends every session.
type Sessions struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[[sha256.Size]byte]time.Time
}

// NewSessions returns an empty table. A nil clock uses the system clock.
func NewSessions(now func() time.Time) *Sessions {
	if now == nil {
		now = time.Now
	}
	return &Sessions{now: now, entries: map[[sha256.Size]byte]time.Time{}}
}

// Create issues a session and returns its identifier and absolute expiry.
func (s *Sessions) Create() (string, time.Time, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", time.Time{}, err
	}
	id := hex.EncodeToString(raw[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if len(s.entries) >= MaxSessions {
		s.sweep(now)
		if len(s.entries) >= MaxSessions {
			return "", time.Time{}, ErrSessionsFull
		}
	}
	expires := now.Add(SessionLifetime)
	s.entries[sha256.Sum256([]byte(id))] = expires
	return id, expires, nil
}

// Valid reports whether the identifier names a live session.
func (s *Sessions) Valid(id string) bool {
	if !sessionPattern.MatchString(id) {
		return false
	}
	key := sha256.Sum256([]byte(id))
	s.mu.Lock()
	defer s.mu.Unlock()
	expires, ok := s.entries[key]
	if !ok {
		return false
	}
	if !s.now().Before(expires) {
		delete(s.entries, key)
		return false
	}
	return true
}

// Delete ends a session. Unknown identifiers are ignored.
func (s *Sessions) Delete(id string) {
	if !sessionPattern.MatchString(id) {
		return
	}
	s.mu.Lock()
	delete(s.entries, sha256.Sum256([]byte(id)))
	s.mu.Unlock()
}

// Len counts sessions, expired ones included until they are swept.
func (s *Sessions) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

func (s *Sessions) sweep(now time.Time) {
	for key, expires := range s.entries {
		if !now.Before(expires) {
			delete(s.entries, key)
		}
	}
}

// sessionCookie builds the cookie of a new session. Secure follows the
// connection itself: forwarded headers are not trusted.
func sessionCookie(id string, secure bool) *http.Cookie {
	return &http.Cookie{
		Name: SessionCookie, Value: id, Path: "/", MaxAge: int(SessionLifetime / time.Second),
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secure,
	}
}

func expiredCookie(secure bool) *http.Cookie {
	return &http.Cookie{
		Name: SessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: secure,
	}
}

// requestOrigin is the origin the admin listener serves this request on. The
// Host was already checked against allowed_hosts by the listener.
func requestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}
