package alerts

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

const defaults = "  thresholds: [50, 80, 100]\n"

func thresholds(payloads []Payload, scope string) []int {
	var out []int
	for _, p := range payloads {
		if p.Scope == scope {
			out = append(out, p.ThresholdPercent)
		}
	}
	slices.Sort(out)
	return out
}

func TestBudgetThresholdsSkippedAndDeduplicated(t *testing.T) {
	e := newEnv(t, defaults, nil)
	e.spend(userB, 490_000)
	e.spend(userA, 490_000)
	e.process()
	if got := thresholds(e.byTrigger(TriggerBudget), "app"); len(got) != 0 {
		t.Fatalf("49%% alerted: %+v", got)
	}
	e.spend("cccccccccccccccccccccccccccccccc", 520_000)
	e.spend("eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", 520_000)
	e.process()
	got := e.byTrigger(TriggerBudget)
	if s := thresholds(got, "app"); !slices.Equal(s, []int{50, 80, 100}) {
		t.Fatalf("app thresholds %v after a jump to 101%%", s)
	}
	for _, p := range got {
		if p.Scope == "app" && (p.Used == nil || *p.Used != 2_020_000 || *p.Limit != 2_000_000 || p.Unit != "microusd" || p.RenewsAt != "2026-11-24T00:00:00Z" || p.WindowStart != "2026-11-23T00:00:00Z") {
			t.Fatalf("payload %+v", p)
		}
	}
	if err := e.service.EvaluateBudgets(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := thresholds(e.byTrigger(TriggerBudget), "app"); len(s) != 3 {
		t.Fatalf("sweep repeated thresholds: %v", s)
	}
}

func TestBudgetWindowsAndUserScope(t *testing.T) {
	e := newEnv(t, "  thresholds: [80]\n", nil)
	e.spend(userA, 850_000)
	e.spend(userB, 100_000)
	e.process()
	users := e.byTrigger(TriggerBudget)
	if len(users) != 1 || users[0].Scope != "user" || users[0].UserHash != userA || users[0].ThresholdPercent != 80 {
		t.Fatalf("user alerts %+v", users)
	}
	// The next day is a new window: the same threshold alerts again.
	e.at(day.Add(36 * time.Hour))
	e.spend(userA, 900_000)
	e.process()
	var windows []string
	for _, p := range e.byTrigger(TriggerBudget) {
		if p.Scope == "user" {
			windows = append(windows, p.WindowStart)
		}
	}
	slices.Sort(windows)
	if !slices.Equal(windows, []string{"2026-11-23T00:00:00Z", "2026-11-24T00:00:00Z"}) {
		t.Fatalf("user windows %v", windows)
	}
}

func blockedRecord(e *env, n int, user string) storage.Record {
	now := e.clock.Now()
	renew := day.AddDate(0, 0, 1)
	status := 429
	return storage.Record{RequestID: fmt.Sprintf("%032x", 1000+n), AttemptIndex: 0, StartedAt: now, FinishedAt: now, AppName: "bot", UserHash: user,
		Operation: "generation", Outcome: "blocked", ErrorClass: "rate_limit", ErrorCode: "hard_cap_exceeded", BlockReason: "spent", DownstreamStatus: &status,
		BlockLimitName: "apps.bot.hard_cap.usd_per_day", BlockScope: "app", BlockRenewsAt: &renew, Day: now.Format("2006-01-02"), Timezone: "UTC"}
}

func TestHardCapFromBlockedRecord(t *testing.T) {
	e := newEnv(t, defaults, nil)
	e.spend(userA, 900_000)
	ctx := context.Background()
	for n := range 3 {
		if err := e.store.Commit(ctx, []storage.Record{blockedRecord(e, n, userA)}); err != nil {
			t.Fatal(err)
		}
	}
	other := blockedRecord(e, 9, userA)
	other.ErrorCode, other.BlockLimitName = "budget_exceeded", "apps.bot.budget.usd_per_day"
	if err := e.store.Commit(ctx, []storage.Record{other}); err != nil {
		t.Fatal(err)
	}
	e.process()
	got := e.byTrigger(TriggerHardCap)
	if len(got) != 1 {
		t.Fatalf("hard cap occurrences %+v", got)
	}
	p := got[0]
	if p.Dimension != "usd_per_day" || p.Scope != "app" || p.UserHash != "" || *p.Limit != 3_000_000 || *p.Used != 900_000 || p.RenewsAt != "2026-11-24T00:00:00Z" {
		t.Fatalf("hard cap payload %+v", p)
	}
}

func publish(t *testing.T, e *env, ev events.Event) {
	t.Helper()
	if err := e.store.Publish(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
}

func loopEvent(at time.Time, n int) events.Event {
	return events.NewLoopCut(fmt.Sprintf("%032x", 5000+n), at, fmt.Sprintf("%032x", 6000+n), "bot", userA, false, events.ReasonSimilarRequests, 10, 30*time.Second, time.Minute)
}

func TestLoopCutDeduplicatesPerAppQuarter(t *testing.T) {
	e := newEnv(t, defaults, nil)
	base := day.Add(12 * time.Hour)
	publish(t, e, loopEvent(base.Add(time.Minute), 1))
	publish(t, e, loopEvent(base.Add(14*time.Minute), 2))
	publish(t, e, loopEvent(base.Add(15*time.Minute), 3))
	e.process()
	got := e.byTrigger(TriggerLoopCut)
	if len(got) != 2 {
		t.Fatalf("loop occurrences %d, want one per quarter", len(got))
	}
	for _, p := range got {
		if p.UserHash != userA || p.Scope != "user" || p.Count != 10 || p.App != "bot" || p.WindowStart == "" {
			t.Fatalf("loop payload %+v", p)
		}
	}
}

func reloadEvent(at time.Time, generation int64) events.Event {
	e := events.New(events.ConfigReloadFailed, at)
	e.Payload.Result, e.Payload.Generation, e.Payload.Count = "rejected", generation, 3
	return e
}

func TestFailedReloadGlobalQuarter(t *testing.T) {
	e := newEnv(t, defaults, nil)
	base := day.Add(12 * time.Hour)
	publish(t, e, reloadEvent(base, 4))
	publish(t, e, reloadEvent(base.Add(5*time.Minute), 4))
	publish(t, e, reloadEvent(base.Add(16*time.Minute), 4))
	e.process()
	got := e.byTrigger(TriggerReloadFailed)
	if len(got) != 2 || got[0].Scope != "global" || got[0].App != "" || got[0].Generation != 4 || got[0].Count != 3 {
		t.Fatalf("reload occurrences %+v", got)
	}
}

func TestFailOpenOnlyForConfiguredApps(t *testing.T) {
	e := newEnv(t, defaults, nil)
	at := day.Add(12 * time.Hour)
	global := events.New(events.LimitStateUnavailable, at)
	global.Payload.Severity, global.Payload.Reason = "warning", "spent"
	publish(t, e, global)
	for _, app := range []string{"bot", "strict"} {
		ev := events.New(events.LimitStateUnavailable, at)
		ev.App, ev.Payload.Severity, ev.Payload.DegradedSeconds = app, "warning", 30
		publish(t, e, ev)
	}
	e.process()
	got := e.byTrigger(TriggerFailOpen)
	if len(got) != 1 || got[0].App != "bot" || got[0].WindowStart != "2026-11-23T11:59:30Z" {
		t.Fatalf("fail open occurrences %+v", got)
	}
}

func fallbackEvent(at time.Time, app string, n int) events.Event {
	return events.NewFallback(at, fmt.Sprintf("%032x", 7000+n), app, 2, "fake-claude", "fake-claude", "529")
}

func TestFallbackBurstRollingWindow(t *testing.T) {
	e := newEnv(t, defaults, nil)
	base := day.Add(12 * time.Hour)
	for n := range 4 {
		publish(t, e, fallbackEvent(base.Add(time.Duration(n)*time.Minute), "bot", n))
	}
	e.process()
	if got := e.byTrigger(TriggerFallback); len(got) != 0 {
		t.Fatal("burst before the fifth fallback")
	}
	publish(t, e, fallbackEvent(base.Add(4*time.Minute+59*time.Second), "bot", 4))
	publish(t, e, fallbackEvent(base.Add(5*time.Minute), "bot", 5))
	e.process()
	got := e.byTrigger(TriggerFallback)
	if len(got) != 1 || got[0].Count < 5 || got[0].App != "bot" {
		t.Fatalf("burst occurrences %+v", got)
	}
}

func TestFallbackBelowThreshold(t *testing.T) {
	e := newEnv(t, defaults, nil)
	base := day.Add(12 * time.Hour)
	publish(t, e, fallbackEvent(base, "bot", 0))
	for n := 1; n <= 4; n++ {
		publish(t, e, fallbackEvent(base.Add(time.Duration(n)*time.Minute+30*time.Second+time.Minute), "bot", n))
	}
	for n := range 3 {
		publish(t, e, fallbackEvent(base.Add(time.Duration(n)*time.Second), "strict", 10+n))
	}
	e.process()
	if got := e.byTrigger(TriggerFallback); len(got) != 0 {
		t.Fatalf("burst outside the five minute window or across apps: %+v", got)
	}
}

func TestConfigEventEvaluatesCurrentBudgets(t *testing.T) {
	e := newEnv(t, "  thresholds: [90]\n", nil)
	e.spend(userA, 850_000)
	e.spend(userB, 850_000)
	e.process()
	if got := e.byTrigger(TriggerBudget); len(got) != 0 {
		t.Fatal("85% alerted at a 90% threshold")
	}
	ev := events.New(events.ConfigChanged, e.clock.Now())
	ev.Payload.Result, ev.Payload.Generation = "applied", 2
	e.reload("  thresholds: [80]\n")
	publish(t, e, ev)
	e.process()
	if got := e.byTrigger(TriggerBudget); len(got) != 1 || got[0].ThresholdPercent != 80 || got[0].Scope != "app" {
		t.Fatalf("new threshold not evaluated on reload: %+v", got)
	}
}

func TestAbandonedInputIsReprocessedOnce(t *testing.T) {
	e := newEnv(t, defaults, nil)
	publish(t, e, loopEvent(e.clock.Now(), 1))
	ctx := context.Background()
	// A worker claims the input and dies before committing.
	claimed, err := e.store.ClaimAlertInputs(ctx, e.clock.Now(), ClaimLease, 10)
	if err != nil || len(claimed) != 1 {
		t.Fatal(claimed, err)
	}
	e.process()
	if got := e.byTrigger(TriggerLoopCut); len(got) != 0 {
		t.Fatal("input taken before its lease ended")
	}
	e.at(e.clock.Now().Add(ClaimLease + time.Second))
	e.process()
	e.process()
	if got := e.byTrigger(TriggerLoopCut); len(got) != 1 {
		t.Fatalf("occurrences %d after takeover", len(got))
	}
	if err := e.store.CompleteAlertInput(ctx, claimed[0].EventID, claimed[0].ClaimToken, nil, nil, e.clock.Now()); err == nil {
		t.Fatal("the dead worker's token still finished the input")
	}
}
