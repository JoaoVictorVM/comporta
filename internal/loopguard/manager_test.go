package loopguard

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSimilarCut11(t *testing.T) {
	e := newEnv(t, Options{})
	u := user("agent", 1)
	for range 10 {
		e.allow(u, "same", defaultPolicy())
		e.clock.Advance(time.Second)
	}
	d := e.cut(u, "same", defaultPolicy())
	if !d.First || d.Episode.Reason != ReasonSimilar || d.Episode.Count != 11 || d.Episode.Limit != 10 || d.Episode.Window != 30*time.Second || d.Episode.Cooldown != 120*time.Second || d.Remaining != 120*time.Second {
		t.Fatalf("eleventh similar request: %+v", d)
	}
	if len(d.Episode.EventID) != 32 || d.Episode.RequestID == "" || !d.Episode.At.Equal(start.Add(10*time.Second)) {
		t.Fatalf("episode metadata: %+v", d.Episode)
	}
}

func TestRateCut61(t *testing.T) {
	e := newEnv(t, Options{})
	u := user("agent", 1)
	for i := range 60 {
		e.allow(u, fmt.Sprintf("distinct-%d", i), defaultPolicy())
		e.clock.Advance(500 * time.Millisecond)
	}
	d := e.cut(u, "distinct-new", defaultPolicy())
	if d.Episode.Reason != ReasonRate || d.Episode.Count != 61 || d.Episode.Window != RateWindow {
		t.Fatalf("61st request: %+v", d)
	}
}

func TestAnonymousIsolation(t *testing.T) {
	e := newEnv(t, Options{})
	identified := user("agent", 1)
	anonymous := Subject{App: "agent", Hash: hashOf(1), Anonymous: true}
	otherApp := user("other", 1)
	for range 10 {
		e.allow(identified, "same", defaultPolicy())
	}
	e.cut(identified, "same", defaultPolicy())
	for _, subject := range []Subject{anonymous, otherApp, user("agent", 2)} {
		e.allow(subject, "same", defaultPolicy())
	}
}

func TestAppKeyRotationSharesSubject(t *testing.T) {
	// The subject has no key field: requests made with two keys of one app land
	// in the same state, so rotating a key mid-loop does not reset the count.
	e := newEnv(t, Options{})
	u := user("agent", 1)
	for range 5 {
		e.allow(u, "same", defaultPolicy())
	}
	rotated := Subject{App: u.App, Hash: u.Hash}
	for range 5 {
		e.allow(rotated, "same", defaultPolicy())
	}
	e.cut(rotated, "same", defaultPolicy())
}

func TestCooldownFixedAndRetriesDoNotExtend(t *testing.T) {
	e := newEnv(t, Options{})
	u := user("agent", 1)
	for range 10 {
		e.allow(u, "same", defaultPolicy())
	}
	first := e.cut(u, "same", defaultPolicy())
	e.clock.Advance(119 * time.Second)
	retry := e.cut(u, "other", defaultPolicy())
	if retry.First || retry.Episode != first.Episode || retry.Remaining != time.Second {
		t.Fatalf("retry during cooldown: %+v", retry)
	}
	e.clock.Advance(time.Second)
	e.allow(u, "same", defaultPolicy())
	for range 9 {
		e.allow(u, "same", defaultPolicy())
	}
	if d := e.cut(u, "same", defaultPolicy()); !d.First || d.Episode.EventID == first.Episode.EventID {
		t.Fatalf("a new episode needs a new event: %+v", d)
	}
}

func TestPrimaryReasonStable(t *testing.T) {
	p := defaultPolicy()
	p.RequestsPerMinute = 10
	e := newEnv(t, Options{})
	u := user("agent", 1)
	for range 10 {
		e.allow(u, "same", p)
	}
	if d := e.cut(u, "same", p); d.Episode.Reason != ReasonSimilar || d.Episode.Count != 11 {
		t.Fatalf("both rules fire: %+v", d)
	}
}

func TestConcurrentSameSignature(t *testing.T) {
	for round := range 50 {
		e := newEnv(t, Options{})
		u := user("agent", round)
		var allowed, firsts atomic.Int64
		events := sync.Map{}
		var wg sync.WaitGroup
		for range 40 {
			wg.Go(func() {
				d, err := e.manager.Check(e.request(u, "same", defaultPolicy()))
				if err != nil {
					t.Error(err)
					return
				}
				if d.Allowed {
					allowed.Add(1)
					return
				}
				events.Store(d.Episode.EventID, true)
				if d.First {
					firsts.Add(1)
				}
			})
		}
		wg.Wait()
		count := 0
		events.Range(func(any, any) bool { count++; return true })
		if allowed.Load() != 10 || firsts.Load() != 1 || count != 1 {
			t.Fatalf("round %d: allowed=%d firsts=%d events=%d", round, allowed.Load(), firsts.Load(), count)
		}
	}
}

func TestConcurrentMixedSignatures(t *testing.T) {
	e := newEnv(t, Options{})
	u := user("agent", 1)
	var allowed atomic.Int64
	var wg sync.WaitGroup
	for i := range 200 {
		wg.Go(func() {
			d, err := e.manager.Check(e.request(u, fmt.Sprintf("s-%d", i%40), defaultPolicy()))
			if err == nil && d.Allowed {
				allowed.Add(1)
			}
		})
	}
	wg.Wait()
	if allowed.Load() > 60 {
		t.Fatalf("more than 60 passed: %d", allowed.Load())
	}
}

func TestDisabledNeverCuts(t *testing.T) {
	e := newEnv(t, Options{})
	p := defaultPolicy()
	p.Enabled = false
	u := user("agent", 1)
	for range 200 {
		e.allow(u, "same", p)
	}
	if e.manager.Subjects() != 0 || e.manager.Occurrences() != 0 {
		t.Fatal("a disabled app must not keep state")
	}
}

func TestInvalidRequestRejected(t *testing.T) {
	e := newEnv(t, Options{})
	if _, err := e.manager.Check(Request{Policy: defaultPolicy()}); err != ErrInvalid {
		t.Fatalf("missing subject: %v", err)
	}
	p := defaultPolicy()
	p.Cooldown = 0
	if _, err := e.manager.Check(e.request(user("a", 1), "x", p)); err != ErrInvalid {
		t.Fatalf("invalid policy: %v", err)
	}
}

func TestDecisionAndSubjectRedacted(t *testing.T) {
	u := user("agent", 7)
	for _, text := range []string{fmt.Sprintf("%v", u), fmt.Sprintf("%+v", u), fmt.Sprint(Request{Subject: u})} {
		if contains(text, u.Hash) {
			t.Fatalf("hash leaked: %s", text)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
