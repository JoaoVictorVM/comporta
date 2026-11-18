package limits

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// The shared decision must equal the local one for the same state: both run
// the same evaluation and the same policy, only the state source differs.
func TestDecideMatchesLocalCoordinator(t *testing.T) {
	start := time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)
	clock := NewFakeClock(start)
	local := New(Options{Clock: clock})
	p := Policy{App: "bot", Timezone: "UTC", FailMode: config.FailClosed, OnLimit: config.OnLimitDowngrade, DowngradeTo: "cheap",
		AppSet:  Set{Limits: config.Limits{RequestsPerMinute: 30, TokensPerMinute: 5_000, MaxConcurrent: 6}, Budget: config.Budget{USDPerDay: 9_000}, HardCap: config.Budget{USDPerDay: 12_000}},
		UserSet: Set{Limits: config.Limits{MaxConcurrent: 4}, Budget: config.Budget{TokensPerDay: 20_000}}}
	user := Subject{App: "bot", Hash: "11111111111111111111111111111111"}
	rng := rand.New(rand.NewPCG(1, 2))
	var shared SharedState
	shared.Rates = map[Subject][]SharedRate{}
	shared.Concurrent = map[Subject]int64{}
	var open []*Handle
	for i := range 200 {
		clock.Advance(time.Duration(rng.IntN(3000)) * time.Millisecond)
		shared.Now = clock.Now()
		r := Request{RequestID: fmt.Sprintf("%032x", i+1), AttemptIndex: 1, Subject: user, Operation: OperationGeneration}
		cost := config.Microusd(rng.IntN(900) + 1)
		candidates := []Candidate{{ID: CandidateOriginal, Model: "m", EstimatedTokens: int64(rng.IntN(800) + 1), ReservedCost: cost},
			{ID: CandidateDowngrade, Model: "cheap", EstimatedTokens: 50, ReservedCost: cost / 4}}
		plan, err := Decide(p, r, candidates, shared, false)
		if err != nil {
			t.Fatal(err)
		}
		h, d, sel, err := local.ReserveSelect(p, r, candidates, func(storage.Reservation, Selection) (int64, error) { return 0, nil })
		if err != nil {
			t.Fatal(err)
		}
		if d.Allowed != plan.Decision.Allowed || sel != plan.Selection || !reflect.DeepEqual(d.Budget, plan.Decision.Budget) {
			t.Fatalf("step %d: local %+v %+v, shared %+v %+v", i, d, sel, plan.Decision, plan.Selection)
		}
		if !d.Allowed {
			if d.Primary.Key() != plan.Decision.Primary.Key() || d.Primary.Reason != plan.Decision.Primary.Reason {
				t.Fatalf("step %d: primary %s/%s != %s/%s", i, d.Primary.Key(), d.Primary.Reason, plan.Decision.Primary.Key(), plan.Decision.Primary.Reason)
			}
			continue
		}
		applyPlan(&shared, plan)
		open = append(open, h)
		// Settle some attempts at a random cost, on both sides.
		if rng.IntN(3) == 0 && len(open) > 0 {
			k := rng.IntN(len(open))
			done := open[k]
			open = append(open[:k], open[k+1:]...)
			u := Usage{Cost: config.Microusd(rng.IntN(1200)), Tokens: int64(rng.IntN(900)), Complete: true}
			res := storage.Reservation{RequestID: done.request.RequestID, AttemptIndex: 1, AppName: "bot", Subject: user.Hash,
				Quote: storage.LimitQuote{Operation: "generation", EstimatedTokens: done.request.EstimatedTokens, ReservedCost: done.request.ReservedCost},
				Items: done.items, Rate: &storage.RateEvent{}}
			st := SettlementFor(res, storage.TransitionSettle, u, clock.Now())
			if err := local.Settle(done, u, func(storage.Settlement) (int64, error) { return 0, nil }); err != nil {
				t.Fatal(err)
			}
			settlePlan(&shared, res, st, user)
		}
	}
}

func bucketIndex(s *SharedState, item storage.LimitItem) int {
	for i, b := range s.Buckets {
		if b.Scope == item.Scope && b.Subject == item.Subject && b.Dimension == item.Dimension && b.WindowStart == item.WindowStart && b.WindowEnd == item.WindowEnd {
			return i
		}
	}
	s.Buckets = append(s.Buckets, SharedBucket{BudgetBucket: storage.BudgetBucket{AppName: "bot", Scope: item.Scope, Subject: item.Subject, Anonymous: item.Anonymous,
		Dimension: item.Dimension, WindowStart: item.WindowStart, WindowEnd: item.WindowEnd}})
	return len(s.Buckets) - 1
}

func applyPlan(s *SharedState, plan Plan) {
	res := plan.Reservation
	for _, item := range res.Items {
		s.Buckets[bucketIndex(s, item)].Reserved += item.Reserved
	}
	user := SubjectOf(res)
	for _, subject := range []Subject{{App: "bot"}, user} {
		s.Rates[subject] = append(s.Rates[subject], SharedRate{RequestID: res.RequestID, Attempt: 1, At: res.AdmittedAt, Tokens: res.Rate.EstimatedTokens, Active: true})
		s.Concurrent[subject]++
	}
}

func settlePlan(s *SharedState, res storage.Reservation, st storage.Settlement, user Subject) {
	for i, item := range res.Items {
		b := &s.Buckets[bucketIndex(s, item)]
		b.Reserved -= item.Reserved
		b.Spent += st.Settled[i]
	}
	for _, subject := range []Subject{{App: "bot"}, user} {
		for k, e := range s.Rates[subject] {
			if e.RequestID == res.RequestID {
				s.Rates[subject][k].Tokens, s.Rates[subject][k].Active = *st.RateTokens, false
			}
		}
		s.Concurrent[subject]--
	}
}
