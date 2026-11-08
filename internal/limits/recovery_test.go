package limits

import (
	"context"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

func TestSnapshotEveryFiveSeconds(t *testing.T) {
	e := newEnv(t, dailyUSD(1000, "UTC"))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.manager.Run(ctx) }()
	saves := func() int {
		e.store.mu.Lock()
		defer e.store.mu.Unlock()
		return e.store.saves
	}
	waitFor := func(want int) {
		deadline := time.Now().Add(5 * time.Second)
		for saves() < want {
			if time.Now().After(deadline) {
				t.Fatalf("checkpoint %d not written", want)
			}
			time.Sleep(time.Millisecond)
		}
	}
	// Give Run time to arm its timer before the clock moves.
	for {
		e.clock.mu.Lock()
		armed := len(e.clock.timers)
		e.clock.mu.Unlock()
		if armed > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	e.clock.Advance(4999 * time.Millisecond)
	if saves() != 0 {
		t.Fatal("checkpoint before 5 seconds")
	}
	e.clock.Advance(time.Millisecond)
	waitFor(1)
	e.clock.Advance(5 * time.Second)
	waitFor(2)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if saves() != 3 {
		t.Fatalf("shutdown checkpoint missing: %d", saves())
	}
}

func TestReplayOnlyAfterCoveredSequence(t *testing.T) {
	e := newEnv(t, dailyUSD(1000, "UTC"))
	u := user("bot", 1)
	e.settle(e.mustReserve(e.request(u, 1, 100)), 100, 1)
	if err := e.manager.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	e.settle(e.mustReserve(e.request(u, 1, 200)), 150, 1)
	e.mustReserve(e.request(u, 1, 300))
	e.restart()
	v := e.view(nil, "apps.bot.budget.usd_per_day")
	// 100 from the checkpoint, 150 replayed after it, and the in-flight 300 the
	// crash left open, settled at its reserved value.
	if v.Used != 550 || v.Reserved != 0 {
		t.Fatalf("replay: %+v", v)
	}
	e.restart()
	if again := e.view(nil, "apps.bot.budget.usd_per_day"); again.Used != v.Used || again.Reserved != v.Reserved {
		t.Fatalf("a second recovery changed totals: %+v", again)
	}
}

func TestExpiredRateReplay(t *testing.T) {
	e := newEnv(t, rpmPolicy(2, 0))
	u := user("bot", 1)
	e.settle(e.mustReserve(e.request(u, 1, 0)), 0, 1)
	e.clock.Advance(50 * time.Second)
	e.settle(e.mustReserve(e.request(u, 1, 0)), 0, 1)
	e.clock.Advance(5 * time.Second)
	e.restart()
	e.mustReject(e.request(u, 1, 0))
	e.clock.Advance(5 * time.Second)
	// The first event is 60 seconds old by the civil clock and no longer counts.
	e.mustReserve(e.request(u, 1, 0))
	if c := e.view(nil, "apps.bot.limits.requests_per_minute"); c.Used != 2 {
		t.Fatalf("rebuilt window: %+v", c)
	}
}

func TestMissingUsageReservesTokensConservatively(t *testing.T) {
	p := Policy{AppSet: Set{Budget: config.Budget{TokensPerDay: 1000, USDPerDay: 1000}}}
	day := start.Format("2006-01-02")
	for _, tc := range []struct {
		name     string
		evidence int64
		want     int64
		unknown  bool
	}{
		{"proven", 2, 300 + 400, false},
		{"retention-removed-detail", 1, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, p)
			e.store.daily = []storage.Daily{{Day: day, Timezone: "UTC", AppName: "bot", Scope: "app", Input: 200, Output: 100, UsageMissing: 2, Cost: 500}}
			e.store.missing = []storage.MissingUsage{{Day: day, Timezone: "UTC", AppName: "bot", Scope: "app", Count: tc.evidence, EstimatedTokens: 400}}
			e.restart()
			v := e.view(nil, "apps.bot.budget.tokens_per_day")
			if v.Unknown != tc.unknown || (!tc.unknown && v.Used != tc.want) {
				t.Fatalf("seeded tokens: %+v", v)
			}
			if usd := e.view(nil, "apps.bot.budget.usd_per_day"); usd.Used != 500 {
				t.Fatalf("seeded cost: %+v", usd)
			}
			u := user("bot", 1)
			if tc.unknown {
				d := e.mustReject(e.request(u, 1, 1))
				if d.Primary.Dimension != TokensPerDay || d.Primary.Reason != ReasonSpent {
					t.Fatalf("unproven history must block: %+v", d.Primary)
				}
				e.clock.Advance(12 * time.Hour)
				e.mustReserve(e.request(u, 1, 1))
				return
			}
			// An open reservation without a result settles at its full estimate.
			e.mustReserve(e.request(u, 250, 1))
			e.restart()
			if v := e.view(nil, "apps.bot.budget.tokens_per_day"); v.Used != tc.want+250 {
				t.Fatalf("missing usage did not reserve conservatively: %+v", v)
			}
			// The bootstrap import does not happen twice.
			e.restart()
			if v := e.view(nil, "apps.bot.budget.tokens_per_day"); v.Used != tc.want+250 {
				t.Fatalf("history imported twice: %+v", v)
			}
		})
	}
}

func TestDegradedFailModes(t *testing.T) {
	for _, mode := range []config.FailMode{config.FailClosed, config.FailOpen} {
		t.Run(string(mode), func(t *testing.T) {
			e := newEnv(t, Policy{FailMode: mode, AppSet: Set{Budget: config.Budget{USDPerDay: 100}}})
			publisher := &recordingPublisher{}
			e.manager.publisher = publisher
			e.store.set(func(s *memStore) { s.failSave = true })
			if err := e.manager.Tick(t.Context()); err == nil {
				t.Fatal("checkpoint failure not reported")
			}
			u := user("bot", 1)
			h, d, err := e.reserve(e.request(u, 1, 10))
			if mode == config.FailClosed {
				if err != ErrUnavailable || h != nil {
					t.Fatalf("fail_closed admitted: %v", err)
				}
			} else if err != nil || !d.Allowed || !d.Degraded || !h.Degraded() {
				t.Fatalf("fail_open refused: %+v %v", d, err)
			}
			// Limits keep being enforced from memory even while degraded.
			if mode == config.FailOpen {
				e.mustReject(e.request(u, 1, 95))
			}
			e.store.set(func(s *memStore) { s.failSave = false })
			if err := e.manager.Tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			kinds := publisher.types()
			if len(kinds) < 3 || kinds[0] != "limit_state_unavailable" || kinds[len(kinds)-1] != "limit_state_recovered" {
				t.Fatalf("events: %v", kinds)
			}
			if e.manager.Degraded() {
				t.Fatal("still degraded after a successful checkpoint")
			}
		})
	}
}
