package panel

import (
	"context"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/limits"
)

func TestAppShowsEveryLimit(t *testing.T) {
	env := newEnv(t)
	renew := time.Date(2026, 11, 19, 3, 0, 0, 0, time.UTC)
	pending := renew.Add(time.Hour)
	env.limits.state = limits.State{ActiveTimezone: "America/Sao_Paulo", PendingTimezone: "UTC", PendingAt: &pending, Limits: []limits.LimitView{
		{LimitName: "apps.bot.limits.requests_per_minute", Scope: limits.ScopeApp, Kind: limits.KindRate, Dimension: limits.RequestsPerMinute, Unit: limits.UnitRequests, Used: 3, Limit: 60},
		{LimitName: "apps.bot.budget.usd_per_day", Scope: limits.ScopeApp, Kind: limits.KindBudget, Dimension: limits.USDPerDay, Unit: limits.UnitMicrousd, Used: 1_970_000, Reserved: 10_000, Limit: 2_000_000, RenewsAt: &renew},
		{LimitName: "apps.bot.hard_cap.usd_per_day", Scope: limits.ScopeApp, Kind: limits.KindHardCap, Dimension: limits.USDPerDay, Unit: limits.UnitMicrousd, Used: 1_970_000, Limit: 2_400_000, RenewsAt: &renew},
		{LimitName: "apps.bot.per_user.budget.usd_per_day", Scope: limits.ScopeUser, Kind: limits.KindBudget, Dimension: limits.USDPerDay, Unit: limits.UnitMicrousd, Limit: 500_000, RenewsAt: &renew},
	}}
	d, err := env.service.App(context.Background(), "bot")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.LimitRows) != 3 || d.Policy != "downgrade" || d.DowngradeTo != "claude-b" || d.PendingTimezone != "UTC" || d.PendingAt == nil {
		t.Fatalf("detail: %+v", d)
	}
	rate, budget := d.LimitRows[0], d.LimitRows[1]
	if rate.RenewsAt != nil || rate.Used != "3" || rate.Limit != "60" {
		t.Fatalf("a rate limit invented a renewal: %+v", rate)
	}
	if budget.Used != "$1.970000" || budget.Reserved != "$0.010000" || budget.Limit != "$2.000000" || budget.RenewsAt == nil || budget.Pressure.Percent != 99 {
		t.Fatalf("budget row: %+v", budget)
	}
	if _, err := env.service.App(context.Background(), "missing"); err != ErrNotFound {
		t.Fatalf("missing app: %v", err)
	}
	apps, _ := env.service.Apps(context.Background())
	if apps[0].PressureName != "apps.bot.budget.usd_per_day" || apps[0].Pressure.Percent != 99 || apps[0].PrimaryModel != "claude-a" {
		t.Fatalf("apps list pressure: %+v", apps[0])
	}
}

func TestTopUsersStable(t *testing.T) {
	env := newEnv(t)
	at := time.Date(2026, 11, 18, 15, 0, 0, 0, time.UTC)
	users := []string{"u-1", "u-2", "u-3", "u-4", "u-5", "u-6", "u-7", "u-8", "u-9", "u-10", "u-11", "u-12"}
	for i, user := range users {
		cost := 1_000
		if i < 2 {
			cost = 9_000
		}
		env.commit(t, env.record(t, i+1, "bot", user, at.Add(time.Duration(i)*time.Second), config.Microusd(cost)))
	}
	first, _ := env.service.App(context.Background(), "bot")
	second, _ := env.service.App(context.Background(), "bot")
	if len(first.TopUsers) != 10 || first.TopUsers[0].Cost != 9_000 || first.TopUsers[1].Cost != 9_000 {
		t.Fatalf("top users: %+v", first.TopUsers)
	}
	for i := range first.TopUsers {
		u := first.TopUsers[i]
		if u != second.TopUsers[i] || len(u.Hash) != 10 || u.Hash[:2] != "u_" {
			t.Fatalf("ranking not stable or not pseudonymous: %+v %+v", u, second.TopUsers[i])
		}
		if i > 0 && first.TopUsers[i-1].Cost == u.Cost && first.TopUsers[i-1].Hash > u.Hash {
			t.Fatalf("tie not ordered by hash: %+v", first.TopUsers)
		}
	}
}

func TestModelMixCountsAttempts(t *testing.T) {
	env := newEnv(t)
	at := time.Date(2026, 11, 18, 15, 0, 0, 0, time.UTC)
	a := env.record(t, 1, "bot", "alice", at, 1_000)
	b := env.record(t, 2, "bot", "alice", at.Add(time.Second), 1_000)
	c := env.record(t, 3, "bot", "alice", at.Add(2*time.Second), 500)
	c.ModelUsed, c.Outcome, c.PolicyOutcome, c.PolicyLimitName, c.PolicyScope, c.PolicyReason = "claude-b", "downgraded", "downgraded", "apps.bot.budget.usd_per_day", "app", "spent"
	env.commit(t, a, b, c)
	d, _ := env.service.App(context.Background(), "bot")
	if len(d.Models) != 2 || d.Models[0].Model != "claude-a" || d.Models[0].Attempts != 2 || d.Models[0].Percent != 66 || d.Models[1].Attempts != 1 || d.Models[1].Cost != 500 {
		t.Fatalf("model mix: %+v", d.Models)
	}
}
