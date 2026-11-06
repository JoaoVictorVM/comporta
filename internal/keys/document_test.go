package keys

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

const (
	testID     = "k_11111111111111111111111111111111"
	testDigest = "0000000000000000000000000000000000000000000000000000000000000000"
)

func recordJSON(id, app, digest, revoked string) string {
	return fmt.Sprintf(`{"id":%q,"app":%q,"sha256":%q,"created_at":"2026-11-06T12:00:00Z","last_used_at":null,"revoked_at":%s}`,
		id, app, digest, revoked)
}

func docJSON(records ...string) string {
	return `{"version":1,"revision":1,"keys":[` + strings.Join(records, ",") + `]}`
}

func numberedID(i int) string     { return fmt.Sprintf("k_%032x", i) }
func numberedDigest(i int) string { return fmt.Sprintf("%064x", i) }

func TestDocumentRoundTrip(t *testing.T) {
	created := time.Date(2026, 11, 6, 12, 0, 0, 123456789, time.UTC)
	doc := &document{revision: 7, keys: []record{
		{id: testID, app: "bot", sha256: testDigest, createdAt: created},
		{id: numberedID(2), app: "bot", sha256: numberedDigest(2), createdAt: created,
			lastUsedAt: created.Add(time.Hour), revokedAt: created.Add(2 * time.Hour)},
	}}
	data, err := doc.encode()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseDocument(data)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.revision != 7 || len(parsed.keys) != 2 {
		t.Fatalf("parsed %+v", parsed)
	}
	for i := range doc.keys {
		if parsed.keys[i] != doc.keys[i] {
			t.Fatalf("record %d: %+v != %+v", i, parsed.keys[i], doc.keys[i])
		}
	}
}

func TestStoreRejectsInvalidDocuments(t *testing.T) {
	valid := recordJSON(testID, "bot", testDigest, "null")
	second := recordJSON(numberedID(2), "bot", numberedDigest(2), "null")
	third := recordJSON(numberedID(3), "bot", numberedDigest(3), "null")
	cases := map[string]string{
		"empty bytes":       "",
		"whitespace":        "  \n",
		"not json":          "keys",
		"array root":        "[]",
		"null root":         "null",
		"trailing document": docJSON() + docJSON(),
		"trailing garbage":  docJSON() + "x",
		"unknown version":   `{"version":2,"revision":1,"keys":[]}`,
		"string version":    `{"version":"1","revision":1,"keys":[]}`,
		"missing version":   `{"revision":1,"keys":[]}`,
		"missing revision":  `{"version":1,"keys":[]}`,
		"missing keys":      `{"version":1,"revision":1}`,
		"null keys":         `{"version":1,"revision":1,"keys":null}`,
		"zero revision":     `{"version":1,"revision":0,"keys":[]}`,
		"negative revision": `{"version":1,"revision":-1,"keys":[]}`,
		"float revision":    `{"version":1,"revision":1.5,"keys":[]}`,
		"revision overflow": `{"version":1,"revision":9223372036854775808,"keys":[]}`,
		"unknown root":      `{"version":1,"revision":1,"keys":[],"extra":1}`,
		"case variant":      `{"Version":1,"revision":1,"keys":[]}`,
		"duplicate root":    `{"version":1,"version":1,"revision":1,"keys":[]}`,
		"invalid utf8":      "{\"version\":1,\"revision\":1,\"keys\":[],\"\xff\":1}",
		"record not object": docJSON("1"),
		"unknown field":     docJSON(strings.Replace(valid, `"id"`, `"extra":1,"id"`, 1)),
		"duplicate field":   docJSON(strings.Replace(valid, `"id"`, `"app":"bot","id"`, 1)),
		"missing last used": docJSON(strings.Replace(valid, `"last_used_at":null,`, "", 1)),
		"null id":           docJSON(strings.Replace(valid, `"`+testID+`"`, "null", 1)),
		"null created":      docJSON(strings.Replace(valid, `"2026-11-06T12:00:00Z"`, "null", 1)),
		"bad id":            docJSON(recordJSON("k_XYZ", "bot", testDigest, "null")),
		"upper id":          docJSON(recordJSON("k_"+strings.Repeat("A", 32), "bot", testDigest, "null")),
		"bad app":           docJSON(recordJSON(testID, "Bot", testDigest, "null")),
		"long app":          docJSON(recordJSON(testID, strings.Repeat("a", 41), testDigest, "null")),
		"short digest":      docJSON(recordJSON(testID, "bot", "abc", "null")),
		"upper digest":      docJSON(recordJSON(testID, "bot", strings.Repeat("A", 64), "null")),
		"offset time":       docJSON(strings.Replace(valid, "12:00:00Z", "12:00:00+00:00", 1)),
		"bad date":          docJSON(strings.Replace(valid, "2026-11-06", "2026-13-06", 1)),
		"revoked too early": docJSON(recordJSON(testID, "bot", testDigest, `"2020-01-01T00:00:00Z"`)),
		"used too early":    docJSON(strings.Replace(valid, `"last_used_at":null`, `"last_used_at":"2020-01-01T00:00:00Z"`, 1)),
		"duplicate id":      docJSON(valid, recordJSON(testID, "other", numberedDigest(2), "null")),
		"duplicate digest":  docJSON(valid, recordJSON(numberedID(2), "other", testDigest, "null")),
		"three active":      docJSON(valid, second, third),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseDocument([]byte(input)); !errors.Is(err, ErrStoreCorrupt) {
				t.Fatalf("err = %v", err)
			}
		})
	}

	t.Run("too large", func(t *testing.T) {
		data := []byte(docJSON() + strings.Repeat(" ", MaxDocumentBytes))
		if _, err := parseDocument(data); !errors.Is(err, ErrStoreCorrupt) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("too many records", func(t *testing.T) {
		records := make([]string, MaxRecords+1)
		for i := range records {
			records[i] = recordJSON(numberedID(i), "bot", numberedDigest(i), `"2026-11-06T13:00:00Z"`)
		}
		if _, err := parseDocument([]byte(docJSON(records...))); !errors.Is(err, ErrStoreCorrupt) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestDocumentAcceptsEdgeCases(t *testing.T) {
	cases := map[string]string{
		"empty keys":       docJSON(),
		"only revoked":     docJSON(recordJSON(testID, "bot", testDigest, `"2026-11-06T13:00:00Z"`)),
		"app not in yaml":  docJSON(recordJSON(testID, "removed-app", testDigest, "null")),
		"two active":       docJSON(recordJSON(testID, "bot", testDigest, "null"), recordJSON(numberedID(2), "bot", numberedDigest(2), "null")),
		"reordered fields": `{"keys":[],"revision":3,"version":1}`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseDocument([]byte(input)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRevisionOverflowRefusesMutation(t *testing.T) {
	doc := &document{revision: 1<<63 - 1}
	if err := doc.nextRevision(); !errors.Is(err, ErrStoreFull) {
		t.Fatalf("err = %v", err)
	}
	if doc.revision != 1<<63-1 {
		t.Fatal("revision must not wrap")
	}
}
