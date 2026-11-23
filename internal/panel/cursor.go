package panel

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// MaxCursorBytes bounds a cursor as received.
const MaxCursorBytes = 2048

const cursorVersion = 1

// cursor is the position of a descending page. It names a row by its sort key
// and carries how many rows were already shown, never a filter, a user or a
// pseudonym: the filters are bound by a MAC over their canonical form, so a
// cursor replayed under other filters is refused.
type cursor struct {
	Version int    `json:"v"`
	Kind    string `json:"k"`
	At      int64  `json:"t"`
	ID      string `json:"i"`
	Attempt int    `json:"a,omitempty"`
	Shown   int    `json:"n"`
}

type cursorSigner struct {
	key []byte
}

func newCursorSigner(key []byte) cursorSigner {
	if len(key) != 32 {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			panic("panel: the system random source failed")
		}
	}
	return cursorSigner{key: append([]byte("comporta-panel-cursor:"), key...)}
}

func (s cursorSigner) mac(payload []byte, filters string) []byte {
	m := hmac.New(sha256.New, s.key)
	m.Write(payload)
	m.Write([]byte{0})
	m.Write([]byte(filters))
	return m.Sum(nil)
}

func (s cursorSigner) seal(c cursor, filters string) string {
	c.Version = cursorVersion
	payload, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(s.mac(payload, filters))
}

func (s cursorSigner) open(token, kind, filters string) (cursor, bool) {
	var c cursor
	if token == "" || len(token) > MaxCursorBytes {
		return c, false
	}
	encoded, tag, ok := strings.Cut(token, ".")
	if !ok {
		return c, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return c, false
	}
	presented, err := base64.RawURLEncoding.DecodeString(tag)
	if err != nil || !hmac.Equal(presented, s.mac(payload, filters)) {
		return c, false
	}
	if json.Unmarshal(payload, &c) != nil || c.Version != cursorVersion || c.Kind != kind || c.Shown < 0 || c.Attempt < 0 {
		return c, false
	}
	return c, true
}

func (c cursor) time() time.Time { return time.UnixMicro(c.At).UTC() }
