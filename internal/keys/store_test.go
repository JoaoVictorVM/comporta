package keys

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	store := NewStore(filepath.Join(t.TempDir(), "data"))
	store.clock = newFakeClock()
	return store
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func mustCreate(t *testing.T, store *Store, app string) Created {
	t.Helper()
	created, err := store.Create(context.Background(), app)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

func TestStorePersistsOnlyDigest(t *testing.T) {
	store := newTestStore(t)
	created := mustCreate(t, store, "bot")

	content := readFile(t, store.documentPath())
	if strings.Contains(content, created.Key.Reveal()) || strings.Contains(content, created.Key.Reveal()[4:]) {
		t.Fatal("the key must not be stored")
	}
	if !strings.Contains(content, digestHex(created.Key.Digest())) {
		t.Fatal("the digest must be stored")
	}
	if !strings.Contains(content, created.Metadata.ID) {
		t.Fatal("the id must be stored")
	}
	entries, err := os.ReadDir(store.dir)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if len(names) != 2 || names[0] != "keys.json" || names[1] != "keys.lock" {
		t.Fatalf("only the document and lock may remain, found %v", names)
	}
	if lock := readFile(t, store.lockPath()); lock != "" {
		t.Fatalf("the lock file must hold nothing: %q", lock)
	}
}

func TestStoreRejectsThirdActiveKey(t *testing.T) {
	store := newTestStore(t)
	first := mustCreate(t, store, "bot")
	mustCreate(t, store, "bot")
	mustCreate(t, store, "other")

	_, err := store.Create(context.Background(), "bot")
	var limit *ActiveLimitError
	if !errors.As(err, &limit) {
		t.Fatalf("err = %v", err)
	}
	if want := "bot already has 2 active keys; revoke one before rotating"; err.Error() != want {
		t.Fatalf("message %q", err)
	}
	if _, err := store.Revoke(context.Background(), first.Metadata.ID); err != nil {
		t.Fatal(err)
	}
	mustCreate(t, store, "bot")

	entries, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("the revoked record must remain: %d", len(entries))
	}
}

func TestRotatePreservesPreviousKey(t *testing.T) {
	store := newTestStore(t)
	first, err := store.Rotate(context.Background(), "bot")
	if err != nil {
		t.Fatalf("rotate must work without a previous key: %v", err)
	}
	second, err := store.Rotate(context.Background(), "bot")
	if err != nil {
		t.Fatal(err)
	}
	if first.Key.Reveal() == second.Key.Reveal() || first.Metadata.ID == second.Metadata.ID {
		t.Fatal("rotation must produce a new key")
	}
	entries, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || !entries[0].Active() || !entries[1].Active() {
		t.Fatalf("both keys must remain active: %+v", entries)
	}
	if _, err := store.Rotate(context.Background(), "bot"); err == nil {
		t.Fatal("rotation respects the limit")
	}
}

func TestRevokeIsIdempotent(t *testing.T) {
	store := newTestStore(t)
	created := mustCreate(t, store, "bot")
	store.clock.(*fakeClock).advance(time.Hour)

	changed, err := store.Revoke(context.Background(), created.Metadata.ID)
	if err != nil || !changed {
		t.Fatalf("changed %v err %v", changed, err)
	}
	before := readFile(t, store.documentPath())
	store.clock.(*fakeClock).advance(time.Hour)
	changed, err = store.Revoke(context.Background(), created.Metadata.ID)
	if err != nil || changed {
		t.Fatalf("second revoke: changed %v err %v", changed, err)
	}
	if after := readFile(t, store.documentPath()); after != before {
		t.Fatal("a repeated revoke must not write")
	}
	if _, err := store.Revoke(context.Background(), "k_00000000000000000000000000000000"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("err = %v", err)
	}
	if _, err := store.Revoke(context.Background(), "not-an-id"); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestMissingStoreRequiresExplicitProvisioning(t *testing.T) {
	store := newTestStore(t)
	entries, err := store.List()
	if err != nil || len(entries) != 0 {
		t.Fatalf("entries %v err %v", entries, err)
	}
	if _, err := store.Revoke(context.Background(), testID); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("err = %v", err)
	}
	if err := store.mergeLastUsed(context.Background(), map[string]time.Time{testID: time.Now()}); !errors.Is(err, ErrStoreMissing) {
		t.Fatalf("err = %v", err)
	}
	if _, err := os.Stat(store.dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("only creating a key may initialize the store")
	}
	mustCreate(t, store, "bot")
	if _, err := os.Stat(store.documentPath()); err != nil {
		t.Fatal(err)
	}
}

func TestCorruptStoreIsNeverRecreated(t *testing.T) {
	for name, content := range map[string]string{"empty bytes": "", "garbage": "{not json"} {
		t.Run(name, func(t *testing.T) {
			store := newTestStore(t)
			mustCreate(t, store, "bot")
			writeRawDocument(t, store, content)

			if _, err := store.Create(context.Background(), "bot"); !errors.Is(err, ErrStoreCorrupt) {
				t.Fatalf("create: %v", err)
			}
			if _, err := store.List(); !errors.Is(err, ErrStoreCorrupt) {
				t.Fatalf("list: %v", err)
			}
			if _, err := store.Revoke(context.Background(), testID); !errors.Is(err, ErrStoreCorrupt) {
				t.Fatalf("revoke: %v", err)
			}
			if got := readFile(t, store.documentPath()); got != content {
				t.Fatalf("the corrupt document must stay untouched: %q", got)
			}
			if err := ErrStoreCorrupt.Error(); err != "keys.json is unreadable; restore from backup or recreate keys" {
				t.Fatalf("documented message changed: %s", err)
			}
		})
	}
}

// writeRawDocument goes through the private creation path, so that permission
// checks accept the file on every platform and only its content is under test.
func writeRawDocument(t *testing.T, store *Store, content string) {
	t.Helper()
	temporary := store.documentPath() + ".test"
	file, err := createPrivateFile(temporary)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := replaceFile(temporary, store.documentPath()); err != nil {
		t.Fatal(err)
	}
}

func TestGenerateCollisionRetries(t *testing.T) {
	store := newTestStore(t)
	first := mustCreate(t, store, "bot")
	store.entropy = rand.Reader

	repeated := make([]byte, 0, keyEntropy+idEntropy)
	raw := bytes.Repeat([]byte{0x42}, keyEntropy)
	repeated = append(repeated, raw...)
	repeated = append(repeated, bytes.Repeat([]byte{0x43}, idEntropy)...)

	// The first attempt uses a key and ID already present, the next one is fresh.
	doc, err := store.read()
	if err != nil {
		t.Fatal(err)
	}
	collidingKey, err := generateKey(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	doc.keys[0].sha256 = digestHex(collidingKey.Digest())

	store.entropy = io.MultiReader(bytes.NewReader(repeated), rand.Reader)
	key, r, err := store.newRecord(doc, "bot")
	if err != nil {
		t.Fatal(err)
	}
	if key.Reveal() == collidingKey.Reveal() || digestHex(key.Digest()) == doc.keys[0].sha256 {
		t.Fatal("a colliding digest must be regenerated")
	}
	if r.id == first.Metadata.ID {
		t.Fatal("ids must differ")
	}

	endless := bytes.Repeat(repeated, maxCollisionRetries+1)
	store.entropy = bytes.NewReader(endless)
	if _, _, err := store.newRecord(doc, "bot"); !errors.Is(err, ErrEntropy) {
		t.Fatalf("a source that keeps colliding must fail, got %v", err)
	}
}

func TestStoreRefusesWhenRecordLimitReached(t *testing.T) {
	store := newTestStore(t)
	doc := &document{revision: 1}
	created := store.clock.Now()
	revoked := created.Add(time.Second)
	for i := range MaxRecords {
		doc.keys = append(doc.keys, record{id: numberedID(i), app: "bot", sha256: numberedDigest(i), createdAt: created, revokedAt: revoked})
	}
	if err := ensurePrivateDir(store.dir); err != nil {
		t.Fatal(err)
	}
	data, err := doc.encode()
	if err != nil {
		t.Fatal(err)
	}
	writeRawDocument(t, store, string(data))
	if _, err := store.Create(context.Background(), "bot"); !errors.Is(err, ErrStoreFull) {
		t.Fatalf("err = %v", err)
	}
}

func TestListOrderAndMetadata(t *testing.T) {
	store := newTestStore(t)
	zeta := mustCreate(t, store, "zeta")
	clock := store.clock.(*fakeClock)
	clock.advance(time.Minute)
	alphaOld := mustCreate(t, store, "alpha")
	clock.advance(time.Minute)
	alphaNew := mustCreate(t, store, "alpha")

	entries, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	got := []string{entries[0].ID, entries[1].ID, entries[2].ID}
	want := []string{alphaOld.Metadata.ID, alphaNew.Metadata.ID, zeta.Metadata.ID}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order %v, want %v", got, want)
		}
	}
	if !entries[0].LastUsedAt.IsZero() || !entries[0].Active() {
		t.Fatalf("metadata %+v", entries[0])
	}
}

func TestRevocationNotDatedBeforeCreation(t *testing.T) {
	store := newTestStore(t)
	created := mustCreate(t, store, "bot")
	store.clock.(*fakeClock).advance(-time.Hour)
	if _, err := store.Revoke(context.Background(), created.Metadata.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.List(); err != nil {
		t.Fatalf("a clock moved back must not corrupt the document: %v", err)
	}
}
