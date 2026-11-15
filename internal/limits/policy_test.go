package limits

import (
	"errors"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

func policyEnv(t *testing.T, onLimit config.OnLimit, app, perUser Set) *env {
	t.Helper()
	return newEnv(t, Policy{OnLimit: onLimit, DowngradeTo: "cheap", AppSet: app, UserSet: perUser})
}

func original(r Request) Candidate {
	return Candidate{ID: CandidateOriginal, Model: "main", EstimatedTokens: r.EstimatedTokens, ReservedCost: r.ReservedCost}
}

func target(tokens int64, cost config.Microusd) Candidate {
	return Candidate{ID: CandidateDowngrade, Model: "cheap", EstimatedTokens: tokens, ReservedCost: cost}
}

func (e *env) choose(r Request, candidates ...Candidate) (*Handle, Decision, Selection, error) {
	return e.manager.ReserveSelect(e.policy, r, candidates, func(res storage.Reservation, _ Selection) (int64, error) {
		return e.store.commit(res)
	})
}

// spend admits and settles one plain request of the given cost.
func (e *env) spend(s Subject, cost config.Microusd) {
	e.t.Helper()
	saved := e.policy.OnLimit
	e.policy.OnLimit = config.OnLimitWarn
	r := e.request(s, 1, cost)
	h, d, _, err := e.choose(r, original(r))
	e.policy.OnLimit = saved
	if err != nil || !d.Allowed {
		e.t.Fatalf("setup spend refused: %+v %v", d.Primary, err)
	}
	e.settle(h, cost, 1)
}

func budget(usd config.Microusd) config.Budget { return config.Budget{USDPerDay: usd} }

func TestWarnOnlyRelaxesCalendarBudgets(t *testing.T) {
	e := policyEnv(t, config.OnLimitWarn, Set{Budget: budget(100)}, Set{})
	u := user("bot", 1)
	e.spend(u, 100)
	r := e.request(u, 1, 10)
	h, d, sel, err := e.choose(r, original(r))
	if err != nil || !d.Allowed || h == nil || sel.Outcome != PolicyWarned || sel.Candidate != CandidateOriginal {
		t.Fatalf("warn did not forward: %+v %+v %v", d, sel, err)
	}
	if sel.Trigger == nil || sel.Trigger.Kind != KindBudget || sel.Trigger.LimitName != "apps.bot.budget.usd_per_day" || sel.Trigger.RenewsAt == nil {
		t.Fatalf("trigger: %+v", sel.Trigger)
	}

	// Capacity limits stay mandatory, even next to a budget the policy relaxes.
	e = policyEnv(t, config.OnLimitWarn, Set{Budget: budget(100), Limits: config.Limits{MaxConcurrent: 1}}, Set{})
	e.spend(u, 100)
	held := e.request(u, 1, 0)
	if _, d, _, err := e.choose(held, original(held)); err != nil || !d.Allowed {
		t.Fatalf("setup: %+v %v", d, err)
	}
	r = e.request(u, 1, 10)
	_, d, sel, err = e.choose(r, original(r))
	if err != nil || d.Allowed || d.Primary == nil || d.Primary.Kind != KindConcurrency || sel.Outcome != PolicyBlocked {
		t.Fatalf("concurrency relaxed by warn: %+v %+v %v", d.Primary, sel, err)
	}
}

func TestDowngradeKeepsRateAndConcurrencyMandatory(t *testing.T) {
	cases := map[string]config.Limits{
		"rpm": {RequestsPerMinute: 1},
		"tpm": {TokensPerMinute: 50},
	}
	for name, limits := range cases {
		t.Run(name, func(t *testing.T) {
			e := policyEnv(t, config.OnLimitDowngrade, Set{Budget: budget(100), HardCap: budget(1000), Limits: limits}, Set{})
			u := user("bot", 1)
			e.spend(u, 100)
			r := e.request(u, 60, 10)
			_, d, sel, err := e.choose(r, original(r), target(1, 1))
			if err != nil || d.Allowed || d.Primary == nil || d.Primary.Kind != KindRate || sel.Outcome != PolicyBlocked {
				t.Fatalf("downgrade bypassed a rate limit: %+v %+v %v", d.Primary, sel, err)
			}
		})
	}
}

func TestReservationBudgetTriggersPolicy(t *testing.T) {
	e := policyEnv(t, config.OnLimitWarn, Set{Budget: budget(100)}, Set{})
	u := user("bot", 1)
	held := e.request(u, 1, 80)
	if _, d, _, err := e.choose(held, original(held)); err != nil || !d.Allowed {
		t.Fatalf("setup: %+v %v", d, err)
	}
	r := e.request(u, 1, 30)
	_, d, sel, err := e.choose(r, original(r))
	if err != nil || !d.Allowed || sel.Outcome != PolicyWarned || sel.Trigger.Kind != KindReservation || sel.Trigger.Reason != ReasonReservation {
		t.Fatalf("reservation budget: %+v %+v %v", d, sel, err)
	}
}

func TestUnknownBudgetNeverBecomesWarning(t *testing.T) {
	for _, onLimit := range []config.OnLimit{config.OnLimitWarn, config.OnLimitDowngrade} {
		e := policyEnv(t, onLimit, Set{Budget: budget(100), HardCap: budget(120)}, Set{})
		u := user("bot", 1)
		w, err := windowFor(start, USDPerDay, "UTC")
		if err != nil {
			t.Fatal(err)
		}
		e.manager.bucket(subjectKey("bot", u.appSubject(), USDPerDay, w)).unknown = true
		r := e.request(u, 1, 1)
		_, d, sel, err := e.choose(r, original(r), target(1, 1))
		if err != nil || d.Allowed || sel.Outcome != PolicyBlocked || d.Primary == nil || !d.Primary.Unknown {
			t.Fatalf("%s relaxed an unknown budget: %+v %+v %v", onLimit, d.Primary, sel, err)
		}
	}
}

func TestHardCapChecksSelectedCandidate(t *testing.T) {
	e := policyEnv(t, config.OnLimitDowngrade, Set{Budget: budget(100), HardCap: budget(120)}, Set{})
	u := user("bot", 1)
	e.spend(u, 100)

	// The original alone would cross the cap; the cheaper target still fits.
	r := e.request(u, 1, 30)
	h, d, sel, err := e.choose(r, original(r), target(1, 10))
	if err != nil || !d.Allowed || sel.Outcome != PolicyDowngraded || sel.Candidate != CandidateDowngrade || sel.Model != "cheap" {
		t.Fatalf("viable target refused: %+v %+v %v", d.Primary, sel, err)
	}
	if h.request.ReservedCost != 10 || e.store.reservations[h.key].Quote.Candidate != string(CandidateDowngrade) {
		t.Fatalf("reserved the wrong candidate: %+v", h.request)
	}

	// A target that crosses the cap too is refused by the cap, not by the budget.
	r = e.request(u, 1, 30)
	_, d, sel, err = e.choose(r, original(r), target(1, 15))
	if err != nil || d.Allowed || d.Primary == nil || d.Primary.Kind != KindHardCap || sel.Outcome != PolicyBlocked {
		t.Fatalf("cap not enforced on the target: %+v %+v %v", d.Primary, sel, err)
	}

	// Token caps are enforced on the selected candidate as well.
	e = policyEnv(t, config.OnLimitDowngrade, Set{Budget: config.Budget{TokensPerDay: 100}, HardCap: config.Budget{TokensPerDay: 120}}, Set{})
	first := e.request(u, 100, 0)
	if h, d, _, err := e.choose(first, original(first)); err != nil || !d.Allowed {
		t.Fatalf("setup: %+v %v", d, err)
	} else {
		e.settle(h, 0, 100)
	}
	r = e.request(u, 30, 0)
	_, d, _, err = e.choose(r, original(r), target(30, 0))
	if err != nil || d.Allowed || d.Primary.Kind != KindHardCap || d.Primary.Dimension != TokensPerDay {
		t.Fatalf("token cap: %+v %v", d.Primary, err)
	}
}

func TestHardCapAtEqualityBlocksNextRequest(t *testing.T) {
	e := policyEnv(t, config.OnLimitWarn, Set{Budget: budget(100), HardCap: budget(120)}, Set{})
	u := user("bot", 1)
	e.spend(u, 100)
	r := e.request(u, 1, 20)
	h, d, sel, err := e.choose(r, original(r))
	if err != nil || !d.Allowed || sel.Outcome != PolicyWarned {
		t.Fatalf("landing exactly on the cap must be admitted: %+v %v", d.Primary, err)
	}
	e.settle(h, 20, 1)
	free := e.request(u, 0, 0)
	_, d, _, err = e.choose(free, original(free))
	if err != nil || d.Allowed || d.Primary == nil || d.Primary.Kind != KindHardCap || d.Primary.Reason != ReasonSpent {
		t.Fatalf("a reached cap must block even a free request: %+v %v", d.Primary, err)
	}
}

func TestHardCapInOtherScopeWins(t *testing.T) {
	e := policyEnv(t, config.OnLimitWarn, Set{Budget: budget(100)}, Set{Budget: budget(50), HardCap: budget(60)})
	u := user("bot", 1)
	e.spend(u, 60)
	r := e.request(u, 1, 1)
	_, d, sel, err := e.choose(r, original(r))
	if err != nil || d.Allowed || d.Primary == nil || d.Primary.Kind != KindHardCap || d.Primary.Scope != ScopeUser || sel.Outcome != PolicyBlocked {
		t.Fatalf("relaxed app budget hid the user cap: %+v %+v %v", d.Primary, sel, err)
	}
}

func TestAlreadyTargetDoesNotRecurse(t *testing.T) {
	e := policyEnv(t, config.OnLimitDowngrade, Set{Budget: budget(100), HardCap: budget(200)}, Set{})
	u := user("bot", 1)
	e.spend(u, 100)
	r := e.request(u, 1, 10)
	asked := Candidate{ID: CandidateOriginal, Model: "cheap", EstimatedTokens: 1, ReservedCost: 10}
	_, d, sel, err := e.choose(r, asked)
	if err != nil || !d.Allowed || sel.Outcome != PolicyDowngraded || sel.Candidate != CandidateOriginal || sel.Model != "cheap" {
		t.Fatalf("request already on the target: %+v %+v %v", d.Primary, sel, err)
	}

	// Without a prepared target the downgrade cannot happen, and nothing is reserved.
	r = e.request(u, 1, 10)
	before := len(e.store.reservations)
	_, _, sel, err = e.choose(r, original(r), Candidate{ID: CandidateDowngrade, Model: "cheap", Unavailable: true})
	if !errors.Is(err, ErrTargetUnavailable) || sel.Outcome != PolicyBlocked || len(e.store.reservations) != before {
		t.Fatalf("unavailable target: %+v %v", sel, err)
	}
}

func TestNoPolicyBeforeBudgetViolation(t *testing.T) {
	e := policyEnv(t, config.OnLimitDowngrade, Set{Budget: budget(100), HardCap: budget(120)}, Set{})
	u := user("bot", 1)
	r := e.request(u, 1, 50)
	h, d, sel, err := e.choose(r, original(r), target(1, 1))
	if err != nil || !d.Allowed || sel.Outcome != PolicyNone || sel.Candidate != CandidateOriginal || sel.Trigger != nil {
		t.Fatalf("downgraded before any budget was exceeded: %+v %v", sel, err)
	}
	if q := e.store.reservations[h.key].Quote; q.Candidate != "" || q.PolicyOutcome != "" || q.ReservedCost != 50 {
		t.Fatalf("plain admission quote changed: %+v", q)
	}

	// Without a relaxable budget, a hard cap alone never triggers a downgrade.
	e = policyEnv(t, config.OnLimitDowngrade, Set{}, Set{Budget: budget(100), HardCap: budget(100)})
	e.spend(u, 100)
	r = e.request(u, 1, 0)
	_, d, sel, err = e.choose(r, original(r), target(1, 0))
	if err != nil || d.Allowed || sel.Outcome != PolicyBlocked || d.Primary.Kind != KindHardCap {
		t.Fatalf("hard cap alone: %+v %+v %v", d.Primary, sel, err)
	}

	// block keeps the F06 answer.
	e = policyEnv(t, config.OnLimitBlock, Set{Budget: budget(100)}, Set{})
	e.spend(u, 100)
	r = e.request(u, 1, 1)
	_, d, sel, err = e.choose(r, original(r), target(1, 1))
	if err != nil || d.Allowed || d.Primary.Kind != KindBudget || sel.Outcome != PolicyBlocked {
		t.Fatalf("block: %+v %+v %v", d.Primary, sel, err)
	}
}
