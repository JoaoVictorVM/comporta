package keys

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestGenerateKeyFormatAndLeadingZeros(t *testing.T) {
	key, err := generateKey(bytes.NewReader(make([]byte, keyEntropy)))
	if err != nil {
		t.Fatal(err)
	}
	if want := "cmp_" + strings.Repeat("0", 43); key.Reveal() != want {
		t.Fatalf("leading zeros must be preserved, got %q", key.Reveal())
	}

	small := make([]byte, keyEntropy)
	small[keyEntropy-1] = 61
	key, err = generateKey(bytes.NewReader(small))
	if err != nil {
		t.Fatal(err)
	}
	if want := "cmp_" + strings.Repeat("0", 42) + "z"; key.Reveal() != want {
		t.Fatalf("got %q, want %q", key.Reveal(), want)
	}

	for range 200 {
		key, err := generateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		value := key.Reveal()
		if len(value) != 47 || !strings.HasPrefix(value, "cmp_") {
			t.Fatalf("bad key shape: %d bytes", len(value))
		}
		if strings.Trim(value[4:], base62Alphabet) != "" {
			t.Fatal("key leaves the base62 alphabet")
		}
	}
}

func TestGenerateKeyRoundTrip256Bits(t *testing.T) {
	for _, raw := range [][]byte{
		make([]byte, keyEntropy),
		bytes.Repeat([]byte{0xff}, keyEntropy),
		append(make([]byte, keyEntropy-1), 1),
		append([]byte{0x80}, make([]byte, keyEntropy-1)...),
	} {
		key, err := generateKey(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		if got := decodeBase62(key.Reveal()[len(keyPrefix):]); !bytes.Equal(got, raw) {
			t.Fatalf("round trip lost bits: %x != %x", got, raw)
		}
		if _, err := ParseKey(key.Reveal()); err != nil {
			t.Fatalf("generated key must parse: %v", err)
		}
	}
	for range 200 {
		raw := make([]byte, keyEntropy)
		if _, err := rand.Read(raw); err != nil {
			t.Fatal(err)
		}
		if got := decodeBase62(encodeBase62(raw)); !bytes.Equal(got, raw) {
			t.Fatal("round trip lost bits")
		}
	}
}

func TestParseKeyRejectsInvalidSyntax(t *testing.T) {
	overflow := "cmp_" + strings.Repeat("z", 43)
	valid := "cmp_" + strings.Repeat("A", 43)
	cases := map[string]string{
		"empty":      "",
		"no prefix":  strings.Repeat("A", 47),
		"short":      "cmp_" + strings.Repeat("A", 42),
		"long":       "cmp_" + strings.Repeat("A", 44),
		"wrong case": "CMP_" + strings.Repeat("A", 43),
		"symbol":     "cmp_" + strings.Repeat("A", 42) + "-",
		"over 256":   overflow,
		"multibyte":  "cmp_" + strings.Repeat("A", 41) + "é",
	}
	for name, presented := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseKey(presented); !errors.Is(err, ErrInvalidKey) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	if _, err := ParseKey(valid); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseKey("cmp_" + maxEncoded); err != nil {
		t.Fatalf("2^256-1 must be accepted: %v", err)
	}
}

func TestGeneratedIDIsIndependentFromKey(t *testing.T) {
	entropy := rand.Reader
	id, err := generateID(entropy)
	if err != nil {
		t.Fatal(err)
	}
	if !validID(id) {
		t.Fatalf("bad id %q", id)
	}
	key, err := generateKey(entropy)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(key.Reveal(), id[len(idPrefix):]) {
		t.Fatal("id must not derive from the key")
	}
}

func TestEntropyFailureIsReported(t *testing.T) {
	if _, err := generateKey(bytes.NewReader(nil)); !errors.Is(err, ErrEntropy) {
		t.Fatalf("err = %v", err)
	}
	if _, err := generateID(bytes.NewReader(nil)); !errors.Is(err, ErrEntropy) {
		t.Fatalf("err = %v", err)
	}
}

func TestSecretsNeverReachGenericOutput(t *testing.T) {
	key, err := generateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	digest := key.Digest()
	hexDigest := digestHex(digest)

	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("event", "key", key, "digest", digest)

	output := []string{
		fmt.Sprint(key), fmt.Sprintf("%v %+v %#v %s %q %x", key, key, key, key, key, key),
		fmt.Sprint(digest), fmt.Sprintf("%v %+v %#v %s %q %x", digest, digest, digest, digest, digest, digest),
		fmt.Sprintf("%+v", struct {
			K Key
			D Digest
		}{key, digest}),
		logs.String(),
	}
	for _, text := range output {
		if strings.Contains(text, key.Reveal()) || strings.Contains(text, hexDigest) {
			t.Fatalf("secret exposed in %q", text)
		}
	}
	if _, err := json.Marshal(key); err == nil {
		t.Fatal("key must not serialize")
	}
	if _, err := json.Marshal(digest); err == nil {
		t.Fatal("digest must not serialize")
	}
	var target Key
	if err := json.Unmarshal([]byte(`"value"`), &target); err == nil {
		t.Fatal("key must not deserialize")
	}
}

func TestIdentityPublicProjection(t *testing.T) {
	identity := Identity{appName: "bot-discord", keyID: "k_11111111111111111111111111111111"}
	data, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"app_name":"bot-discord","key_id":"k_11111111111111111111111111111111"}`
	if string(data) != want {
		t.Fatalf("got %s", data)
	}
}
