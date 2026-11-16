// Package responsecache keeps exact copies of successful generation responses in
// memory, for one process and for a bounded time. Content never leaves this
// package except through a lease handed to the request that replays it.
package responsecache

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
)

// Kind is how a stored response is replayed.
type Kind uint8

const (
	KindJSON Kind = iota + 1
	KindSSE
)

// keyVersion changes whenever the composition of a key changes, so entries
// built by another algorithm can never be confused with current ones.
const keyVersion = 1

// MaxCapacity is the absolute ceiling of the global budget, in decimal bytes.
const MaxCapacity int64 = 256_000_000

// MaxEntryBytes bounds a single response, metadata included. A larger one is
// still delivered, only not kept.
const MaxEntryBytes int64 = 16_000_000

// Fixed overheads charged on top of the bytes retained, so that many tiny
// entries cannot hold much more memory than the budget says. They are
// conservative estimates of the index node, the list element, the entry and
// each event's slice header, not exact measurements of the Go runtime.
const (
	entryOverhead int64 = 512
	eventOverhead int64 = 48
)

// Key identifies one cached response. It is comparable and opaque: the digest
// covers every component, each with its type and length, and only the app is
// kept apart because invalidation works per app.
type Key struct {
	app    string
	digest [32]byte
}

// App returns the app the key belongs to.
func (k Key) App() string { return k.app }

// String never shows the digest, which is derived from request content.
func (k Key) String() string { return "responsecache.Key{app=" + k.app + "}" }

// GoString keeps %#v as safe as %v.
func (k Key) GoString() string { return k.String() }

// Format keeps every verb safe.
func (k Key) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(k.String())) }

// Shared is the namespace of an app whose users share entries.
const Shared = "shared"

// KeyParts are the components of a key. Namespace is the pseudonymous user
// hash, the anonymous marker or Shared; it is never a raw identifier.
type KeyParts struct {
	Generation   int64
	App          string
	Namespace    string
	Anonymous    bool
	Format       string
	Endpoint     string
	Provider     string
	Model        string
	Stream       bool
	HeaderDigest [32]byte
	BodyDigest   [32]byte
}

// NewKey derives the key of parts.
func NewKey(p KeyParts) Key {
	h := sha256.New()
	writeInt(h, keyVersion)
	writeInt(h, p.Generation)
	for _, s := range []string{p.App, p.Namespace, p.Format, p.Endpoint, p.Provider, p.Model} {
		writeString(h, s)
	}
	writeBool(h, p.Anonymous)
	writeBool(h, p.Stream)
	h.Write(p.HeaderDigest[:])
	h.Write(p.BodyDigest[:])
	var out Key
	out.app = p.App
	copy(out.digest[:], h.Sum(nil))
	return out
}

func writeInt(h hash.Hash, v int64) {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(v))
	h.Write(b[:])
}

func writeString(h hash.Hash, s string) {
	writeInt(h, int64(len(s)))
	h.Write([]byte(s))
}

func writeBool(h hash.Hash, v bool) {
	if v {
		h.Write([]byte{1})
		return
	}
	h.Write([]byte{0})
}

// Stats are safe counters about the cache: no key, no content.
type Stats struct {
	Entries   int
	UsedBytes int64
	MaxBytes  int64
	Leases    int
	Builders  int
}
