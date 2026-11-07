// Package useridentity turns the end-user identifiers sent by an app into stable,
// app-scoped pseudonyms. It knows nothing about HTTP, authentication or storage:
// callers bring an app that was already authenticated and the value they selected.
package useridentity

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"log/slog"
)

// HashHexLength is the size of the canonical pseudonym: 16 bytes in lowercase hex.
const HashHexLength = 32

const (
	hashBytes = HashHexLength / 2
	keyBytes  = 32

	// The domain is versioned so that a later change of framing cannot produce a
	// pseudonym equal to one computed under this one.
	domain = "comporta:user:v1\x00"

	kindAnonymous  byte = 0
	kindIdentified byte = 1

	redactedHash    = "[user hash]"
	redactedService = "[user identity service]"
)

// Failures of a request's identity. Their messages are fixed: none carries the value
// that caused it.
var (
	ErrUserRequired = errors.New("this app requires a user ID (X-Comporta-User header)")
	ErrUserInvalid  = errors.New("user ID must be 1-128 characters of [A-Za-z0-9_.:-]")
	ErrUserEmail    = errors.New("user ID looks like an email address; send an opaque ID instead")
	ErrUserDocument = errors.New("user ID looks like a CPF or CNPJ; send an opaque ID instead")
)

// Hash is a pseudonym built only by a Service. It is sensitive even though it is not
// the raw identifier, so generic representations hide it; Hex is the explicit way to
// read it, for the provider field and for what later stores it.
type Hash struct {
	hex string
}

// Hex returns the 32 lowercase hexadecimal characters that identify the user.
func (h Hash) Hex() string { return h.hex }

// IsZero reports whether no pseudonym was computed.
func (h Hash) IsZero() bool { return h.hex == "" }

// Format hides the value for every verb.
func (h Hash) Format(state fmt.State, _ rune) { _, _ = fmt.Fprint(state, redactedHash) }

// LogValue hides the value from structured logs.
func (h Hash) LogValue() slog.Value { return slog.StringValue(redactedHash) }

// Identity is the resolved user of a request. Anonymous is a separate flag, so that a
// user explicitly called "anonymous" never merges with requests that sent no user.
type Identity struct {
	hash      Hash
	anonymous bool
	resolved  bool
}

// Hash returns the pseudonym of the user.
func (i Identity) Hash() Hash { return i.hash }

// Anonymous reports whether the request sent no user and the app allowed that.
func (i Identity) Anonymous() bool { return i.anonymous }

// Resolved tells an identity already processed from the zero value.
func (i Identity) Resolved() bool { return i.resolved }

// Format hides the pseudonym for every verb: fmt does not call the methods of the
// unexported field, so Hash alone would not protect it here.
func (i Identity) Format(state fmt.State, _ rune) {
	_, _ = fmt.Fprintf(state, "{hash:%s anonymous:%t}", redactedHash, i.anonymous)
}

// LogValue hides the pseudonym from structured logs.
func (i Identity) LogValue() slog.Value {
	return slog.GroupValue(slog.String("hash", redactedHash), slog.Bool("anonymous", i.anonymous))
}

// Service computes pseudonyms with a key that never leaves it. It is immutable after
// Open and safe for concurrent use.
type Service struct {
	key [keyBytes]byte
}

// Format hides the key for every verb.
func (s *Service) Format(state fmt.State, _ rune) { _, _ = fmt.Fprint(state, redactedService) }

// LogValue hides the key from structured logs.
func (s *Service) LogValue() slog.Value { return slog.StringValue(redactedService) }

// Resolve applies the rules of one request: value is what the caller selected by
// precedence and present says whether any source was sent at all.
func (s *Service) Resolve(app, value string, present, required bool) (Identity, error) {
	if !present {
		if required {
			return Identity{}, ErrUserRequired
		}
		return Identity{hash: s.AnonymousHash(app), anonymous: true, resolved: true}, nil
	}
	userHash, err := s.LookupHash(app, value)
	if err != nil {
		return Identity{}, err
	}
	return Identity{hash: userHash, resolved: true}, nil
}

// LookupHash applies the same validation and calculation as a request, for search
// and deletion. The app may no longer exist in the configuration; a search across
// apps has to enumerate the apps in its own history and call this for each one.
func (s *Service) LookupHash(app, id string) (Hash, error) {
	if err := ValidateID(id); err != nil {
		return Hash{}, err
	}
	return s.compute(app, kindIdentified, id), nil
}

// AnonymousHash returns the pseudonym shared by the anonymous requests of an app.
func (s *Service) AnonymousHash(app string) Hash {
	return s.compute(app, kindAnonymous, "")
}

func (s *Service) compute(app string, kind byte, id string) Hash {
	mac := hmac.New(sha256.New, s.key[:])
	mac.Write([]byte(domain))
	writeFramed(mac, app)
	mac.Write([]byte{kind})
	writeFramed(mac, id)
	return Hash{hex: hex.EncodeToString(mac.Sum(nil)[:hashBytes])}
}

// writeFramed prefixes the length, which keeps two different pairs of app and ID from
// concatenating into the same input.
func writeFramed(mac hash.Hash, value string) {
	mac.Write(binary.BigEndian.AppendUint32(nil, uint32(len(value))))
	mac.Write([]byte(value))
}
