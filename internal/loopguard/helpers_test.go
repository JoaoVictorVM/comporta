package loopguard

import (
	"crypto/sha256"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

var start = time.Date(2026, 11, 11, 15, 0, 0, 0, time.UTC)

func defaultPolicy() Policy {
	return Policy{Generation: 1, Enabled: true, SimilarRequests: 10, SimilarWindow: 30 * time.Second, RequestsPerMinute: 60, Cooldown: 120 * time.Second}
}

func hashOf(n int) string { return fmt.Sprintf("%032x", n) }

func user(app string, n int) Subject { return Subject{App: app, Hash: hashOf(n)} }

func sig(text string) [32]byte { return sha256.Sum256([]byte(text)) }

type env struct {
	t       *testing.T
	clock   *FakeClock
	manager *Manager
	next    atomic.Int64
}

func newEnv(t *testing.T, options Options) *env {
	t.Helper()
	clock := NewFakeClock(start)
	options.Clock = clock
	return &env{t: t, clock: clock, manager: New(options)}
}

func (e *env) request(subject Subject, signature string, policy Policy) Request {
	r := Request{Subject: subject, RequestID: fmt.Sprintf("%032x", e.next.Add(1)), Policy: policy}
	if signature != "" {
		r.Signature, r.HasSignature = sig(signature), true
	}
	return r
}

func (e *env) check(subject Subject, signature string, policy Policy) Decision {
	e.t.Helper()
	d, err := e.manager.Check(e.request(subject, signature, policy))
	if err != nil {
		e.t.Fatalf("check: %v", err)
	}
	return d
}

func (e *env) allow(subject Subject, signature string, policy Policy) {
	e.t.Helper()
	if d := e.check(subject, signature, policy); !d.Allowed {
		e.t.Fatalf("expected allowed, got %v", d)
	}
}

func (e *env) cut(subject Subject, signature string, policy Policy) Decision {
	e.t.Helper()
	d := e.check(subject, signature, policy)
	if d.Allowed {
		e.t.Fatal("expected a cut")
	}
	return d
}
