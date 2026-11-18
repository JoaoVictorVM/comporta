//go:build distributed

package distributed

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/limits"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/storage/postgres"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

// Two coordinators with separate pools share one budget: concurrent
// admissions never reserve past it, and settled spend stays within it.
func TestSharedBudgetNeverOvershoots(t *testing.T) {
	c := newCluster(t, 2)
	const cost, budget = 100, 1_000
	p := policy(budget, config.FailClosed)
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for i := range 60 {
		r := c.replicas[i%2]
		wg.Go(func() {
			// Clients retry a 503, as the SDKs do; every request ends with a
			// definitive answer, so the count of admissions is exact.
			var a limits.Attempt
			var rec storage.Record
			var d limits.Decision
			var err error
			for range 50 {
				a, rec, d, err = r.admit(t, p, cost)
				if err == nil {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if err != nil {
				t.Error(err)
				return
			}
			if !d.Allowed {
				return
			}
			admitted.Add(1)
			if err := r.coord.SettleAttempt(context.Background(), a, limits.Usage{Cost: cost, Tokens: 10, Complete: true}, r.persistFor(rec)); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	// A reply lost under load leaves its reservation held until reconciliation:
	// conservative, never an overshoot. Spend plus what is still held is the
	// whole budget, and spend is exactly what was admitted.
	s, reserved := spent(t, c.replicas[1])
	if admitted.Load() > budget/cost || s != admitted.Load()*cost || s+reserved != budget {
		t.Fatalf("admitted %d spent %d reserved %d of %d", admitted.Load(), s, reserved, budget)
	}
}

func TestSharedConcurrencyAcrossReplicas(t *testing.T) {
	c := newCluster(t, 2)
	p := policy(1_000_000, config.FailClosed)
	p.AppSet.Limits.MaxConcurrent = 3
	var held []limits.Attempt
	var recs []storage.Record
	var owners []*replica
	for i := range 5 {
		r := c.replicas[i%2]
		a, rec, d, err := r.admit(t, p, 1)
		if err != nil {
			t.Fatal(err)
		}
		if d.Allowed {
			held, recs, owners = append(held, a), append(recs, rec), append(owners, r)
		} else if d.Primary == nil || d.Primary.Kind != limits.KindConcurrency {
			t.Fatalf("unexpected refusal %+v", d.Primary)
		}
	}
	if len(held) != 3 {
		t.Fatalf("concurrency must be shared: %d admitted", len(held))
	}
	if err := owners[0].coord.SettleAttempt(context.Background(), held[0], limits.Usage{Cost: 1, Complete: true}, owners[0].persistFor(recs[0])); err != nil {
		t.Fatal(err)
	}
	if _, _, d, err := c.replicas[1].admit(t, p, 1); err != nil || !d.Allowed {
		t.Fatal("a settled attempt must free its slot on every replica")
	}
}

func TestSettlementOutboxReplays(t *testing.T) {
	c := newCluster(t, 2)
	p := policy(1_000_000, config.FailClosed)
	r := c.replicas[0]
	a, rec, _, err := r.admit(t, p, 500)
	if err != nil {
		t.Fatal(err)
	}
	resume := testkit.PauseRedis(t)
	err = r.coord.SettleAttempt(context.Background(), a, limits.Usage{Cost: 700, Tokens: 10, Complete: true}, r.persistFor(rec))
	resume()
	if err != nil {
		t.Fatalf("the durable settlement must succeed without Redis: %v", err)
	}
	var pending int
	if err := r.sql.DB().QueryRow(`SELECT count(*) FROM coordination_outbox WHERE delivered_at IS NULL`).Scan(&pending); err != nil || pending != 1 {
		t.Fatalf("the projection must wait in the outbox: %d %v", pending, err)
	}
	// The next admission of the app replays it before deciding, so spend
	// above the estimate is never invisible to a decision.
	if _, _, d, err := c.replicas[1].admit(t, p, 1); err != nil || !d.Allowed {
		t.Fatalf("admission after replay: %v", err)
	}
	s, _ := spent(t, c.replicas[1])
	if s != 700 {
		t.Fatalf("replayed settlement not projected: spent %d", s)
	}
	if err := r.sql.DB().QueryRow(`SELECT count(*) FROM coordination_outbox WHERE delivered_at IS NULL`).Scan(&pending); err != nil || pending != 0 {
		t.Fatal("outbox not acknowledged")
	}
	if err := c.replicas[1].coord.ProjectOutbox(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s2, _ := spent(t, c.replicas[0]); s2 != 700 {
		t.Fatal("a replay must not apply twice")
	}
}

func TestFailClosedWhenRedisUnavailable(t *testing.T) {
	c := newCluster(t, 1)
	resume := testkit.PauseRedis(t)
	defer resume()
	start := time.Now()
	_, _, _, err := c.replicas[0].admit(t, policy(1_000, config.FailClosed), 1)
	if !errors.Is(err, limits.ErrUnavailable) {
		t.Fatalf("fail_closed must refuse, got %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("admission exceeded its deadline")
	}
	var journal int
	if err := c.replicas[0].sql.DB().QueryRow(`SELECT count(*) FROM metering_journal`).Scan(&journal); err != nil || journal != 0 {
		t.Fatal("a refused admission left a journal entry")
	}
}

func TestFailOpenRecordedDegraded(t *testing.T) {
	c := newCluster(t, 2)
	resume := testkit.PauseRedis(t)
	a, rec, d, err := c.replicas[0].admit(t, policy(1_000, config.FailOpen), 400)
	if err != nil || !d.Allowed || !d.Degraded || !a.Degraded() || d.Budget != nil {
		resume()
		t.Fatalf("fail_open must admit degraded without budget headers: %+v %v", d, err)
	}
	// Every replica reads the mode in the gate, including one that still
	// reaches Redis, and a fail_closed app stays blocked.
	_, _, _, err = c.replicas[1].admit(t, policy(1_000, config.FailClosed), 1)
	resume()
	if !errors.Is(err, limits.ErrUnavailable) {
		t.Fatalf("fail_closed app during degradation: %v", err)
	}
	state, _ := postgres.LoadCluster(context.Background(), c.replicas[1].sql.DB(), c.name)
	if state.Mode != postgres.ModeDegraded {
		t.Fatalf("cluster mode %s", state.Mode)
	}
	if err := c.replicas[0].coord.SettleAttempt(context.Background(), a, limits.Usage{Cost: 400, Complete: true}, c.replicas[0].persistFor(rec)); err != nil {
		t.Fatal(err)
	}
	var degraded int
	if err := c.replicas[0].sql.DB().QueryRow(`SELECT count(*) FROM requests WHERE (record_json->>'LimitsDegraded')::boolean`).Scan(&degraded); err != nil {
		t.Fatal(err)
	}
	var journal string
	if err := c.replicas[0].sql.DB().QueryRow(`SELECT quote_json->>'degraded' FROM limit_reservations`).Scan(&journal); err != nil || journal != "true" {
		t.Fatal("the degraded admission must be in the ledger for the recovery to reapply it")
	}
}

func TestPauseIsCheckedInTheGate(t *testing.T) {
	c := newCluster(t, 2)
	if _, _, err := c.replicas[0].sql.SavePause(context.Background(), storage.PauseChange{App: "bot", Paused: true, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// The Redis projection was never told: the durable state still refuses.
	if _, _, _, err := c.replicas[1].admit(t, policy(1_000, config.FailClosed), 1); !errors.Is(err, limits.ErrPaused) {
		t.Fatalf("a paused app must be refused on every replica, got %v", err)
	}
}

func TestChainHoldsOneSlot(t *testing.T) {
	c := newCluster(t, 1)
	r := c.replicas[0]
	p := policy(1_000_000, config.FailClosed)
	p.AppSet.Limits.MaxConcurrent = 1
	id := newRequestID()
	first := record(id, 1, 10)
	ch, a, d, _, err := r.coord.OpenChain(context.Background(), p, request(id), candidates(10), r.commitFor(first))
	if err != nil || !d.Allowed {
		t.Fatal(err)
	}
	if _, _, d, _ := r.admit(t, p, 1); d.Allowed {
		t.Fatal("the chain holds the only slot")
	}
	if err := r.coord.ReleaseAttempt(context.Background(), a, limits.ReleaseNotSent, r.persistFor(first)); err != nil {
		t.Fatal(err)
	}
	second := record(id, 2, 10)
	second.Outcome, second.FallbackFrom, second.FallbackCause, second.ModelUsed = "fallback", "m", "429", "m2"
	next := request(id)
	next.AttemptIndex = 2
	commit := func(ctx context.Context, res storage.Reservation, sel limits.Selection) (int64, error) {
		return r.sql.AdmitContinuation(ctx, second, res, storage.Operation{RequestID: id, AppName: "bot", Subject: userHash, StartedAt: first.StartedAt})
	}
	if _, d, _, err := r.coord.ContinueChain(context.Background(), ch, next, candidates(10), commit); err != nil || !d.Allowed {
		t.Fatalf("continuation: %v", err)
	}
	if _, _, d, _ := r.admit(t, p, 1); d.Allowed {
		t.Fatal("a continuation keeps the chain's single slot")
	}
	r.coord.CloseChain(ch)
	if _, _, d, _ := r.admit(t, p, 1); !d.Allowed {
		t.Fatal("closing the chain frees its slot")
	}
}

func TestStaleRevisionStopsAdmitting(t *testing.T) {
	c := newCluster(t, 1)
	c.revision.Store(2)
	if _, _, _, err := c.replicas[0].admit(t, policy(1_000, config.FailClosed), 1); !errors.Is(err, limits.ErrStale) {
		t.Fatalf("a replica on another revision must not admit, got %v", err)
	}
}
