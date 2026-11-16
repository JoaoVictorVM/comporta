package responsecache

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestReloadDiscardsOldGeneration(t *testing.T) {
	c, _ := newTestCache(MaxCapacity)
	k := key("bot", "a")
	store(t, c, k, "{}", time.Minute)
	c.Sync(Settings{Generation: 1, MaxBytes: MaxCapacity, Apps: []string{"bot"}})
	if l, ok := c.Lookup(k, 1); !ok {
		t.Fatal("the same generation keeps entries")
	} else {
		l.Release()
	}
	c.Sync(Settings{Generation: 2, MaxBytes: MaxCapacity, Apps: []string{"bot"}})
	if _, ok := c.Lookup(k, 1); ok {
		t.Fatal("an old generation never hits")
	}
	if s := c.Stats(); s.Entries != 0 || s.UsedBytes != 0 {
		t.Fatalf("stats = %+v", s)
	}
	c.Sync(Settings{Generation: 1, MaxBytes: 1})
	if s := c.Stats(); s.MaxBytes != MaxCapacity {
		t.Fatal("an older generation never moves the cache back")
	}
}

func TestLookupObservesNewerGeneration(t *testing.T) {
	c, _ := newTestCache(MaxCapacity)
	k := key("bot", "a")
	store(t, c, k, "{}", time.Minute)
	if _, ok := c.Lookup(key("bot", "b"), 2); ok {
		t.Fatal("miss expected")
	}
	if _, ok := c.Lookup(k, 1); ok {
		t.Fatal("a request on a newer generation invalidates older entries")
	}
}

func TestStalePublishRejected(t *testing.T) {
	c, _ := newTestCache(MaxCapacity)
	k := key("bot", "a")
	b := c.Begin(k, 1, KindJSON, "application/json")
	b.Append([]byte("{}"))
	c.Sync(Settings{Generation: 2, MaxBytes: MaxCapacity, Apps: []string{"bot"}})
	if b.Publish(time.Minute) {
		t.Fatal("a capture of an old generation cannot publish")
	}
	if c.Begin(k, 1, KindJSON, "application/json") != nil {
		t.Fatal("a capture cannot start on an old generation")
	}
	purged := c.Begin(key("bot", "b"), 2, KindJSON, "application/json")
	purged.Append([]byte("{}"))
	c.PurgeApps([]string{"bot"})
	if purged.Publish(time.Minute) {
		t.Fatal("a capture that started before a purge cannot publish")
	}
	other := c.Begin(key("other", "b"), 2, KindJSON, "application/json")
	other.Append([]byte("{}"))
	c.PurgeApps([]string{"bot"})
	if !other.Publish(time.Minute) {
		t.Fatal("a purge only affects its apps")
	}
	if s := c.Stats(); s.Builders != 0 || s.Entries != 1 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestShrinkDefersPinnedBytes(t *testing.T) {
	c, _ := newTestCache(MaxCapacity)
	a, b := key("bot", "a"), key("bot", "b")
	body := strings.Repeat("x", 1000)
	store(t, c, a, body, time.Minute)
	store(t, c, b, body, time.Minute)
	l, _ := c.Lookup(a, 1)
	c.Sync(Settings{Generation: 1, MaxBytes: 100, Apps: []string{"bot"}})
	s := c.Stats()
	if s.Entries != 1 || s.UsedBytes <= 100 {
		t.Fatalf("the unpinned entry goes, the pinned one keeps its bytes: %+v", s)
	}
	if c.Begin(key("bot", "c"), 1, KindJSON, "application/json") != nil {
		t.Fatal("no capture fits while leases exceed the budget")
	}
	l.Release()
	c.Sync(Settings{Generation: 1, MaxBytes: 100, Apps: []string{"bot"}})
	if s := c.Stats(); s.UsedBytes != 0 || s.Entries != 0 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestCloseReleasesAllReferences(t *testing.T) {
	c, _ := newTestCache(MaxCapacity)
	k := key("bot", "a")
	store(t, c, k, "{}", time.Minute)
	l, _ := c.Lookup(k, 1)
	b := c.Begin(key("bot", "b"), 1, KindJSON, "application/json")
	b.Append([]byte("{}"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error)
	go func() { done <- c.Run(ctx, func() Settings { return Settings{Generation: 1, MaxBytes: MaxCapacity} }) }()
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, ok := c.Lookup(k, 1); ok {
		t.Fatal("a closed cache never hits")
	}
	if b.Publish(time.Minute) {
		t.Fatal("a closed cache accepts nothing")
	}
	l.Release()
	if s := c.Stats(); s.UsedBytes != 0 || s.Entries != 0 || s.Leases != 0 || s.Builders != 0 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestSweepRemovesExpired(t *testing.T) {
	c, clk := newTestCache(MaxCapacity)
	store(t, c, key("bot", "a"), "{}", time.Second)
	store(t, c, key("bot", "b"), "{}", time.Hour)
	clk.t = clk.t.Add(2 * time.Second)
	c.Sweep()
	if s := c.Stats(); s.Entries != 1 {
		t.Fatalf("stats = %+v", s)
	}
}
