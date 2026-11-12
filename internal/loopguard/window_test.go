package loopguard

import (
	"fmt"
	"testing"
	"time"
)

func TestSlidingBoundary(t *testing.T) {
	e := newEnv(t, Options{})
	u := user("agent", 1)
	for range 10 {
		e.allow(u, "same", defaultPolicy())
	}
	// An instant exactly one window old has already expired.
	e.clock.Advance(30 * time.Second)
	e.allow(u, "same", defaultPolicy())

	r := user("agent", 2)
	for i := range 60 {
		e.allow(r, fmt.Sprint(i), defaultPolicy())
	}
	e.clock.Advance(RateWindow - time.Nanosecond)
	e.cut(r, "late", defaultPolicy())
	r = user("agent", 3)
	for i := range 60 {
		e.allow(r, fmt.Sprint(i), defaultPolicy())
	}
	e.clock.Advance(RateWindow)
	e.allow(r, "on-time", defaultPolicy())
}

func TestInterleavedSignatures(t *testing.T) {
	e := newEnv(t, Options{})
	u := user("agent", 1)
	for range 10 {
		e.allow(u, "a", defaultPolicy())
		e.allow(u, "b", defaultPolicy())
	}
	if d := e.cut(u, "a", defaultPolicy()); d.Episode.Reason != ReasonSimilar {
		t.Fatalf("interleaved repetition: %+v", d)
	}
}

func TestMissingSignatureOnlyRate(t *testing.T) {
	e := newEnv(t, Options{})
	u := user("agent", 1)
	for range 60 {
		e.allow(u, "", defaultPolicy())
	}
	if d := e.cut(u, "", defaultPolicy()); d.Episode.Reason != ReasonRate {
		t.Fatalf("no signature: %+v", d)
	}
}

func TestThresholdOne(t *testing.T) {
	e := newEnv(t, Options{})
	p := defaultPolicy()
	p.SimilarRequests, p.RequestsPerMinute = 1, 1
	e.allow(user("a", 1), "x", p)
	if d := e.cut(user("a", 1), "x", p); d.Episode.Reason != ReasonSimilar || d.Episode.Count != 2 {
		t.Fatalf("similar=1: %+v", d)
	}
	e.allow(user("a", 2), "x", p)
	if d := e.cut(user("a", 2), "y", p); d.Episode.Reason != ReasonRate || d.Episode.Count != 2 {
		t.Fatalf("rpm=1: %+v", d)
	}
}

func TestSignatureSlotsZeroed(t *testing.T) {
	var q signatures
	for i := range 100 {
		q.push(time.Duration(i), sig(fmt.Sprint(i)))
	}
	q.purge(time.Duration(79))
	if q.len() != 20 || len(q.counts) != 20 {
		t.Fatalf("len=%d counts=%d", q.len(), len(q.counts))
	}
	for _, item := range q.items[q.len():cap(q.items)] {
		if item != (signed{}) {
			t.Fatal("expired digest kept in the backing array")
		}
	}
	q.purge(time.Duration(1000))
	if q.len() != 0 || q.counts != nil {
		t.Fatal("empty queue keeps state")
	}
}
