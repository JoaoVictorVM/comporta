package panel

import (
	"context"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/limits"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

func TestOverviewUsesEachAppTimezone(t *testing.T) {
	env := newEnv(t)
	// 01:30 UTC on the 19th is the evening of the 18th in Sao Paulo and the
	// morning of the 19th in Tokyo.
	env.commit(t,
		env.record(t, 1, "bot", "alice", time.Date(2026, 11, 18, 23, 0, 0, 0, time.UTC), 1_000),
		env.record(t, 2, "bot", "alice", time.Date(2026, 11, 19, 1, 0, 0, 0, time.UTC), 2_000),
		env.record(t, 3, "web", "bob", time.Date(2026, 11, 19, 0, 30, 0, 0, time.UTC), 4_000),
		env.record(t, 4, "web", "bob", time.Date(2026, 11, 18, 14, 0, 0, 0, time.UTC), 8_000),
	)
	o := env.service.Overview(context.Background())
	if o.Spend != Available || len(o.Apps) != 2 {
		t.Fatalf("overview: %+v", o)
	}
	bot, web := o.Apps[0], o.Apps[1]
	if bot.App != "bot" || bot.Day != "2026-11-18" || bot.Today != 3_000 || bot.Month != 3_000 || bot.TodayRequests != 2 {
		t.Fatalf("bot: %+v", bot)
	}
	if web.App != "web" || web.Day != "2026-11-19" || web.Today != 4_000 || web.Month != 12_000 {
		t.Fatalf("web: %+v", web)
	}
	if o.TodayCost != 7_000 || o.MonthCost != 15_000 {
		t.Fatalf("totals: today %d month %d", o.TodayCost, o.MonthCost)
	}
	if !bot.DayPressure.Known || bot.DayBudget != 2_000_000 || bot.DayPressure.Percent != 0 || web.DayPressure.Limit {
		t.Fatalf("pressure: %+v %+v", bot.DayPressure, web.DayPressure)
	}
	if o.Alerts.Enabled {
		t.Fatal("alerts shown without a reader")
	}
}

func TestUnknownLimitIsNotZero(t *testing.T) {
	env := newEnv(t)
	renew := env.now.Add(time.Hour)
	env.limits.state = limits.State{ActiveTimezone: "America/Sao_Paulo", Limits: []limits.LimitView{
		{LimitName: "apps.bot.budget.usd_per_day", Scope: limits.ScopeApp, Kind: limits.KindBudget, Dimension: limits.USDPerDay, Unit: limits.UnitMicrousd, Limit: 2_000_000, RenewsAt: &renew, Unknown: true},
	}}
	o := env.service.Overview(context.Background())
	if p := o.Apps[0].DayPressure; p.Known || p.Level() != "unknown" {
		t.Fatalf("an unknown budget showed as %+v", p)
	}
	detail, err := env.service.App(context.Background(), "bot")
	if err != nil || len(detail.LimitRows) != 1 || !detail.LimitRows[0].Unknown || detail.LimitRows[0].Pressure.Known {
		t.Fatalf("app detail: %+v %v", detail.LimitRows, err)
	}
	env.service.limits = nil
	detail, _ = env.service.App(context.Background(), "bot")
	if detail.Limits != NotVerifiable || len(detail.LimitRows) != 0 {
		t.Fatalf("without limit state: %+v", detail)
	}
	env.service.limits = &limitsFake{state: limits.State{Degraded: true}}
	if detail, _ = env.service.App(context.Background(), "bot"); detail.Limits != NotVerifiable {
		t.Fatal("degraded limit state shown as verified")
	}
}

func TestPauseProjection(t *testing.T) {
	env := newEnv(t)
	env.pause.set(storage.PauseState{Apps: []storage.AppPause{{App: "web", Paused: true}, {App: "removed", Paused: true}}})
	o := env.service.Overview(context.Background())
	if o.GlobalPaused || o.Apps[0].Paused || !o.Apps[1].Paused {
		t.Fatalf("app pause: %+v", o.Apps)
	}
	apps, _ := env.service.Apps(context.Background())
	if apps[0].Paused || !apps[1].Paused {
		t.Fatalf("apps list: %+v", apps)
	}
	env.pause.set(storage.PauseState{Global: true})
	o = env.service.Overview(context.Background())
	if !o.GlobalPaused || !o.Apps[0].Paused || !o.Apps[1].Paused {
		t.Fatalf("global pause: %+v", o)
	}
	detail, _ := env.service.App(context.Background(), "bot")
	if !detail.GlobalPaused || detail.Paused {
		t.Fatalf("detail keeps the app override apart from the global one: %+v", detail)
	}
}
