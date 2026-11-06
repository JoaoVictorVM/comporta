// Package auth turns a presented credential into an authenticated identity and
// adapts that decision to HTTP. It opens no ports and forwards nothing.
package auth

import (
	"crypto/subtle"
	"errors"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/keys"
)

// Authentication outcomes. They are categories, never echoes of the credential.
var (
	ErrInvalidKey  = errors.New("invalid Comporta API key")
	ErrRevokedKey  = errors.New("this Comporta API key was revoked")
	ErrUnavailable = errors.New("authentication storage unavailable")
)

// KeySource provides the current state of the key store.
type KeySource interface {
	Capture() *keys.Generation
	RecordUse(keyID string, at time.Time)
}

// Clock provides the current time.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// Authenticator resolves a credential against one generation of the store and one
// snapshot of the configuration, both captured once by the caller.
type Authenticator struct {
	source  KeySource
	clock   Clock
	compare func(a, b []byte) int
}

// NewAuthenticator returns an Authenticator reading from source.
func NewAuthenticator(source KeySource, clock Clock) *Authenticator {
	if clock == nil {
		clock = systemClock{}
	}
	return &Authenticator{source: source, clock: clock, compare: subtle.ConstantTimeCompare}
}

// Authenticate never returns early on a match: it visits every entry, tombstones
// included, so that the position of a key among the others does not change how
// much work is done. That is about the secret bytes; total time still depends on
// how many keys exist, which is not secret.
//
// A key whose app left the configuration gets the generic error, so the response
// cannot be used to learn which apps exist. Restoring the app restores the key.
func (a *Authenticator) Authenticate(key keys.Key, generation *keys.Generation, snapshot *config.Snapshot) (keys.Identity, error) {
	if !generation.Available() {
		return keys.Identity{}, ErrUnavailable
	}
	presented := key.Digest()
	var match keys.Entry
	found := false
	for entry := range generation.Entries() {
		if a.compare(presented[:], entry.Digest[:]) == 1 {
			match, found = entry, true
		}
	}
	if !found {
		return keys.Identity{}, ErrInvalidKey
	}
	if match.Revoked {
		return keys.Identity{}, ErrRevokedKey
	}
	if _, ok := snapshot.App(match.Identity.AppName()); !ok {
		return keys.Identity{}, ErrInvalidKey
	}
	a.source.RecordUse(match.Identity.KeyID(), a.clock.Now().UTC())
	return match.Identity, nil
}
