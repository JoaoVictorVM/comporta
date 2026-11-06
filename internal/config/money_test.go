package config

import (
	"math"
	"testing"
)

func TestMoneyExactMicrousd(t *testing.T) {
	cases := map[string]Microusd{
		"0":        0,
		"0.1":      100_000,
		"0.000001": 1,
		"2.00":     2_000_000,
		"2.40":     2_400_000,
		// 0.29 and 1.1 have no exact binary form, so a float path would drift.
		"0.29":                 290_000,
		"1.1":                  1_100_000,
		"9223372036854.775807": math.MaxInt64,
	}
	for lexeme, want := range cases {
		got, err := ParseMoney(lexeme)
		if err != nil || got != want {
			t.Errorf("ParseMoney(%q) = %d, %v; want %d", lexeme, got, err, want)
		}
	}
	if got := Microusd(2_000_000).String(); got != "2.000000" {
		t.Errorf("String: %s", got)
	}
}

func TestMoneyRejectsPrecisionAndOverflow(t *testing.T) {
	for _, lexeme := range []string{
		"0.0000001", "1e3", "1_000", "1,50", "-1", "+1", ".5", "1.", "007", "0x10",
		"NaN", ".inf", "", " 1", "9223372036854.775808", "99999999999999999999",
	} {
		if value, err := ParseMoney(lexeme); err == nil {
			t.Errorf("ParseMoney(%q) = %d, want an error", lexeme, value)
		}
	}
	requireDiagnostic(t, replaced(t, baseYAML, "usd_per_day: 2.00", "usd_per_day: 0.0000001"), CodeInvalidValue, "apps.bot.budget.usd_per_day")
	requireDiagnostic(t, replaced(t, baseYAML, "usd_per_day: 2.00", "usd_per_day: \"2.00\""), CodeInvalidValue, "apps.bot.budget.usd_per_day")
	requireDiagnostic(t, replaced(t, baseYAML, "usd_per_day: 2.00", "usd_per_day: 1e3"), CodeInvalidValue, "apps.bot.budget.usd_per_day")
}

func TestHardCapsPerDimension(t *testing.T) {
	app := func(lines string) string {
		return replaced(t, baseYAML, "    budget: {usd_per_day: 2.00}\n", lines)
	}
	requireValid(t, app("    budget: {usd_per_day: 2.00}\n    hard_cap: {usd_per_day: 2.00}\n"))
	requireDiagnostic(t, app("    budget: {usd_per_day: 2.00}\n    hard_cap: {usd_per_day: 1.99}\n"), CodeInvalidValue, "apps.bot.hard_cap.usd_per_day")
	requireDiagnostic(t, app("    budget: {usd_per_day: 2.00}\n    hard_cap: {usd_per_month: 5}\n"), CodeInvalidValue, "apps.bot.hard_cap.usd_per_month")
	requireDiagnostic(t, app("    budget: {tokens_per_day: 100}\n    hard_cap: {tokens_per_day: 99}\n"), CodeInvalidValue, "apps.bot.hard_cap.tokens_per_day")
	requireDiagnostic(t, app("    per_user: {budget: {tokens_per_month: 100}, hard_cap: {tokens_per_month: 99}}\n"), CodeInvalidValue, "apps.bot.per_user.hard_cap.tokens_per_month")
	requireDiagnostic(t, app("    per_user: {hard_cap: {tokens_per_month: 99}}\n"), CodeInvalidValue, "apps.bot.per_user.hard_cap.tokens_per_month")

	warn := requireValid(t, app("    on_limit: warn\n    budget: {usd_per_day: 2.00}\n")).cfg.Apps["bot"]
	if warn.HardCap.USDPerDay != 0 {
		t.Fatalf("warn without a hard cap must stay uncapped: %+v", warn.HardCap)
	}
}

func TestDowngradeDefaultRoundsUp(t *testing.T) {
	text := replaced(t, baseYAML, "    budget: {usd_per_day: 2.00}\n", `    on_limit: downgrade
    downgrade_to: model-b
    budget: {usd_per_day: 0.000001, usd_per_month: 2.00, tokens_per_day: 101}
    hard_cap: {usd_per_month: 2.10}
    per_user:
      budget: {tokens_per_month: 7}
`)
	app := requireValid(t, text).cfg.Apps["bot"]
	if app.HardCap.USDPerDay != 2 {
		t.Errorf("1 microdollar at 120%% rounds up to 2, got %d", app.HardCap.USDPerDay)
	}
	if app.HardCap.USDPerMonth != 2_100_000 {
		t.Errorf("an explicit cap must be kept, got %d", app.HardCap.USDPerMonth)
	}
	if app.HardCap.TokensPerDay != 122 {
		t.Errorf("101 tokens at 120%% rounds up to 122, got %d", app.HardCap.TokensPerDay)
	}
	if app.PerUser.HardCap.TokensPerMonth != 9 {
		t.Errorf("per-user cap must be derived too, got %d", app.PerUser.HardCap.TokensPerMonth)
	}
	if app.HardCap.TokensPerMonth != 0 {
		t.Errorf("a dimension without a budget must stay absent, got %d", app.HardCap.TokensPerMonth)
	}

	overflow := replaced(t, baseYAML, "    budget: {usd_per_day: 2.00}\n", `    on_limit: downgrade
    downgrade_to: model-b
    budget: {tokens_per_day: 9223372036854775807}
`)
	requireDiagnostic(t, overflow, CodeInvalidValue, "apps.bot.hard_cap.tokens_per_day")
}
