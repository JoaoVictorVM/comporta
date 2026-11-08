package limits

import (
	"errors"
	"math"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

func defaultConcurrency() Policy {
	return Policy{AppSet: Set{Limits: config.Limits{MaxConcurrent: 20}}, UserSet: Set{Limits: config.Limits{MaxConcurrent: 4}}}
}

func TestAppAndUserReserveAtomically(t *testing.T) {
	p := Policy{AppSet: Set{Budget: config.Budget{USDPerDay: 1_000_000}}, UserSet: Set{Budget: config.Budget{USDPerDay: 300_000}}}
	e := newEnv(t, p)
	u := user("bot", 1)
	e.mustReserve(e.request(u, 10, 200_000))
	d := e.mustReject(e.request(u, 10, 200_000))
	if d.Primary.Scope != ScopeUser {
		t.Fatalf("user budget must refuse: %+v", d.Primary)
	}
	// The refusal by the user budget left the app budget untouched.
	if v := e.view(nil, "apps.bot.budget.usd_per_day"); v.Reserved != 200_000 || v.Used != 0 {
		t.Fatalf("partial delta on app: %+v", v)
	}
	e.store.set(func(s *memStore) { s.failCommit = true })
	if _, _, err := e.reserve(e.request(user("bot", 2), 10, 100_000)); !errors.Is(err, errInjected) {
		t.Fatal(err)
	}
	if v := e.view(nil, "apps.bot.budget.usd_per_day"); v.Reserved != 200_000 {
		t.Fatalf("a failed commit changed memory: %+v", v)
	}
}

func TestConcurrentUserLimit(t *testing.T) {
	e := newEnv(t, defaultConcurrency())
	u := user("bot", 1)
	var held []*Handle
	for range 4 {
		held = append(held, e.mustReserve(e.request(u, 1, 1)))
	}
	d := e.mustReject(e.request(u, 1, 1))
	if d.Primary.Kind != KindConcurrency || d.Primary.Limit != 4 || d.Primary.Scope != ScopeUser || d.Primary.RetryAfter != 0 {
		t.Fatalf("fifth call: %+v", d.Primary)
	}
	if err := e.manager.Release(held[0], ReleaseNotSent, nil); err != nil {
		t.Fatal(err)
	}
	e.mustReserve(e.request(u, 1, 1))
	for i := range 16 {
		e.mustReserve(e.request(user("bot", 10+i), 1, 1))
	}
	if d := e.mustReject(e.request(user("bot", 99), 1, 1)); d.Primary.Scope != ScopeApp || d.Primary.Limit != 20 {
		t.Fatalf("app concurrency: %+v", d.Primary)
	}
}

func TestAnonymousBucketsIsolatedByApp(t *testing.T) {
	p := Policy{UserSet: Set{Limits: config.Limits{RequestsPerMinute: 1}}}
	e := newEnv(t, p)
	anonymous := Subject{App: "bot", Hash: "a0000000000000000000000000000001", Anonymous: true}
	e.mustReserve(e.request(anonymous, 1, 0))
	e.mustReject(e.request(anonymous, 1, 0))
	other := e.policy
	other.App = "other"
	r := e.request(Subject{App: "other", Hash: anonymous.Hash, Anonymous: true}, 1, 0)
	if _, d, err := e.manager.Reserve(other, r, e.store.commit); err != nil || !d.Allowed {
		t.Fatalf("anonymous bucket shared across apps: %+v %v", d.Primary, err)
	}
	identified := Subject{App: "bot", Hash: anonymous.Hash}
	e.mustReserve(e.request(identified, 1, 0))
}

func TestStablePrimaryLimit(t *testing.T) {
	p := Policy{
		AppSet:  Set{Limits: config.Limits{RequestsPerMinute: 1, MaxConcurrent: 1}, Budget: config.Budget{USDPerDay: 10, TokensPerMonth: 10}, HardCap: config.Budget{USDPerDay: 10}},
		UserSet: Set{Limits: config.Limits{RequestsPerMinute: 1}, Budget: config.Budget{USDPerDay: 10}},
	}
	for range 5 {
		e := newEnv(t, p)
		u := user("bot", 1)
		e.mustReserve(e.request(u, 1, 1))
		d := e.mustReject(e.request(u, 100, 100))
		want := []string{"app.limits.requests_per_minute", "user.limits.requests_per_minute", "app.limits.max_concurrent", "app.hard_cap.usd_per_day", "app.budget.usd_per_day", "app.budget.tokens_per_month", "user.budget.usd_per_day"}
		if len(d.Exceeded) != len(want) {
			t.Fatalf("exceeded: %+v", d.Exceeded)
		}
		for i, v := range d.Exceeded {
			if v.Key() != want[i] {
				t.Fatalf("order %d: %s, want %s", i, v.Key(), want[i])
			}
		}
		if d.Primary.Key() != want[0] {
			t.Fatal("primary is not the first in the stable order")
		}
		hard := d.Exceeded[3]
		if hard.Kind != KindHardCap || hard.LimitName != "apps.bot.hard_cap.usd_per_day" || !hard.HardCap() {
			t.Fatalf("hard cap: %+v", hard)
		}
	}
}

func TestSpentAndReservationReasonsDiffer(t *testing.T) {
	p := Policy{App: "bot-discord", Timezone: "America/Sao_Paulo", AppSet: Set{Budget: config.Budget{USDPerDay: 2_000_000}}}
	spent := newEnv(t, p)
	u := user("bot-discord", 1)
	spent.settle(spent.mustReserve(spent.request(u, 1, 1_970_000)), 1_970_000, 1)
	d := spent.mustReject(spent.request(u, 1, 50_000))
	if d.Primary.Kind != KindBudget || d.Primary.Reason != ReasonSpent || d.Primary.Used != 1_970_000 || d.Primary.Reserved != 0 || d.Primary.RenewsAt == nil {
		t.Fatalf("spent: %+v", d.Primary)
	}
	if got := Message(*d.Primary, "bot-discord", spent.clock.Now()); got != "daily budget of $2.00 for app bot-discord would be exceeded (spent $1.97, reserved $0.00); renews at 00:00 America/Sao_Paulo (in 15h0m)" {
		t.Fatalf("message: %s", got)
	}

	reserved := newEnv(t, p)
	reserved.settle(reserved.mustReserve(reserved.request(u, 1, 1_900_000)), 1_900_000, 1)
	reserved.mustReserve(reserved.request(u, 1, 80_000))
	d = reserved.mustReject(reserved.request(u, 1, 50_000))
	if d.Primary.Kind != KindReservation || d.Primary.Reason != ReasonReservation || d.Primary.Reserved != 80_000 {
		t.Fatalf("reservation: %+v", d.Primary)
	}
}

func TestSettlementAndReleaseIdempotent(t *testing.T) {
	e := newEnv(t, Policy{AppSet: Set{Budget: config.Budget{USDPerDay: 1_000_000}, Limits: config.Limits{MaxConcurrent: 2}}})
	u := user("bot", 1)
	h := e.mustReserve(e.request(u, 100, 500_000))
	e.settle(h, 100_000, 50)
	e.settle(h, 900_000, 50)
	if err := e.manager.Release(h, ReleaseNotSent, nil); err != nil {
		t.Fatal(err)
	}
	v := e.view(nil, "apps.bot.budget.usd_per_day")
	if v.Used != 100_000 || v.Reserved != 0 {
		t.Fatalf("settled twice: %+v", v)
	}
	if c := e.view(nil, "apps.bot.limits.max_concurrent"); c.Used != 0 {
		t.Fatalf("slot freed twice or never: %+v", c)
	}
	r := e.request(u, 10, 10)
	first := e.mustReserve(r)
	again, _, err := e.reserve(r)
	if err != nil || again != first {
		t.Fatal("repeated reservation with identical data must return the same handle")
	}
	changed := r
	changed.ReservedCost++
	if _, _, err := e.reserve(changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting reservation: %v", err)
	}
}

func TestRejectedAdmissionConsumesNothing(t *testing.T) {
	p := Policy{AppSet: Set{Limits: config.Limits{RequestsPerMinute: 2, TokensPerMinute: 100, MaxConcurrent: 5}, Budget: config.Budget{USDPerDay: 1000}}}
	e := newEnv(t, p)
	u := user("bot", 1)
	e.mustReserve(e.request(u, 10, 10))
	before := e.manager.State(e.policy, &u)
	for range 10 {
		e.mustReject(e.request(u, 10, 5000))
	}
	after := e.manager.State(e.policy, &u)
	for i := range before.Limits {
		b, a := before.Limits[i], after.Limits[i]
		b.RenewsAt, a.RenewsAt = nil, nil
		if b != a {
			t.Fatalf("a refusal changed %s: %+v -> %+v", before.Limits[i].LimitName, before.Limits[i], after.Limits[i])
		}
	}
	if after.Concurrent != 1 {
		t.Fatalf("concurrency %d", after.Concurrent)
	}
	if len(e.store.ledger) != 1 {
		t.Fatal("a refusal wrote to the ledger")
	}
}

func TestCountTokensOnlyUsesRateAndConcurrency(t *testing.T) {
	p := Policy{AppSet: Set{Limits: config.Limits{RequestsPerMinute: 3, TokensPerMinute: 1, MaxConcurrent: 2}, Budget: config.Budget{USDPerDay: 1, TokensPerDay: 1}}}
	e := newEnv(t, p)
	u := user("bot", 1)
	r := e.request(u, 0, 0)
	r.Operation = OperationCountTokens
	h, d, err := e.reserve(r)
	if err != nil || !d.Allowed {
		t.Fatalf("count_tokens refused: %+v %v", d.Primary, err)
	}
	if len(h.items) != 0 {
		t.Fatal("count_tokens reserved a budget")
	}
	if c := e.view(nil, "apps.bot.limits.max_concurrent"); c.Used != 1 {
		t.Fatal("count_tokens must hold a concurrency slot")
	}
	if err := e.manager.Settle(h, Usage{Tokens: 5000, Complete: true}, nil); err != nil {
		t.Fatal(err)
	}
	if v := e.view(nil, "apps.bot.limits.tokens_per_minute"); v.Used != 0 {
		t.Fatalf("count_tokens consumed TPM: %+v", v)
	}
	if v := e.view(nil, "apps.bot.limits.requests_per_minute"); v.Used != 1 {
		t.Fatalf("count_tokens must count as a request: %+v", v)
	}
}

func TestOverflowNeverFailsOpen(t *testing.T) {
	p := Policy{FailMode: config.FailOpen, AppSet: Set{Budget: config.Budget{TokensPerDay: math.MaxInt64}}}
	e := newEnv(t, p)
	u := user("bot", 1)
	e.mustReserve(e.request(u, math.MaxInt64-1, 0))
	if _, _, err := e.reserve(e.request(u, 10, 0)); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("overflow must be an integrity failure: %v", err)
	}
	bad := e.request(u, -1, 0)
	if _, _, err := e.reserve(bad); !errors.Is(err, ErrIntegrity) {
		t.Fatal("negative estimate accepted")
	}
	if err := e.manager.Settle(&Handle{state: storage.ReservationReserved}, Usage{Cost: -1}, nil); !errors.Is(err, ErrIntegrity) {
		t.Fatal("negative usage accepted")
	}
}

func TestStaleGenerationIsRefused(t *testing.T) {
	e := newEnv(t, Policy{})
	u := user("bot", 1)
	r := e.request(u, 1, 1)
	r.Generation = 5
	e.mustReserve(r)
	old := e.request(u, 1, 1)
	old.Generation = 4
	if _, _, err := e.reserve(old); !errors.Is(err, ErrStale) {
		t.Fatalf("old generation admitted: %v", err)
	}
}

func TestBudgetHeaderPicksSmallestFraction(t *testing.T) {
	p := Policy{AppSet: Set{Budget: config.Budget{USDPerDay: 1_000_000, TokensPerMonth: 1000}}, UserSet: Set{Budget: config.Budget{USDPerDay: 100_000}}}
	e := newEnv(t, p)
	_, d, err := e.reserve(e.request(user("bot", 1), 100, 50_000))
	if err != nil || d.Budget == nil {
		t.Fatal(err)
	}
	if d.Budget.LimitName != "apps.bot.per_user.budget.usd_per_day" || d.Budget.Remaining != 50_000 || d.Budget.Unit != UnitMicrousd {
		t.Fatalf("most restrictive budget: %+v", d.Budget)
	}
}
