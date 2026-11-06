package keys

import (
	"errors"
	"strings"
	"testing"
)

func FuzzKeyDocument(f *testing.F) {
	f.Add([]byte(docJSON()))
	f.Add([]byte(docJSON(recordJSON(testID, "bot", testDigest, "null"))))
	f.Add([]byte(`{"version":1,"version":1}`))
	f.Add([]byte(`[[[[[[`))
	f.Add([]byte{0xff, 0xfe})
	f.Fuzz(func(t *testing.T, data []byte) {
		doc, err := parseDocument(data)
		if err != nil {
			if !errors.Is(err, ErrStoreCorrupt) || err.Error() != ErrStoreCorrupt.Error() {
				t.Fatalf("errors must come from the fixed vocabulary: %v", err)
			}
			return
		}
		encoded, err := doc.encode()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parseDocument(encoded); err != nil {
			t.Fatalf("an accepted document must survive a round trip: %v", err)
		}
	})
}

func FuzzKeySyntax(f *testing.F) {
	f.Add("cmp_" + strings.Repeat("0", 43))
	f.Add("cmp_" + strings.Repeat("z", 43))
	f.Add("cmp_" + maxEncoded)
	f.Add("")
	f.Add("cmp_\x00")
	f.Fuzz(func(t *testing.T, presented string) {
		key, err := ParseKey(presented)
		if err != nil {
			if err.Error() != ErrInvalidKey.Error() {
				t.Fatalf("error must be generic: %v", err)
			}
			return
		}
		raw := decodeBase62(presented[len(keyPrefix):])
		if encodeBase62(raw) != presented[len(keyPrefix):] {
			t.Fatal("accepted key must represent 256 bits exactly")
		}
		if key.Reveal() != presented {
			t.Fatal("parsing must preserve the key")
		}
	})
}
