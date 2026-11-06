// Package keys manages the lifecycle of app keys: generation, the on-disk store,
// the live state used for authentication and last-used bookkeeping.
package keys

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"
)

const redacted = "[REDACTED]"

// Clock provides the current time.
type Clock interface {
	Now() time.Time
}

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

// Errors from this package use a fixed vocabulary, so they are safe to show to an
// operator: they never carry file content, key material or raw system errors.
var (
	ErrStoreMissing  = errors.New("keys.json does not exist; create a key with \"comporta keys create <app>\" first")
	ErrStoreCorrupt  = errors.New("keys.json is unreadable; restore from backup or recreate keys")
	ErrStoreInsecure = errors.New("keys.json or keys.lock is a link or can be accessed by other users; restrict it and try again")
	ErrStoreIO       = errors.New("keys.json cannot be accessed")
	ErrStoreFull     = errors.New("keys.json reached its limit of 10000 records; plan maintenance before creating more keys")
	ErrKeyNotFound   = errors.New("key ID not found")
	ErrLockTimeout   = errors.New("keys.json is locked by another process; try again")
	ErrIndeterminate = errors.New("the change to keys.json may or may not have been saved; check with \"comporta keys list\"")
	ErrEntropy       = errors.New("the system random source failed")
	ErrInvalidKey    = errors.New("invalid key syntax")
)

// ActiveLimitError reports that an app already holds the maximum of active keys.
type ActiveLimitError struct {
	App string
}

func (e *ActiveLimitError) Error() string {
	return fmt.Sprintf("%s already has %d active keys; revoke one before rotating", e.App, MaxActiveKeysPerApp)
}

// Key is an app key held in memory. Generic formatting and serialization reveal
// nothing: the value only leaves through an explicit call to Reveal.
type Key struct {
	value string
}

// Reveal returns the key. Only the output of key creation may use it.
func (k Key) Reveal() string { return k.value }

// Digest returns the SHA-256 of the key.
func (k Key) Digest() Digest { return digestOf(k.value) }

func (Key) String() string                { return redacted }
func (Key) GoString() string              { return redacted }
func (Key) Format(f fmt.State, _ rune)    { _, _ = io.WriteString(f, redacted) }
func (Key) LogValue() slog.Value          { return slog.StringValue(redacted) }
func (Key) MarshalText() ([]byte, error)  { return nil, errNotSerializable }
func (Key) MarshalJSON() ([]byte, error)  { return nil, errNotSerializable }
func (*Key) UnmarshalText(_ []byte) error { return errNotSerializable }
func (*Key) UnmarshalJSON(_ []byte) error { return errNotSerializable }

var errNotSerializable = errors.New("secret values cannot be serialized")

// Digest is the SHA-256 of a key. Like Key, it never reaches generic output.
type Digest [digestSize]byte

func (Digest) String() string               { return redacted }
func (Digest) GoString() string             { return redacted }
func (Digest) Format(f fmt.State, _ rune)   { _, _ = io.WriteString(f, redacted) }
func (Digest) LogValue() slog.Value         { return slog.StringValue(redacted) }
func (Digest) MarshalText() ([]byte, error) { return nil, errNotSerializable }
func (Digest) MarshalJSON() ([]byte, error) { return nil, errNotSerializable }

// Identity is the result of a successful authentication. Its fields are private so
// that a consumer cannot build one or alter it after it was accepted.
type Identity struct {
	appName string
	keyID   string
}

// AppName is the app the key belongs to.
func (i Identity) AppName() string { return i.appName }

// KeyID is the public identifier of the key.
func (i Identity) KeyID() string { return i.keyID }

// MarshalJSON renders the public projection of the identity.
func (i Identity) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		AppName string `json:"app_name"`
		KeyID   string `json:"key_id"`
	}{i.appName, i.keyID})
}

// Metadata is the public view of a stored key. A zero time means never.
type Metadata struct {
	ID         string
	App        string
	CreatedAt  time.Time
	LastUsedAt time.Time
	RevokedAt  time.Time
}

// Active reports whether the key has not been revoked.
func (m Metadata) Active() bool { return m.RevokedAt.IsZero() }

// Created is the result of creating a key. Key holds the only copy of the value.
type Created struct {
	Key      Key
	Metadata Metadata
}
