package metering

import (
	"context"
	"errors"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/limits"
	"github.com/JoaoVictorVM/comporta/internal/proxy"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

func replayed() proxy.AttemptResult {
	return proxy.AttemptResult{Operation: proxy.OperationGeneration, NotSent: true, CacheHit: true, AttemptIndex: 1,
		DownstreamStatus: proxy.Optional[int]{Value: 200, Known: true}, Committed: true}
}

func reservedUSD(t *testing.T, s *Service, policy limits.Policy) (reserved, spent int64) {
	t.Helper()
	for _, v := range s.limits.(*limits.Manager).State(policy, nil).Limits {
		if v.Dimension == limits.USDPerDay {
			return v.Reserved, v.Used
		}
	}
	t.Fatal("no daily budget in state")
	return 0, 0
}

func TestCachedIsNotBlocked(t *testing.T) {
	s, store, snapshot, e := policySetup(t, "    budget: {usd_per_day: 100}\n")
	admission, reject := s.AdmitCandidates(context.Background(), snapshot, e, candidates(snapshot, e, false))
	if reject != nil || admission.Conclude == nil {
		t.Fatalf("admission: %+v %+v", admission, reject)
	}
	if err := admission.Conclude(replayed()); err != nil {
		t.Fatal(err)
	}
	r := onlyRecord(t, s, store)
	if r.Outcome != "cached" || r.AttemptIndex != 1 || r.RequestCount != 1 || r.UpstreamStatus != nil || r.DownstreamStatus == nil || *r.DownstreamStatus != 200 {
		t.Fatalf("hit recorded as %+v", r)
	}
	if got := eventsOf(t, store, events.Cached); len(got) != 1 || got[0].RequestID != e.RequestID || got[0].Payload.Outcome != "cached" || got[0].Payload.Cost != 0 {
		t.Fatalf("cached event: %+v", got)
	}
	if got := eventsOf(t, store, events.Blocked); len(got) != 0 {
		t.Fatalf("a hit is never a block: %+v", got)
	}
}

func TestCachedUsageIsZero(t *testing.T) {
	s, store, snapshot, e := policySetup(t, "    budget: {usd_per_day: 100}\n")
	admission, _ := s.AdmitCandidates(context.Background(), snapshot, e, candidates(snapshot, e, false))
	hit := replayed()
	// Whatever the stored payload said, a replay never charges it.
	hit.Usage = proxy.Usage{Input: known(1000), Output: known(1000)}
	hit.UsageComplete = true
	if err := admission.Conclude(hit); err != nil {
		t.Fatal(err)
	}
	r := onlyRecord(t, s, store)
	if r.Cost != 0 || r.ReservedCost == 0 || r.UsageMissing || r.EstimateExceeded || r.Input == nil || *r.Input != 0 || *r.Output != 0 || *r.CacheWrite != 0 || *r.CacheRead != 0 {
		t.Fatalf("hit consumed something: %+v", r)
	}
	policy, _ := limits.PolicyFor(snapshot, "bot")
	if reserved, spent := reservedUSD(t, s, policy); reserved != 0 || spent != 0 {
		t.Fatalf("budget after a hit: reserved %d spent %d", reserved, spent)
	}
	rows, err := store.Daily(context.Background(), storage.Filter{AppName: "bot"})
	if err != nil || len(rows) == 0 || rows[0].Cost != 0 || rows[0].Input != 0 || rows[0].Requests != 1 {
		t.Fatalf("daily: %+v %v", rows, err)
	}
}

func TestCacheReleaseOnce(t *testing.T) {
	s, store, snapshot, e := policySetup(t, "    budget: {usd_per_day: 100}\n")
	admission, _ := s.AdmitCandidates(context.Background(), snapshot, e, candidates(snapshot, e, false))
	if err := admission.Conclude(replayed()); err != nil {
		t.Fatal(err)
	}
	if err := admission.Conclude(answered()); err != nil {
		t.Fatalf("a repeat reports the first conclusion: %v", err)
	}
	admission.Complete(answered())
	r := onlyRecord(t, s, store)
	if r.Outcome != "cached" || r.Cost != 0 {
		t.Fatalf("a second conclusion changed the record: %+v", r)
	}
}

func TestCacheReleaseFailureRetainsReservation(t *testing.T) {
	s, store, snapshot, e := policySetup(t, "    budget: {usd_per_day: 100}\n")
	if err := s.UseLimits(s.limits, failingSettlement{store}, s.source); err != nil {
		t.Fatal(err)
	}
	admission, _ := s.AdmitCandidates(context.Background(), snapshot, e, candidates(snapshot, e, false))
	if err := admission.Conclude(replayed()); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("a failed release must not be reported as concluded: %v", err)
	}
	policy, _ := limits.PolicyFor(snapshot, "bot")
	if reserved, _ := reservedUSD(t, s, policy); reserved == 0 {
		t.Fatal("the reservation stays held until the release is durable")
	}
}

func TestCachedInvalidUpstreamCombination(t *testing.T) {
	s, store, snapshot, e := policySetup(t, "    budget: {usd_per_day: 100}\n")
	admission, _ := s.AdmitCandidates(context.Background(), snapshot, e, candidates(snapshot, e, false))
	forged := answered()
	forged.CacheHit = true
	if err := admission.Conclude(forged); err != nil {
		t.Fatal(err)
	}
	r := onlyRecord(t, s, store)
	if r.Outcome != "ok" || r.Cost == 0 || r.Input == nil || *r.Input != 10 {
		t.Fatalf("a real call can never become a free hit: %+v", r)
	}
	if got := eventsOf(t, store, events.Cached); len(got) != 0 {
		t.Fatalf("cached event for a real call: %+v", got)
	}
}

func TestCachedSessionClosesOnce(t *testing.T) {
	s, store, snapshot, e := policySetup(t, "    budget: {usd_per_day: 100}\n")
	session := openSession(t, s, snapshot, e)
	if err := session.Conclude(replayed()); err != nil {
		t.Fatal(err)
	}
	session.Close(nil)
	session.Close(nil)
	r := onlyRecord(t, s, store)
	if r.Outcome != "cached" || r.RequestCount != 1 || r.Cost != 0 {
		t.Fatalf("session hit: %+v", r)
	}
	policy, _ := limits.PolicyFor(snapshot, "bot")
	if reserved, spent := reservedUSD(t, s, policy); reserved != 0 || spent != 0 {
		t.Fatalf("budget after a session hit: reserved %d spent %d", reserved, spent)
	}
}
