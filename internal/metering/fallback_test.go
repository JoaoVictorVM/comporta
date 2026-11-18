package metering

import (
	"context"
	"errors"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/limits"
	"github.com/JoaoVictorVM/comporta/internal/proxy"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/storage/sqlite"
)

func overloaded() proxy.AttemptResult {
	return proxy.AttemptResult{Operation: proxy.OperationGeneration, UpstreamStatus: proxy.Optional[int]{Value: 529, Known: true},
		ErrorClass: proxy.ClassOverload, ErrorCode: proxy.CodeProviderOverloaded}
}

func answered() proxy.AttemptResult {
	return proxy.AttemptResult{Operation: proxy.OperationGeneration, UpstreamStatus: proxy.Optional[int]{Value: 200, Known: true}, DownstreamStatus: proxy.Optional[int]{Value: 200, Known: true},
		UsageComplete: true, Usage: proxy.Usage{Input: known(10), Output: known(2), CacheWrite: known(0), CacheRead: known(0)}}
}

func openSession(t *testing.T, s *Service, snapshot *config.Snapshot, e *proxy.Envelope) proxy.AccountingSession {
	t.Helper()
	session, admission, reject := s.AdmitSession(context.Background(), snapshot, e, candidates(snapshot, e, false))
	if reject != nil || session == nil || admission.Complete != nil {
		t.Fatalf("session not opened: %+v %+v", admission, reject)
	}
	return session
}

func onModel(e *proxy.Envelope, model string) *proxy.Envelope {
	next := *e
	next.ModelEffective = model
	return &next
}

func records(t *testing.T, s *Service, store *sqlite.Store) []storage.Record {
	t.Helper()
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	page, err := store.Records(context.Background(), storage.RecordFilter{AppName: "bot", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	out := map[int]storage.Record{}
	for _, r := range page.Records {
		out[r.AttemptIndex] = r
	}
	list := make([]storage.Record, 0, len(out))
	for i := 1; i <= len(out); i++ {
		list = append(list, out[i])
	}
	return list
}

func TestFallbackPricesEachAttempt(t *testing.T) {
	s, store, snapshot, e := policySetup(t, "")
	session := openSession(t, s, snapshot, e)
	if err := session.Conclude(overloaded()); err != nil {
		t.Fatal(err)
	}
	next := onModel(e, "cheap")
	admission, reject := session.Continue(context.Background(), next, candidates(snapshot, next, false), "main", "529")
	if reject != nil || admission.Selected != proxy.CandidateOriginal {
		t.Fatalf("continuation refused: %+v", reject)
	}
	if err := session.Conclude(answered()); err != nil {
		t.Fatal(err)
	}
	session.Close(nil)
	session.Close(nil)
	list := records(t, s, store)
	if len(list) != 2 {
		t.Fatalf("records %+v", list)
	}
	first, second := list[0], list[1]
	// Each attempt is quoted with its own model's price: the expensive model
	// reserves ten times the cheap one for the same body.
	if first.ModelUsed != "main" || second.ModelUsed != "cheap" || first.ReservedCost != 10*second.ReservedCost || first.ReservedCost == 0 {
		t.Fatalf("quotes %d %d", first.ReservedCost, second.ReservedCost)
	}
	if first.RequestCount != 0 || second.RequestCount != 1 || second.Outcome != "fallback" || second.FallbackFrom != "main" || second.FallbackCause != "529" || second.ModelRequested != "main" {
		t.Fatalf("roles %+v %+v", first, second)
	}
	if second.Cost != 32 {
		t.Fatalf("cost of the answer %d", second.Cost)
	}
	rows, err := store.Daily(context.Background(), storage.Filter{AppName: "bot"})
	if err != nil || len(rows) != 1 || rows[0].Requests != 1 || rows[0].Attempts != 2 || rows[0].Cost != first.Cost+second.Cost {
		t.Fatalf("daily %+v %v", rows, err)
	}
	fallback := eventsOf(t, store, events.Fallback)
	if len(fallback) != 1 || fallback[0].Payload.FromModel != "main" || fallback[0].Payload.ToModel != "cheap" || fallback[0].Payload.AttemptIndex != 2 {
		t.Fatalf("fallback events %+v", fallback)
	}
	if len(s.slots) != 0 {
		t.Fatalf("slots left %d", len(s.slots))
	}
}

func TestFallbackIncompleteUsageKeepsReservationCost(t *testing.T) {
	s, store, snapshot, e := policySetup(t, "")
	session := openSession(t, s, snapshot, e)
	partial := overloaded()
	partial.Usage = proxy.Usage{Input: known(3)}
	if err := session.Conclude(partial); err != nil {
		t.Fatal(err)
	}
	next := onModel(e, "cheap")
	if _, reject := session.Continue(context.Background(), next, candidates(snapshot, next, false), "main", "529"); reject != nil {
		t.Fatal(reject)
	}
	if err := session.Conclude(overloaded()); err != nil {
		t.Fatal(err)
	}
	session.Close(&proxy.Failure{Status: 502, Code: "fallback_exhausted"})
	list := records(t, s, store)
	if len(list) != 2 {
		t.Fatalf("records %+v", list)
	}
	// A failure without complete usage never becomes free: it is charged at its
	// reservation, whatever the status was.
	for _, r := range list {
		if !r.UsageMissing || r.Cost != r.ReservedCost || r.Cost == 0 {
			t.Fatalf("attempt %d charged %d of %d", r.AttemptIndex, r.Cost, r.ReservedCost)
		}
	}
	last := list[1]
	if last.TerminalErrorCode != "fallback_exhausted" || last.TerminalStatus == nil || *last.TerminalStatus != 502 || last.ErrorCode != "provider_overloaded" {
		t.Fatalf("terminal answer %+v", last)
	}
}

// failingSettlement makes the settlement write fail while admissions succeed.
type failingSettlement struct{ *sqlite.Store }

func (f failingSettlement) CompleteSettled(context.Context, storage.Record, storage.Settlement) (int64, error) {
	return 0, storage.ErrUnavailable
}

func TestFallbackSettlementFailureStops(t *testing.T) {
	s, store, snapshot, e := policySetup(t, "    budget: {usd_per_day: 100}\n")
	if err := s.UseLimits(s.limits, failingSettlement{store}, s.source); err != nil {
		t.Fatal(err)
	}
	session := openSession(t, s, snapshot, e)
	if err := session.Conclude(overloaded()); !errors.Is(err, storage.ErrUnavailable) {
		t.Fatalf("settlement failure hidden: %v", err)
	}
	next := onModel(e, "cheap")
	_, reject := session.Continue(context.Background(), next, candidates(snapshot, next, false), "main", "529")
	if reject == nil || reject.Status != 503 || reject.Code != proxy.CodeUsageStorageUnavailable {
		t.Fatalf("next attempt admitted after a failed settlement: %+v", reject)
	}
	session.Close(&proxy.Failure{Status: 503, Code: proxy.CodeUsageStorageUnavailable})
	if n := len(eventsOf(t, store, events.Fallback)); n != 0 {
		t.Fatalf("fallback events %d", n)
	}
	// The reservation stays held: the limit worker retries it.
	policy, _ := limits.PolicyFor(snapshot, "bot")
	for _, v := range s.limits.(*limits.Manager).State(policy, nil).Limits {
		if v.Dimension == limits.USDPerDay && v.Reserved == 0 {
			t.Fatalf("reservation released after a failed settlement: %+v", v)
		}
	}
}

func TestFallbackRefusalKeepsPreviousTerminal(t *testing.T) {
	s, store, snapshot, e := policySetup(t, "    budget: {usd_per_day: 0.0125}\n")
	session := openSession(t, s, snapshot, e)
	if err := session.Conclude(overloaded()); err != nil {
		t.Fatal(err)
	}
	// The failed attempt was charged at its reservation; the next one no
	// longer fits the budget, and no fallback is used to get around it.
	next := onModel(e, "main")
	_, reject := session.Continue(context.Background(), next, candidates(snapshot, next, false), "main", "529")
	if reject == nil || reject.Status != 429 || reject.Code != proxy.CodeBudgetExceeded {
		t.Fatalf("budget bypassed: %+v", reject)
	}
	session.Close(&proxy.Failure{Status: reject.Status, Code: reject.Code})
	list := records(t, s, store)
	if len(list) != 1 || list[0].RequestCount != 1 || list[0].TerminalErrorCode != "budget_exceeded" {
		t.Fatalf("records %+v", list)
	}
	if n := len(eventsOf(t, store, events.Blocked)); n != 0 {
		t.Fatalf("late block %d", n)
	}
}
