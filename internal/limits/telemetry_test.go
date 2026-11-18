package limits

import (
	"strings"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

func entry(t *testing.T, a Aggregate, app string, scope AggregateScope, d Dimension, kind Kind) AggregateEntry {
	t.Helper()
	for _, view := range a.Apps {
		if view.App != app {
			continue
		}
		for _, e := range view.Entries {
			if e.Scope == scope && e.Dimension == d && e.Kind == kind {
				return e
			}
		}
	}
	t.Fatalf("no %s %s %s entry for %s in %+v", scope, d, kind, app, a)
	return AggregateEntry{}
}

func hasEntry(a Aggregate, scope AggregateScope, d Dimension) bool {
	for _, view := range a.Apps {
		for _, e := range view.Entries {
			if e.Scope == scope && e.Dimension == d {
				return true
			}
		}
	}
	return false
}

func TestUserBudgetMaxWithoutIdentity(t *testing.T) {
	e := newEnv(t, Policy{
		AppSet:  Set{Budget: config.Budget{USDPerDay: 1_000_000}},
		UserSet: Set{Budget: config.Budget{USDPerDay: 100_000}},
	})
	first, second := user("bot", 1), user("bot", 2)
	e.settle(e.mustReserve(e.request(first, 10, 20_000)), 20_000, 10)
	e.settle(e.mustReserve(e.request(second, 10, 60_000)), 60_000, 10)
	open := e.mustReserve(e.request(first, 10, 30_000))

	a := e.manager.Aggregate([]Policy{e.policy})
	app := entry(t, a, "bot", AggregateApp, USDPerDay, KindBudget)
	if app.Used != 0.08 || app.Reserved != 0.03 {
		t.Fatalf("app ratios = %v / %v", app.Used, app.Reserved)
	}
	users := entry(t, a, "bot", AggregateUserMax, USDPerDay, KindBudget)
	// The largest single user, never the sum of both (0.8) nor a ratio of sums.
	if users.Used != 0.6 || users.Reserved != 0.3 {
		t.Fatalf("user max ratios = %v / %v", users.Used, users.Reserved)
	}
	if a.Apps[0].UsersObserved != 2 {
		t.Fatalf("users observed = %d", a.Apps[0].UsersObserved)
	}
	for _, view := range a.Apps {
		for _, x := range view.Entries {
			if strings.Contains(string(x.Scope), first.Hash) || strings.Contains(string(x.Scope), second.Hash) {
				t.Fatal("aggregate carries a subject")
			}
		}
	}
	e.settle(open, 30_000, 10)
}

func TestBudgetUnknownAndUnconfigured(t *testing.T) {
	e := newEnv(t, Policy{AppSet: Set{HardCap: config.Budget{TokensPerDay: 1000}}, UserSet: Set{Budget: config.Budget{TokensPerMonth: 500}}})
	a := e.manager.Aggregate([]Policy{e.policy})
	if hasEntry(a, AggregateApp, USDPerDay) || hasEntry(a, AggregateUserMax, USDPerDay) {
		t.Fatal("an unconfigured ceiling produced an entry")
	}
	// A configured per-user ceiling without active users is zero, not absent.
	if users := entry(t, a, "bot", AggregateUserMax, TokensPerMonth, KindBudget); users.Used != 0 || users.Unknown {
		t.Fatalf("empty user max = %+v", users)
	}
	e.settle(e.mustReserve(e.request(user("bot", 1), 100, 0)), 0, 100)
	e.manager.mu.Lock()
	for _, b := range e.manager.buckets {
		b.unknown = true
	}
	e.manager.mu.Unlock()
	a = e.manager.Aggregate([]Policy{e.policy})
	if !entry(t, a, "bot", AggregateApp, TokensPerDay, KindHardCap).Unknown || !entry(t, a, "bot", AggregateUserMax, TokensPerMonth, KindBudget).Unknown {
		t.Fatal("unknown state was not reported")
	}
	e.manager.mu.Lock()
	e.manager.degraded = true
	e.manager.mu.Unlock()
	if !e.manager.Aggregate([]Policy{e.policy}).Degraded {
		t.Fatal("degraded state was not reported")
	}
}

func TestBudgetRolloverAndForget(t *testing.T) {
	e := newEnv(t, Policy{AppSet: Set{Budget: config.Budget{USDPerDay: 100_000}}, UserSet: Set{Budget: config.Budget{USDPerDay: 50_000}}})
	someone := user("bot", 7)
	e.settle(e.mustReserve(e.request(someone, 10, 25_000)), 25_000, 10)
	if got := entry(t, e.manager.Aggregate([]Policy{e.policy}), "bot", AggregateUserMax, USDPerDay, KindBudget).Used; got != 0.5 {
		t.Fatalf("user max before forget = %v", got)
	}
	if _, err := e.manager.ForgetSubjects([]storage.SubjectRef{{App: "bot", Hash: someone.Hash}}, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	a := e.manager.Aggregate([]Policy{e.policy})
	if got := entry(t, a, "bot", AggregateUserMax, USDPerDay, KindBudget).Used; got != 0 || a.Apps[0].UsersObserved != 0 {
		t.Fatalf("forgotten user still in the aggregate: %v, %d users", got, a.Apps[0].UsersObserved)
	}
	if got := entry(t, a, "bot", AggregateApp, USDPerDay, KindBudget).Used; got != 0.25 {
		t.Fatalf("forget changed the app total: %v", got)
	}
	// The next civil day opens new windows: both ratios start again.
	e.clock.SetCivil(start.Add(24 * time.Hour))
	a = e.manager.Aggregate([]Policy{e.policy})
	if got := entry(t, a, "bot", AggregateApp, USDPerDay, KindBudget).Used; got != 0 {
		t.Fatalf("app ratio after rollover = %v", got)
	}
}
