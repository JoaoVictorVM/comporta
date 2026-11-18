//go:build distributed

package distributed

import (
	"context"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/limits"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

func TestDowngradeReservesSelectedModel(t *testing.T) {
	c := newCluster(t, 2)
	p := policy(1_000, config.FailClosed)
	p.OnLimit, p.DowngradeTo = config.OnLimitDowngrade, "cheap"
	p.AppSet.HardCap = config.Budget{USDPerDay: 1_200}
	// Spend 900 on the first replica, so the original no longer fits the budget.
	a, rec, _, err := c.replicas[0].admit(t, p, 900)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.replicas[0].coord.SettleAttempt(context.Background(), a, limits.Usage{Cost: 900, Complete: true}, c.replicas[0].persistFor(rec)); err != nil {
		t.Fatal(err)
	}
	id := newRequestID()
	options := []limits.Candidate{
		{ID: limits.CandidateOriginal, Model: "m", EstimatedTokens: 10, ReservedCost: 200},
		{ID: limits.CandidateDowngrade, Model: "cheap", EstimatedTokens: 10, ReservedCost: 50},
	}
	r := c.replicas[1]
	var reserved storage.Reservation
	commit := func(ctx context.Context, res storage.Reservation, sel limits.Selection) (int64, error) {
		reserved = res
		out := record(id, 1, 50)
		out.ModelUsed, out.PolicyOutcome, out.Outcome = "cheap", "downgraded", "downgraded"
		out.PolicyLimitName, out.PolicyScope, out.PolicyReason = sel.Trigger.LimitName, string(sel.Trigger.Scope), string(sel.Trigger.Reason)
		return r.sql.AdmitReserved(ctx, out, res)
	}
	_, d, sel, err := r.coord.ReserveAttempt(context.Background(), p, request(id), options, commit)
	if err != nil || !d.Allowed || sel.Outcome != limits.PolicyDowngraded || sel.Candidate != limits.CandidateDowngrade {
		t.Fatalf("expected a downgrade on the other replica: %+v %+v %v", d, sel, err)
	}
	if reserved.Quote.ReservedCost != 50 || reserved.Quote.Model != "cheap" {
		t.Fatalf("the reservation must freeze the selected model and its cost: %+v", reserved.Quote)
	}
	// The hard cap is global too: 900 spent + 50 held + 300 would pass it.
	_, d, _, err = c.replicas[0].coord.ReserveAttempt(context.Background(), p, request(newRequestID()),
		[]limits.Candidate{{ID: limits.CandidateOriginal, Model: "m", EstimatedTokens: 10, ReservedCost: 300}, {ID: limits.CandidateDowngrade, Model: "cheap", EstimatedTokens: 10, ReservedCost: 300}},
		c.replicas[0].commitFor(record(newRequestID(), 1, 300)))
	if err != nil || d.Allowed || d.Primary == nil || !d.Primary.HardCap() {
		t.Fatalf("hard cap must block across replicas: %+v %v", d.Primary, err)
	}
}

func TestLateAttemptSettlementIdempotent(t *testing.T) {
	c := newCluster(t, 1)
	r := c.replicas[0]
	a, rec, _, err := r.admit(t, policy(1_000_000, config.FailClosed), 100)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := r.coord.SettleAttempt(context.Background(), a, limits.Usage{Cost: 80, Complete: true}, r.persistFor(rec)); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := r.coord.ProjectOutbox(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if s, res := spent(t, r); s != 80 || res != 0 {
		t.Fatalf("a repeated ending must be counted once: spent %d reserved %d", s, res)
	}
}

func TestCacheHitReleasesSharedReservation(t *testing.T) {
	c := newCluster(t, 2)
	p := policy(1_000, config.FailClosed)
	a, rec, _, err := c.replicas[0].admit(t, p, 600)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, d, _ := c.replicas[1].admit(t, p, 600); d.Allowed {
		t.Fatal("the reservation must be visible on the other replica")
	}
	released := rec
	released.Outcome = "cached"
	if err := c.replicas[0].coord.ReleaseAttempt(context.Background(), a, limits.ReleaseCache, func(ctx context.Context, st storage.Settlement) (int64, error) {
		return c.replicas[0].sql.SettleReservation(ctx, st)
	}); err != nil {
		t.Fatal(err)
	}
	if s, res := spent(t, c.replicas[1]); s != 0 || res != 0 {
		t.Fatalf("a cache hit costs nothing and frees the reservation: spent %d reserved %d", s, res)
	}
	if _, _, d, _ := c.replicas[1].admit(t, p, 600); !d.Allowed {
		t.Fatal("the released budget must be available on the other replica")
	}
}
