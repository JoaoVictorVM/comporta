package useridentity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// testKey is synthetic: the bytes 0 to 31.
func testKey() []byte {
	key := make([]byte, keyBytes)
	for i := range key {
		key[i] = byte(i)
	}
	return key
}

// The expected values were calculated outside this code, with Python's hmac module over
// the documented framing, so that they check the format and not just self-consistency.
func TestHMACIndependentVectors(t *testing.T) {
	service := newService(testKey())
	cases := []struct {
		app, id, want string
	}{
		{"bot", "user_123", "9c6435c3e9046ddeb011ed47369f79df"},
		{"other", "user_123", "e1ddf31536dc3c32e6feb9ffe14c73fe"},
		{"bot", "User_123", "69a64d2a354d3e61991242fdc1c07456"},
		{"bot", "anonymous", "df133d03324a732e9b9af339fd62d78c"},
	}
	for _, tc := range cases {
		got, err := service.LookupHash(tc.app, tc.id)
		if err != nil {
			t.Fatal(err)
		}
		if got.Hex() != tc.want {
			t.Errorf("%s/%s: got %s, want %s", tc.app, tc.id, got.Hex(), tc.want)
		}
	}
	if got := service.AnonymousHash("bot").Hex(); got != "914310b4c55242cdde08d187b884e890" {
		t.Errorf("anonymous: got %s", got)
	}
}

func TestHashShapeIsCanonical(t *testing.T) {
	canonical := regexp.MustCompile(`^[0-9a-f]{32}$`)
	service := newService(testKey())
	identified, _ := service.LookupHash("bot", "user_123")
	for _, hash := range []Hash{identified, service.AnonymousHash("bot")} {
		if !canonical.MatchString(hash.Hex()) || len(hash.Hex()) != HashHexLength {
			t.Fatalf("hash %q", hash.Hex())
		}
	}
}

func TestHashScopedByApp(t *testing.T) {
	service := newService(testKey())
	first, _ := service.LookupHash("bot", "user_123")
	second, _ := service.LookupHash("other", "user_123")
	if first == second {
		t.Fatal("the same user in two apps must not correlate")
	}
	if service.AnonymousHash("bot") == service.AnonymousHash("other") {
		t.Fatal("anonymous users of two apps must not correlate")
	}
	// Framing: moving a byte between the app and the ID changes the input.
	moved, _ := service.LookupHash("botu", "ser_123")
	shifted, _ := service.LookupHash("bot", "user_123")
	if moved == shifted {
		t.Fatal("app and ID boundaries must be part of the input")
	}
}

func TestAnonymousDomainSeparated(t *testing.T) {
	service := newService(testKey())
	anonymous, err := service.Resolve("bot", "", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if !anonymous.Anonymous() || !anonymous.Resolved() || anonymous.Hash().IsZero() {
		t.Fatalf("anonymous %+v", anonymous)
	}
	named, err := service.Resolve("bot", "anonymous", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if named.Anonymous() || named.Hash() == anonymous.Hash() {
		t.Fatal("a user named anonymous must not merge with requests without a user")
	}
	again, _ := service.Resolve("bot", "", false, false)
	if again.Hash() != anonymous.Hash() {
		t.Fatal("anonymous requests of an app must share a hash")
	}
}

func TestResolveRules(t *testing.T) {
	service := newService(testKey())
	if _, err := service.Resolve("bot", "", false, true); !errors.Is(err, ErrUserRequired) {
		t.Fatalf("required: %v", err)
	}
	if _, err := service.Resolve("bot", "", true, false); !errors.Is(err, ErrUserInvalid) {
		t.Fatalf("a present empty value is not an absence: %v", err)
	}
	if _, err := service.Resolve("bot", "joao@mail.com", true, false); !errors.Is(err, ErrUserEmail) {
		t.Fatalf("email: %v", err)
	}
	identity, err := service.Resolve("bot", "user_123", true, true)
	if err != nil || identity.Anonymous() || !identity.Resolved() {
		t.Fatalf("identity %+v, %v", identity, err)
	}
	if (Identity{}).Resolved() {
		t.Fatal("the zero value must not look resolved")
	}
}

func TestHashStableAcrossRestartAndAppKeyRotation(t *testing.T) {
	dir := t.TempDir()
	first := mustOpen(t, dir)
	before, _ := first.LookupHash("bot", "user_123")
	// App keys live in another file that has no part in the calculation.
	if err := os.WriteFile(filepath.Join(dir, "keys.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	second := mustOpen(t, dir)
	after, _ := second.LookupHash("bot", "user_123")
	if before != after {
		t.Fatal("the pseudonym changed after a restart")
	}
}

func TestLookupMatchesRequestHash(t *testing.T) {
	service := newService(testKey())
	resolved, err := service.Resolve("bot", "user_123", true, false)
	if err != nil {
		t.Fatal(err)
	}
	found, err := service.LookupHash("bot", "user_123")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Hash() != found {
		t.Fatal("a search would not find the record")
	}
	if _, err := service.LookupHash("bot", "joao@mail.com"); !errors.Is(err, ErrUserEmail) {
		t.Fatalf("lookup must validate too: %v", err)
	}
}

func TestRepresentationsHideKeyAndHash(t *testing.T) {
	key := testKey()
	service := newService(key)
	identity, _ := service.Resolve("bot", "user_123", true, false)
	hexValue := identity.Hash().Hex()
	marshaled, err := json.Marshal(identity)
	if err != nil {
		t.Fatal(err)
	}
	var logged strings.Builder
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("x", "hash", identity.Hash(), "service", service)
	var text strings.Builder
	slog.New(slog.NewTextHandler(&text, nil)).Info("x", "hash", identity.Hash(), "service", service)
	for name, value := range map[string]string{
		"json": string(marshaled), "slog": logged.String(), "text": text.String(),
		"v": fmt.Sprintf("%v", identity), "plus": fmt.Sprintf("%+v", identity), "gosyn": fmt.Sprintf("%#v", identity),
		"hash v": fmt.Sprintf("%v", identity.Hash()), "hash s": fmt.Sprintf("%s", identity.Hash()),
		"service v": fmt.Sprintf("%v", service), "service plus": fmt.Sprintf("%+v", service), "service gosyn": fmt.Sprintf("%#v", service),
	} {
		if strings.Contains(value, hexValue) {
			t.Errorf("%s exposes the hash: %s", name, value)
		}
		if strings.Contains(value, fmt.Sprint(key[1:8])) || strings.Contains(value, "0x1f") {
			t.Errorf("%s exposes the key: %s", name, value)
		}
	}
}

func mustOpen(t *testing.T, dir string) *Service {
	t.Helper()
	service, err := Open(context.Background(), Options{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	return service
}
