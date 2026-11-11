package limits

import (
	"context"
	"errors"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

func forgetPolicy() Policy {
	return Policy{
		AppSet:  Set{Budget: config.Budget{USDPerDay: 1_000_000}, Limits: config.Limits{RequestsPerMinute: 100}},
		UserSet: Set{Budget: config.Budget{USDPerDay: 300_000}, Limits: config.Limits{RequestsPerMinute: 2}},
	}
}

func ref(s Subject) []storage.SubjectRef {
	return []storage.SubjectRef{{App: s.App, Hash: s.Hash}}
}

func TestForgetKeepsAppSpendAndRate(t *testing.T) {
	e := newEnv(t, forgetPolicy())
	gone, kept := user("bot", 1), user("bot", 2)
	e.settle(e.mustReserve(e.request(gone, 10, 200_000)), 200_000, 10)
	e.settle(e.mustReserve(e.request(kept, 10, 100_000)), 100_000, 10)
	appBefore := e.view(nil, "apps.bot.budget.usd_per_day")
	rateBefore := e.view(nil, "apps.bot.limits.requests_per_minute")
	persisted := false
	cleared, err := e.manager.ForgetSubjects(ref(gone), func() error { persisted = true; return nil })
	if err != nil || !persisted || cleared != 1 {
		t.Fatalf("forget: %d %v %v", cleared, persisted, err)
	}
	if after := e.view(nil, "apps.bot.budget.usd_per_day"); after.Used != appBefore.Used || after.Used != 300_000 {
		t.Fatalf("the app's spending changed: %+v -> %+v", appBefore, after)
	}
	if after := e.view(nil, "apps.bot.limits.requests_per_minute"); after.Used != rateBefore.Used {
		t.Fatalf("the app's rate changed: %+v -> %+v", rateBefore, after)
	}
	if v := e.view(&kept, "apps.bot.per_user.budget.usd_per_day"); v.Used != 100_000 {
		t.Fatalf("another user lost state: %+v", v)
	}
}

func TestForgetRemovesUserProjection(t *testing.T) {
	e := newEnv(t, forgetPolicy())
	gone := user("bot", 1)
	e.settle(e.mustReserve(e.request(gone, 10, 300_000)), 300_000, 10)
	e.mustReject(e.request(gone, 10, 1))
	if _, err := e.manager.ForgetSubjects(ref(gone), func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if v := e.view(&gone, "apps.bot.per_user.budget.usd_per_day"); v.Used != 0 || v.Reserved != 0 {
		t.Fatalf("the user's budget survived: %+v", v)
	}
	if v := e.view(&gone, "apps.bot.per_user.limits.requests_per_minute"); v.Used != 0 {
		t.Fatalf("the user's rate window survived: %+v", v)
	}
	e.mustReserve(e.request(gone, 10, 1))
}

func TestForgetRefusesUnreconciledReservation(t *testing.T) {
	e := newEnv(t, forgetPolicy())
	gone := user("bot", 1)
	held := e.mustReserve(e.request(gone, 10, 100_000))
	called := false
	if _, err := e.manager.ForgetSubjects(ref(gone), func() error { called = true; return nil }); !errors.Is(err, storage.ErrBusy) || called {
		t.Fatalf("a held reservation: %v %v", err, called)
	}
	e.store.set(func(s *memStore) { s.failSettle = true })
	if err := e.manager.Settle(held, Usage{Cost: 100_000, Complete: true}, nil); err == nil {
		t.Fatal("the injected settlement failure did not happen")
	}
	if _, err := e.manager.ForgetSubjects(ref(gone), func() error { called = true; return nil }); !errors.Is(err, storage.ErrBusy) || called {
		t.Fatalf("a settlement waiting for its retry: %v %v", err, called)
	}
}

func TestForgetPersistFailureKeepsMemory(t *testing.T) {
	e := newEnv(t, forgetPolicy())
	gone := user("bot", 1)
	e.settle(e.mustReserve(e.request(gone, 10, 200_000)), 200_000, 10)
	if _, err := e.manager.ForgetSubjects(ref(gone), func() error { return errInjected }); !errors.Is(err, errInjected) {
		t.Fatal(err)
	}
	if v := e.view(&gone, "apps.bot.per_user.budget.usd_per_day"); v.Used != 200_000 {
		t.Fatalf("a rolled back deletion changed memory: %+v", v)
	}
}

func TestCheckpointCannotRestoreForgottenUser(t *testing.T) {
	e := newEnv(t, forgetPolicy())
	gone := user("bot", 1)
	e.settle(e.mustReserve(e.request(gone, 10, 200_000)), 200_000, 10)
	if err := e.manager.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The store side of the deletion, as the SQLite transaction does it.
	persist := func() error {
		e.store.set(func(s *memStore) {
			for key := range s.buckets {
				if key.scope == ScopeUser && key.subject == gone.Hash {
					delete(s.buckets, key)
				}
			}
			for i := range s.ledger {
				kept := s.ledger[i].Deltas[:0]
				for _, d := range s.ledger[i].Deltas {
					if d.Subject != gone.Hash {
						kept = append(kept, d)
					}
				}
				s.ledger[i].Deltas = kept
			}
		})
		return nil
	}
	if _, err := e.manager.ForgetSubjects(ref(gone), persist); err != nil {
		t.Fatal(err)
	}
	if err := e.manager.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.store.mu.Lock()
	for key := range e.store.buckets {
		if key.subject == gone.Hash {
			e.store.mu.Unlock()
			t.Fatal("a later checkpoint wrote the forgotten user back")
		}
	}
	e.store.mu.Unlock()
	e.restart()
	if v := e.view(&gone, "apps.bot.per_user.budget.usd_per_day"); v.Used != 0 {
		t.Fatalf("recovery restored the user: %+v", v)
	}
	if v := e.view(nil, "apps.bot.budget.usd_per_day"); v.Used != 200_000 {
		t.Fatalf("recovery lost the app's spending: %+v", v)
	}
}
