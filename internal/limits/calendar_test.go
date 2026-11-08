package limits

import (
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
)

func dailyUSD(limit config.Microusd, tz string) Policy {
	return Policy{Timezone: tz, AppSet: Set{Budget: config.Budget{USDPerDay: limit}}}
}

func TestMidnightMonthEndLeapDay(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tz       string
		midnight time.Time
	}{
		{"month-end", "America/Sao_Paulo", time.Date(2026, 12, 1, 3, 0, 0, 0, time.UTC)},
		{"year-end", "America/Sao_Paulo", time.Date(2027, 1, 1, 3, 0, 0, 0, time.UTC)},
		{"leap-day-in", "Asia/Tokyo", time.Date(2028, 2, 28, 15, 0, 0, 0, time.UTC)},
		{"leap-day-out", "Asia/Tokyo", time.Date(2028, 2, 29, 15, 0, 0, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, dailyUSD(100, tc.tz))
			e.clock.SetCivil(tc.midnight.Add(-2 * time.Second))
			u := user("bot", 1)
			crossing := e.mustReserve(e.request(u, 1, 60))
			e.settle(e.mustReserve(e.request(u, 1, 40)), 40, 1)
			d := e.mustReject(e.request(u, 1, 1))
			if !d.Primary.RenewsAt.Equal(tc.midnight) {
				t.Fatalf("renews at %v, want %v", d.Primary.RenewsAt, tc.midnight)
			}
			e.clock.Advance(time.Second)
			e.mustReject(e.request(u, 1, 1))
			e.clock.Advance(time.Second)
			// The handle crossing midnight still settles into the old day.
			e.settle(crossing, 60, 1)
			e.mustReserve(e.request(u, 1, 100))
		})
	}
}

func TestMonthlyResetFirstDay(t *testing.T) {
	e := newEnv(t, Policy{Timezone: "Europe/Lisbon", AppSet: Set{Budget: config.Budget{TokensPerMonth: 100}}})
	boundary := time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)
	e.clock.SetCivil(boundary.Add(-time.Minute))
	u := user("bot", 1)
	e.settle(e.mustReserve(e.request(u, 100, 0)), 0, 100)
	d := e.mustReject(e.request(u, 1, 0))
	if !d.Primary.RenewsAt.Equal(boundary) || d.Primary.Dimension != TokensPerMonth {
		t.Fatalf("monthly renewal: %+v", d.Primary)
	}
	e.clock.Advance(time.Minute)
	e.mustReserve(e.request(u, 100, 0))
}

func TestDSTDaysAndMidnightTransition(t *testing.T) {
	newYork, _ := time.LoadLocation("America/New_York")
	for _, tc := range []struct {
		day  time.Time
		want time.Duration
	}{
		{time.Date(2026, 3, 8, 12, 0, 0, 0, newYork), 23 * time.Hour},
		{time.Date(2026, 11, 1, 12, 0, 0, 0, newYork), 25 * time.Hour},
		{time.Date(2026, 7, 1, 12, 0, 0, 0, newYork), 24 * time.Hour},
	} {
		w := calendarWindow(tc.day, false, newYork)
		if w.end.Sub(w.start) != tc.want || !w.start.In(newYork).Equal(time.Date(tc.day.Year(), tc.day.Month(), tc.day.Day(), 0, 0, 0, 0, newYork)) {
			t.Fatalf("%v: window %v..%v", tc.day, w.start, w.end)
		}
	}
	// Cuba moved clocks forward at midnight in 2013: 00:00 did not exist, and the
	// day started at 01:00 local time.
	havana, _ := time.LoadLocation("America/Havana")
	w := calendarWindow(time.Date(2013, 3, 10, 12, 0, 0, 0, havana), false, havana)
	if got := w.start.In(havana); got.Day() != 10 || got.Hour() != 1 || got.Minute() != 0 {
		t.Fatalf("first valid instant: %v", got)
	}
	if !w.end.After(w.start) || w.end.Sub(w.start) != 23*time.Hour {
		t.Fatalf("day without midnight: %v..%v", w.start, w.end)
	}
	for at := w.start; at.Before(w.end); at = at.Add(17 * time.Minute) {
		if inner := calendarWindow(at, false, havana); inner != w {
			t.Fatalf("instant %v left its window", at)
		}
	}
}

func TestTimezoneReloadDoesNotRenewBudget(t *testing.T) {
	e := newEnv(t, dailyUSD(100, "UTC"))
	// 22:00 UTC is 07:00 of the next day in Tokyo: switching at once would open
	// a fresh Tokyo day and grant the budget twice.
	e.clock.SetCivil(time.Date(2026, 11, 8, 22, 0, 0, 0, time.UTC))
	u := user("bot", 1)
	e.settle(e.mustReserve(e.request(u, 1, 100)), 100, 1)
	e.policy.Timezone = "Asia/Tokyo"
	d := e.mustReject(e.request(u, 1, 1))
	if d.Primary.Timezone != "UTC" {
		t.Fatalf("new timezone applied before the active window ended: %+v", d.Primary)
	}
	state := e.manager.State(e.policy, nil)
	if state.ActiveTimezone != "UTC" || state.PendingTimezone != "Asia/Tokyo" || state.PendingAt == nil || !state.PendingAt.Equal(time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("pending timezone not reported: %+v", state)
	}
	if err := e.manager.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	e.restart()
	e.clock.Advance(2 * time.Hour)
	// At the cut the Tokyo day of Nov 9 already started at 15:00 UTC on Nov 8,
	// overlapping the charged UTC day, so it starts with that spend.
	d = e.mustReject(e.request(u, 1, 1))
	if d.Primary.Timezone != "Asia/Tokyo" || d.Primary.Used != 100 {
		t.Fatalf("overlapping window granted fresh: %+v", d.Primary)
	}
	e.clock.Advance(15 * time.Hour)
	e.mustReserve(e.request(u, 1, 100))
}
