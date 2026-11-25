package translation

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"regexp"
)

// maxIDLength keeps converted identifiers within what every protocol accepts.
const maxIDLength = 64

var portableID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// ToolID returns an identifier every protocol accepts for a tool call. A
// portable identifier is kept as it is: the gateway holds no state between
// requests, so the client must be able to send back the same value on its next
// turn, whichever provider answers it. Anything else becomes a deterministic
// synthetic value, so the call and its result keep matching within a request
// and across requests.
func ToolID(id string) string {
	if portableID.MatchString(id) {
		return id
	}
	sum := sha256.Sum256([]byte(id))
	return "call_" + hex.EncodeToString(sum[:12])
}

// newID returns a fresh identifier for objects the gateway renders itself,
// such as a message or a response.
func newID(prefix string) string {
	var raw [12]byte
	_, _ = rand.Read(raw[:])
	return prefix + hex.EncodeToString(raw[:])
}

// IDMap correlates the identifiers of one operation. It is bounded by the
// number of calls in the body and lives only as long as the operation.
type IDMap struct {
	limit int
	ids   map[string]string
}

// NewIDMap returns a map that accepts up to limit distinct identifiers.
func NewIDMap(limit int) *IDMap { return &IDMap{limit: limit, ids: map[string]string{}} }

// Map returns the portable identifier of id, the same one every time. It
// reports false when the bound would be exceeded.
func (m *IDMap) Map(id string) (string, bool) {
	if mapped, ok := m.ids[id]; ok {
		return mapped, true
	}
	if len(m.ids) >= m.limit {
		return "", false
	}
	mapped := ToolID(id)
	m.ids[id] = mapped
	return mapped, true
}

// Len reports how many identifiers are mapped.
func (m *IDMap) Len() int { return len(m.ids) }
