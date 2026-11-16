package responsecache

import (
	"strconv"
	"testing"
	"time"
)

func BenchmarkLookupHit(b *testing.B) {
	c := New(Options{})
	c.Sync(Settings{Generation: 1})
	k := key("bot", "a")
	builder := c.Begin(k, 1, KindJSON, "application/json")
	builder.Append(make([]byte, 2048))
	builder.Publish(time.Hour)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			l, _ := c.Lookup(k, 1)
			l.Release()
		}
	})
}

func BenchmarkCaptureStream(b *testing.B) {
	c := New(Options{})
	c.Sync(Settings{Generation: 1})
	event := make([]byte, 256)
	b.ResetTimer()
	for i := range b.N {
		builder := c.Begin(key("bot", strconv.Itoa(i%1000)), 1, KindSSE, "text/event-stream")
		for range 50 {
			builder.Append(event)
		}
		builder.Publish(time.Hour)
	}
	if s := c.Stats(); s.UsedBytes > s.MaxBytes {
		b.Fatalf("budget exceeded: %+v", s)
	}
}
