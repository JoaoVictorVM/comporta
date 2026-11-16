package limits

import (
	"errors"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

func onlyOriginal(r Request) []Candidate {
	return []Candidate{{ID: CandidateOriginal, Model: "model-a", EstimatedTokens: r.EstimatedTokens, ReservedCost: r.ReservedCost}}
}

func (e *env) open(r Request) (*Session, *Handle) {
	e.t.Helper()
	op, h, d, _, err := e.manager.ReserveSession(e.policy, r, onlyOriginal(r), func(res storage.Reservation, _ Selection) (int64, error) {
		return e.store.commit(res)
	})
	if err != nil || !d.Allowed || h == nil || op == nil {
		e.t.Fatalf("session refused: %+v %v", d.Primary, err)
	}
	return op, h
}

func (e *env) continueAt(op *Session, r Request, index int, tokens int64, cost config.Microusd) (*Handle, Decision, error) {
	r.AttemptIndex, r.EstimatedTokens, r.ReservedCost = index, tokens, cost
	h, d, _, err := e.manager.ReserveContinuation(op, r, onlyOriginal(r), func(res storage.Reservation, _ Selection) (int64, error) {
		return e.store.commit(res)
	})
	return h, d, err
}

// failSent settles a sent attempt whose usage is unknown, at its reservation.
func (e *env) failSent(h *Handle) {
	e.t.Helper()
	if err := e.manager.Settle(h, Usage{Cost: h.request.ReservedCost}, nil); err != nil {
		e.t.Fatal(err)
	}
}

func TestFallbackCountsRPMOnce(t *testing.T) {
	e := newEnv(t, Policy{AppSet: Set{Limits: config.Limits{RequestsPerMinute: 1}}})
	u := user("bot", 1)
	r := e.request(u, 10, 1)
	op, h := e.open(r)
	for index := 2; index <= storage.MaxAttempts; index++ {
		e.failSent(h)
		var d Decision
		var err error
		h, d, err = e.continueAt(op, r, index, 10, 1)
		if err != nil || !d.Allowed {
			t.Fatalf("attempt %d refused by the request rate: %+v %v", index, d.Primary, err)
		}
	}
	if used := e.view(nil, "apps.bot.limits.requests_per_minute").Used; used != 1 {
		t.Fatalf("requests counted %d", used)
	}
	e.failSent(h)
	e.manager.CloseSession(op)
	// A new logical request still meets the limit.
	e.mustReject(e.request(u, 10, 1))
	if _, _, err := e.continueAt(op, r, storage.MaxAttempts+1, 10, 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("attempt past the maximum: %v", err)
	}
}

func TestFallbackHoldsConcurrency(t *testing.T) {
	e := newEnv(t, Policy{AppSet: Set{Limits: config.Limits{MaxConcurrent: 1}}})
	u := user("bot", 1)
	r := e.request(u, 10, 1)
	op, h := e.open(r)
	e.failSent(h)
	// Between attempts the slot stays taken: nobody else gets in.
	e.mustReject(e.request(user("bot", 2), 10, 1))
	h, d, err := e.continueAt(op, r, 2, 10, 1)
	if err != nil || !d.Allowed {
		t.Fatalf("continuation counted as a new concurrent request: %+v %v", d.Primary, err)
	}
	if e.manager.concurrent[u.appSubject()] != 1 {
		t.Fatalf("slots %d", e.manager.concurrent[u.appSubject()])
	}
	e.failSent(h)
	e.manager.CloseSession(op)
	e.manager.CloseSession(op)
	if e.manager.concurrent[u.appSubject()] != 0 {
		t.Fatalf("slot not freed exactly once: %d", e.manager.concurrent[u.appSubject()])
	}
	e.mustReserve(e.request(user("bot", 2), 10, 1))
}

func TestFallbackTokensUseAttemptAdmissionTime(t *testing.T) {
	e := newEnv(t, Policy{AppSet: Set{Limits: config.Limits{TokensPerMinute: 100}}})
	u := user("bot", 1)
	r := e.request(u, 60, 1)
	op, h := e.open(r)
	e.failSent(h)
	// Each attempt reserves its own tokens: a second one of 60 does not fit next
	// to the 60 the first attempt consumed.
	if _, d, err := e.continueAt(op, r, 2, 60, 1); err != nil || d.Allowed || d.Primary.Dimension != TokensPerMinute {
		t.Fatalf("second attempt ignored the first one's tokens: %+v %v", d, err)
	}
	// Once the first attempt leaves the window, the next attempt fits, and its
	// tokens count from its own admission even though the request is older.
	e.clock.Advance(61 * time.Second)
	h, d, err := e.continueAt(op, r, 2, 60, 1)
	if err != nil || !d.Allowed {
		t.Fatalf("attempt refused after the window moved: %+v %v", d.Primary, err)
	}
	if v := e.view(nil, "apps.bot.limits.tokens_per_minute"); v.Reserved != 60 {
		t.Fatalf("tokens of the attempt %+v", v)
	}
	e.settle(h, 1, 50)
	e.clock.Advance(30 * time.Second)
	if v := e.view(nil, "apps.bot.limits.tokens_per_minute"); v.Used != 50 {
		t.Fatalf("settled tokens left the window early: %+v", v)
	}
	e.manager.CloseSession(op)
	// After a restart the attempt's tokens still count, and the old request
	// never comes back as a request per minute.
	e.restart()
	if v := e.view(nil, "apps.bot.limits.tokens_per_minute"); v.Used != 50 {
		t.Fatalf("tokens after restart: %+v", v)
	}
}

func TestFallbackContinuationRequiresLiveHandle(t *testing.T) {
	e := newEnv(t, Policy{})
	u := user("bot", 1)
	r := e.request(u, 10, 1)
	op, h := e.open(r)
	// The previous attempt is still reserved: no overlap.
	if _, _, err := e.continueAt(op, r, 2, 10, 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("overlapping attempts: %v", err)
	}
	e.failSent(h)
	for name, change := range map[string]func(*Request){
		"another request":  func(x *Request) { x.RequestID = e.request(u, 1, 1).RequestID },
		"another subject":  func(x *Request) { x.Subject = user("bot", 2) },
		"skipped index":    func(x *Request) { x.AttemptIndex = 3 },
		"repeated index":   func(x *Request) { x.AttemptIndex = 1 },
		"count tokens now": func(x *Request) { x.Operation = OperationCountTokens },
	} {
		asked := r
		asked.AttemptIndex = 2
		change(&asked)
		if _, _, _, err := e.manager.ReserveContinuation(op, asked, onlyOriginal(asked), func(res storage.Reservation, _ Selection) (int64, error) {
			return e.store.commit(res)
		}); !errors.Is(err, ErrConflict) {
			t.Errorf("%s: %v", name, err)
		}
	}
	e.manager.CloseSession(op)
	if _, _, err := e.continueAt(op, r, 2, 10, 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("continuation of a closed session: %v", err)
	}
	if _, _, err := e.continueAt(nil, r, 2, 10, 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("continuation without a session: %v", err)
	}
	// A failed settlement keeps the previous attempt reserved: nothing else is sent.
	r = e.request(u, 10, 1)
	op, h = e.open(r)
	e.store.set(func(s *memStore) { s.failSettle = true })
	if err := e.manager.Settle(h, Usage{Cost: 1}, nil); err == nil {
		t.Fatal("settlement failure not reported")
	}
	if _, _, err := e.continueAt(op, r, 2, 10, 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("continuation after a failed settlement: %v", err)
	}
	e.manager.CloseSession(op)
}

func TestFallbackPinnedGeneration(t *testing.T) {
	e := newEnv(t, Policy{AppSet: Set{Budget: config.Budget{USDPerDay: 10}}})
	u := user("bot", 1)
	r := e.request(u, 10, 4)
	r.Generation = 1
	op, h := e.open(r)
	e.failSent(h)
	// A reload reaches the coordinator in the middle of the chain.
	newer := e.request(u, 1, 1)
	newer.Generation = 2
	e.settle(e.mustReserve(newer), 1, 1)
	// The continuation keeps the policy and generation it was admitted under...
	h2, d, err := e.continueAt(op, r, 2, 10, 4)
	if err != nil || !d.Allowed || h2.request.Generation != 1 {
		t.Fatalf("continuation refused after reload: %+v %v", d, err)
	}
	// ...while a new admission on the old generation is still stale, and the
	// budget still counts every attempt: 4 + 1 + 4 + 4 > 10.
	stale := e.request(u, 1, 1)
	stale.Generation = 1
	if _, _, err := e.reserve(stale); !errors.Is(err, ErrStale) {
		t.Fatalf("old generation admitted: %v", err)
	}
	e.failSent(h2)
	if _, d, err := e.continueAt(op, r, 3, 10, 4); err != nil || d.Allowed || d.Primary.Kind != KindBudget {
		t.Fatalf("budget ignored by continuation: %+v %v", d, err)
	}
	e.manager.CloseSession(op)
}
