package responsecache

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newTestCache(max int64) (*Cache, *clock) {
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	c := New(Options{MaxBytes: max, Now: clk.now})
	c.Sync(Settings{Generation: 1, MaxBytes: max, Apps: []string{"bot", "other"}})
	return c, clk
}

func key(app, body string) Key {
	var digest [32]byte
	copy(digest[:], body)
	return NewKey(KeyParts{Generation: 1, App: app, Namespace: "user", Format: "openai", Endpoint: "/v1/chat/completions", Provider: "p", Model: "m", BodyDigest: digest})
}

func store(t *testing.T, c *Cache, k Key, body string, ttl time.Duration) bool {
	t.Helper()
	b := c.Begin(k, 1, KindJSON, "application/json")
	if b == nil {
		return false
	}
	if !b.Append([]byte(body)) {
		return false
	}
	return b.Publish(ttl)
}

func TestLRUEviction(t *testing.T) {
	size := entryOverhead + int64(len("bot")+len("application/json")) + 100
	c, _ := newTestCache(size * 2)
	a, b, d := key("bot", "a"), key("bot", "b"), key("bot", "d")
	body := strings.Repeat("x", 100)
	if !store(t, c, a, body, time.Minute) || !store(t, c, b, body, time.Minute) {
		t.Fatal("first two entries must fit")
	}
	lease, ok := c.Lookup(a, 1)
	if !ok {
		t.Fatal("a must hit")
	}
	lease.Release()
	if !store(t, c, d, body, time.Minute) {
		t.Fatal("third entry must evict the least recently used")
	}
	if _, ok := c.Lookup(b, 1); ok {
		t.Fatal("b was the least recently used and must be gone")
	}
	for _, k := range []Key{a, d} {
		l, ok := c.Lookup(k, 1)
		if !ok {
			t.Fatal("recent entries must stay")
		}
		l.Release()
	}
	if s := c.Stats(); s.UsedBytes != size*2 || s.Entries != 2 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestTTLBoundary(t *testing.T) {
	c, clk := newTestCache(MaxCapacity)
	k := key("bot", "a")
	store(t, c, k, "{}", 10*time.Second)
	clk.t = clk.t.Add(10*time.Second - time.Nanosecond)
	l, ok := c.Lookup(k, 1)
	if !ok {
		t.Fatal("entry must be valid just before expiry")
	}
	l.Release()
	clk.t = clk.t.Add(time.Nanosecond)
	if _, ok := c.Lookup(k, 1); ok {
		t.Fatal("entry must be a miss at expires_at")
	}
	if s := c.Stats(); s.Entries != 0 || s.UsedBytes != 0 {
		t.Fatalf("expired entry must leave the index: %+v", s)
	}
}

func TestHitDoesNotExtendTTL(t *testing.T) {
	c, clk := newTestCache(MaxCapacity)
	k := key("bot", "a")
	store(t, c, k, "{}", 10*time.Second)
	for range 3 {
		clk.t = clk.t.Add(3 * time.Second)
		l, ok := c.Lookup(k, 1)
		if !ok {
			t.Fatal("must hit before expiry")
		}
		l.Release()
	}
	clk.t = clk.t.Add(time.Second)
	if _, ok := c.Lookup(k, 1); ok {
		t.Fatal("hits must not renew the expiry")
	}
}

func TestLeaseSurvivesEviction(t *testing.T) {
	c, _ := newTestCache(MaxCapacity)
	k := key("bot", "a")
	store(t, c, k, `{"id":"1"}`, time.Minute)
	l, ok := c.Lookup(k, 1)
	if !ok {
		t.Fatal("must hit")
	}
	c.PurgeApps([]string{"bot"})
	if !bytes.Equal(l.Body(), []byte(`{"id":"1"}`)) {
		t.Fatal("a lease keeps its bytes after the entry left the index")
	}
	if s := c.Stats(); s.Entries != 0 || s.UsedBytes == 0 || s.Leases != 1 {
		t.Fatalf("pinned bytes must stay charged: %+v", s)
	}
	l.Release()
	l.Release()
	if s := c.Stats(); s.UsedBytes != 0 || s.Leases != 0 {
		t.Fatalf("last release must return the bytes once: %+v", s)
	}
}

func TestGlobalBytesIncludePinnedEntries(t *testing.T) {
	size := entryOverhead + int64(len("bot")+len("application/json")) + 100
	c, _ := newTestCache(size)
	a := key("bot", "a")
	body := strings.Repeat("x", 100)
	store(t, c, a, body, time.Minute)
	l, _ := c.Lookup(a, 1)
	defer l.Release()
	if store(t, c, key("bot", "b"), body, time.Minute) {
		t.Fatal("a pinned entry cannot be evicted to make room")
	}
	if s := c.Stats(); s.UsedBytes != size || s.Builders != 0 {
		t.Fatalf("abandoned capture must return its bytes: %+v", s)
	}
}

func TestTinyCapacityBypasses(t *testing.T) {
	c, _ := newTestCache(10)
	if b := c.Begin(key("bot", "a"), 1, KindJSON, "application/json"); b != nil {
		t.Fatal("a budget smaller than the metadata keeps nothing")
	}
	if s := c.Stats(); s.UsedBytes != 0 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestKeyNeverShowsDigest(t *testing.T) {
	k := key("bot", "secret-body")
	for _, text := range []string{fmt.Sprint(k), fmt.Sprintf("%#v", k), fmt.Sprintf("%+v", k), fmt.Sprintf("%x", k)} {
		if strings.Contains(text, "secret") || strings.Contains(text, fmt.Sprintf("%x", k.digest[:4])) {
			t.Fatalf("key leaked its digest: %s", text)
		}
	}
}

func TestKeyComponentsAreUnambiguous(t *testing.T) {
	a := NewKey(KeyParts{App: "bot", Namespace: "ab", Format: "c"})
	b := NewKey(KeyParts{App: "bot", Namespace: "a", Format: "bc"})
	if a == b {
		t.Fatal("moving bytes between components must change the key")
	}
	if NewKey(KeyParts{App: "bot", Stream: true}) == NewKey(KeyParts{App: "bot"}) {
		t.Fatal("stream must partition keys")
	}
}
