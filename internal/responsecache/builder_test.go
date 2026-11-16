package responsecache

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCaptureBudgetBeforeAllocation(t *testing.T) {
	meta := entryOverhead + int64(len("bot")+len("text/event-stream"))
	c, _ := newTestCache(meta + 2*(eventOverhead+10))
	b := c.Begin(key("bot", "a"), 1, KindSSE, "text/event-stream")
	if b == nil {
		t.Fatal("capture must start")
	}
	if !b.Append(make([]byte, 10)) || !b.Append(make([]byte, 10)) {
		t.Fatal("events within the budget must be kept")
	}
	if b.Append(make([]byte, 1)) {
		t.Fatal("an event past the budget must abandon the capture")
	}
	if b.Active() || len(b.events) != 0 {
		t.Fatal("an abandoned capture keeps nothing")
	}
	if s := c.Stats(); s.UsedBytes != 0 || s.Builders != 0 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestOversizeCaptureAborts(t *testing.T) {
	c, _ := newTestCache(MaxCapacity)
	b := c.Begin(key("bot", "a"), 1, KindJSON, "application/json")
	if b.Append(make([]byte, MaxEntryBytes)) {
		t.Fatal("a body over the per-entry limit cannot be kept")
	}
	if b.Publish(time.Minute) {
		t.Fatal("an abandoned capture cannot be published")
	}
	if s := c.Stats(); s.UsedBytes != 0 || s.Entries != 0 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestPublishTransfersOwnership(t *testing.T) {
	c, _ := newTestCache(MaxCapacity)
	k := key("bot", "a")
	b := c.Begin(k, 1, KindSSE, "text/event-stream")
	b.Append([]byte("data: 1\n\n"))
	b.Append([]byte("data: [DONE]\n\n"))
	captured := &b.events[0][0]
	used := c.Stats().UsedBytes
	if !b.Publish(time.Minute) {
		t.Fatal("publish must succeed")
	}
	if c.Stats().UsedBytes != used {
		t.Fatal("publishing must not charge the bytes twice")
	}
	l, ok := c.Lookup(k, 1)
	if !ok {
		t.Fatal("published entry must hit")
	}
	defer l.Release()
	if &l.Events()[0][0] != captured || len(l.Events()) != 2 || l.Kind() != KindSSE || l.ContentType() != "text/event-stream" {
		t.Fatal("the entry must own the captured buffers without a copy")
	}
	if b.Publish(time.Minute) {
		t.Fatal("a builder publishes once")
	}
}

func TestAbortAndReleaseAreIdempotent(t *testing.T) {
	c, _ := newTestCache(MaxCapacity)
	b := c.Begin(key("bot", "a"), 1, KindJSON, "application/json")
	b.Append([]byte("{}"))
	b.Abort()
	b.Abort()
	var nilBuilder *Builder
	nilBuilder.Abort()
	nilBuilder.Append([]byte("x"))
	var nilLease *Lease
	nilLease.Release()
	if s := c.Stats(); s.UsedBytes != 0 || s.Builders != 0 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestSecondJSONBodyAborts(t *testing.T) {
	c, _ := newTestCache(MaxCapacity)
	b := c.Begin(key("bot", "a"), 1, KindJSON, "application/json")
	b.Append([]byte("{}"))
	if b.Append([]byte("{}")) || b.Active() {
		t.Fatal("a JSON capture holds one body")
	}
}

func TestConcurrentBuildersBoundMemory(t *testing.T) {
	meta := entryOverhead + int64(len("bot")+len("text/event-stream"))
	budget := 4 * (meta + 10*(eventOverhead+100))
	c, _ := newTestCache(budget)
	var wg sync.WaitGroup
	var mu sync.Mutex
	peak := int64(0)
	for i := range 32 {
		wg.Go(func() {
			b := c.Begin(key("bot", strings.Repeat("k", i+1)), 1, KindSSE, "text/event-stream")
			for range 10 {
				b.Append(make([]byte, 100))
				used := c.Stats().UsedBytes
				mu.Lock()
				peak = max(peak, used)
				mu.Unlock()
			}
			if i%2 == 0 {
				b.Publish(time.Minute)
			} else {
				b.Abort()
			}
		})
	}
	wg.Wait()
	if peak > budget {
		t.Fatalf("captures exceeded the budget: %d > %d", peak, budget)
	}
	if s := c.Stats(); s.UsedBytes > budget || s.Builders != 0 || s.UsedBytes < 0 {
		t.Fatalf("stats = %+v", s)
	}
}
