package keys

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	documentVersion = 1

	// MaxDocumentBytes bounds what is read from keys.json.
	MaxDocumentBytes = 10_000_000
	// MaxRecords bounds the records in keys.json, including revoked ones.
	MaxRecords = 10_000
	// MaxActiveKeysPerApp allows rotation without downtime and nothing beyond it.
	MaxActiveKeysPerApp = 2

	maxAppNameLength = 40
)

type record struct {
	id         string
	app        string
	sha256     string
	createdAt  time.Time
	lastUsedAt time.Time
	revokedAt  time.Time
}

func (r *record) active() bool { return r.revokedAt.IsZero() }

func (r *record) metadata() Metadata {
	return Metadata{ID: r.id, App: r.app, CreatedAt: r.createdAt, LastUsedAt: r.lastUsedAt, RevokedAt: r.revokedAt}
}

type document struct {
	revision int64
	keys     []record
}

func (d *document) activeCount(app string) int {
	count := 0
	for i := range d.keys {
		if d.keys[i].app == app && d.keys[i].active() {
			count++
		}
	}
	return count
}

func (d *document) find(id string) *record {
	for i := range d.keys {
		if d.keys[i].id == id {
			return &d.keys[i]
		}
	}
	return nil
}

func (d *document) hasDigest(digest string) bool {
	for i := range d.keys {
		if d.keys[i].sha256 == digest {
			return true
		}
	}
	return false
}

// tokens walks the document so that duplicate keys and case variants of field
// names are visible: decoding into a struct would silently accept both.
type tokens struct {
	decoder *json.Decoder
}

var errMalformed = errors.New("malformed")

func (t *tokens) delim(want json.Delim) error {
	token, err := t.decoder.Token()
	if err != nil {
		return errMalformed
	}
	if got, ok := token.(json.Delim); !ok || got != want {
		return errMalformed
	}
	return nil
}

// fields calls visit for each member of the object at the cursor and then checks
// that every name in required appeared exactly once.
func (t *tokens) fields(required []string, visit func(name string) error) error {
	if err := t.delim('{'); err != nil {
		return err
	}
	seen := make(map[string]bool, len(required))
	for t.decoder.More() {
		token, err := t.decoder.Token()
		if err != nil {
			return errMalformed
		}
		name, ok := token.(string)
		if !ok || seen[name] {
			return errMalformed
		}
		seen[name] = true
		if err := visit(name); err != nil {
			return err
		}
	}
	if err := t.delim('}'); err != nil {
		return err
	}
	for _, name := range required {
		if !seen[name] {
			return errMalformed
		}
	}
	return nil
}

func (t *tokens) integer() (int64, error) {
	token, err := t.decoder.Token()
	if err != nil {
		return 0, errMalformed
	}
	number, ok := token.(json.Number)
	if !ok {
		return 0, errMalformed
	}
	value, err := strconv.ParseInt(number.String(), 10, 64)
	if err != nil {
		return 0, errMalformed
	}
	return value, nil
}

func (t *tokens) text() (string, error) {
	token, err := t.decoder.Token()
	if err != nil {
		return "", errMalformed
	}
	text, ok := token.(string)
	if !ok {
		return "", errMalformed
	}
	return text, nil
}

func (t *tokens) timestamp() (time.Time, error) {
	text, err := t.text()
	if err != nil {
		return time.Time{}, err
	}
	return parseUTC(text)
}

func (t *tokens) optionalTimestamp() (time.Time, error) {
	token, err := t.decoder.Token()
	if err != nil {
		return time.Time{}, errMalformed
	}
	if token == nil {
		return time.Time{}, nil
	}
	text, ok := token.(string)
	if !ok {
		return time.Time{}, errMalformed
	}
	return parseUTC(text)
}

func parseUTC(text string) (time.Time, error) {
	if !strings.HasSuffix(text, "Z") {
		return time.Time{}, errMalformed
	}
	at, err := time.Parse(time.RFC3339Nano, text)
	if err != nil {
		return time.Time{}, errMalformed
	}
	return at, nil
}

func parseDocument(data []byte) (*document, error) {
	if len(data) > MaxDocumentBytes || !utf8.Valid(data) {
		return nil, ErrStoreCorrupt
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	t := &tokens{decoder: decoder}

	doc := &document{}
	var version int64
	err := t.fields([]string{"version", "revision", "keys"}, func(name string) (err error) {
		switch name {
		case "version":
			version, err = t.integer()
		case "revision":
			doc.revision, err = t.integer()
		case "keys":
			err = t.records(doc)
		default:
			err = errMalformed
		}
		return err
	})
	if err != nil {
		return nil, ErrStoreCorrupt
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, ErrStoreCorrupt
	}
	if version != documentVersion || doc.revision < 1 || !doc.valid() {
		return nil, ErrStoreCorrupt
	}
	return doc, nil
}

func (t *tokens) records(doc *document) error {
	if err := t.delim('['); err != nil {
		return err
	}
	for t.decoder.More() {
		if len(doc.keys) == MaxRecords {
			return errMalformed
		}
		var r record
		err := t.fields([]string{"id", "app", "sha256", "created_at", "last_used_at", "revoked_at"}, func(name string) (err error) {
			switch name {
			case "id":
				r.id, err = t.text()
			case "app":
				r.app, err = t.text()
			case "sha256":
				r.sha256, err = t.text()
			case "created_at":
				r.createdAt, err = t.timestamp()
			case "last_used_at":
				r.lastUsedAt, err = t.optionalTimestamp()
			case "revoked_at":
				r.revokedAt, err = t.optionalTimestamp()
			default:
				err = errMalformed
			}
			return err
		})
		if err != nil {
			return err
		}
		doc.keys = append(doc.keys, r)
	}
	return t.delim(']')
}

func (d *document) valid() bool {
	ids := make(map[string]bool, len(d.keys))
	digests := make(map[string]bool, len(d.keys))
	active := make(map[string]int)
	for i := range d.keys {
		r := &d.keys[i]
		if !validID(r.id) || !validAppName(r.app) || !isLowerHex(r.sha256, digestSize*2) || ids[r.id] || digests[r.sha256] {
			return false
		}
		if !r.lastUsedAt.IsZero() && r.lastUsedAt.Before(r.createdAt) {
			return false
		}
		if !r.revokedAt.IsZero() && r.revokedAt.Before(r.createdAt) {
			return false
		}
		ids[r.id], digests[r.sha256] = true, true
		if r.active() {
			active[r.app]++
			if active[r.app] > MaxActiveKeysPerApp {
				return false
			}
		}
	}
	return true
}

func validID(id string) bool {
	return strings.HasPrefix(id, idPrefix) && isLowerHex(id[len(idPrefix):], idEntropy*2)
}

func validAppName(name string) bool {
	if name == "" || len(name) > maxAppNameLength {
		return false
	}
	for i := range len(name) {
		c := name[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

func isLowerHex(text string, length int) bool {
	if len(text) != length {
		return false
	}
	for i := range len(text) {
		c := text[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

type wireRecord struct {
	ID         string  `json:"id"`
	App        string  `json:"app"`
	SHA256     string  `json:"sha256"`
	CreatedAt  string  `json:"created_at"`
	LastUsedAt *string `json:"last_used_at"`
	RevokedAt  *string `json:"revoked_at"`
}

func formatUTC(at time.Time) string { return at.UTC().Format(time.RFC3339Nano) }

func optionalUTC(at time.Time) *string {
	if at.IsZero() {
		return nil
	}
	text := formatUTC(at)
	return &text
}

func (d *document) encode() ([]byte, error) {
	wire := struct {
		Version  int          `json:"version"`
		Revision int64        `json:"revision"`
		Keys     []wireRecord `json:"keys"`
	}{Version: documentVersion, Revision: d.revision, Keys: make([]wireRecord, 0, len(d.keys))}
	for i := range d.keys {
		r := &d.keys[i]
		wire.Keys = append(wire.Keys, wireRecord{
			ID: r.id, App: r.app, SHA256: r.sha256, CreatedAt: formatUTC(r.createdAt),
			LastUsedAt: optionalUTC(r.lastUsedAt), RevokedAt: optionalUTC(r.revokedAt),
		})
	}
	data, err := json.MarshalIndent(wire, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

// nextRevision refuses to wrap around, which would make an old revision look new.
func (d *document) nextRevision() error {
	if d.revision == math.MaxInt64 {
		return ErrStoreFull
	}
	d.revision++
	return nil
}

func decodeDigest(text string) (Digest, error) {
	var digest Digest
	if _, err := hex.Decode(digest[:], []byte(text)); err != nil {
		return Digest{}, ErrStoreCorrupt
	}
	return digest, nil
}
