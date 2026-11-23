package panel

import (
	"context"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// MaxAlerts is how many recent alerts the overview lists.
const MaxAlerts = 10

// spendRow is the aggregate of one app over its own day and month.
type spendRow struct {
	today, month               config.Microusd
	todayRequests, todayTokens int64
	incomplete                 int64
	cacheSaved, downgradeSaved config.Microusd
}

// spendByApp reads the app-scope aggregates of every app in one query and
// sums each app inside its own day and month. Totals are summed only after
// each app's window was applied.
func (s *Service) spendByApp(ctx context.Context, screen Screen, snapshot *config.Snapshot, apps []string) (map[string]spendRow, map[string]window, error) {
	now := s.now()
	windows := map[string]window{}
	from, through := "", ""
	for _, name := range apps {
		app, _ := snapshot.App(name)
		w := windowFor(now, appTimezone(snapshot, app))
		windows[name] = w
		if from == "" || w.monthStart < from {
			from = w.monthStart
		}
		if w.day > through {
			through = w.day
		}
	}
	out := map[string]spendRow{}
	if len(apps) == 0 {
		return out, windows, nil
	}
	var rows []storage.Daily
	err := s.query(ctx, screen, func(ctx context.Context) error {
		var err error
		rows, err = s.store.QueryDaily(ctx, storage.UsageSelector{Scope: "app", From: from, Through: through})
		return err
	})
	if err != nil {
		return nil, windows, err
	}
	for _, d := range rows {
		w, ok := windows[d.AppName]
		if !ok || d.Day < w.monthStart || d.Day > w.day {
			continue
		}
		row := out[d.AppName]
		row.month += d.Cost
		row.cacheSaved += d.CacheSaved
		row.downgradeSaved += d.DowngradeSaved
		if d.Day == w.day {
			row.today += d.Cost
			row.todayRequests += d.Requests
			row.todayTokens += d.Input + d.Output + d.CacheWrite + d.CacheRead
			row.incomplete += d.UsageMissing + d.Recovered
		}
		out[d.AppName] = row
	}
	return out, windows, nil
}

// Overview builds the first screen. A failed source shows as unavailable;
// the others still render.
func (s *Service) Overview(ctx context.Context) Overview {
	snapshot := s.config.Capture()
	pause := s.pause.PauseState()
	apps := configuredApps(snapshot)
	out := Overview{At: s.now().UTC(), GlobalPaused: pause.Global}
	spend, windows, err := s.spendByApp(ctx, ScreenOverview, snapshot, apps)
	if err != nil {
		out.Spend = Unavailable
	}
	for _, name := range apps {
		app, _ := snapshot.App(name)
		row := spend[name]
		w := windows[name]
		item := AppSpend{App: name, Paused: paused(pause, name), Timezone: w.loc.String(), Day: w.day,
			Today: row.today, Month: row.month, TodayRequests: row.todayRequests, Incomplete: row.incomplete,
			DayBudget: app.Budget.USDPerDay, MonthBudget: app.Budget.USDPerMonth}
		known := out.Spend == Available
		item.DayPressure = PressureOf(int64(row.today), 0, int64(app.Budget.USDPerDay), !known)
		item.MonthPressure = PressureOf(int64(row.month), 0, int64(app.Budget.USDPerMonth), !known)
		// A budget whose history could not be proven never shows as within it.
		if state, availability := s.limitState(snapshot, name, nil); availability == Available {
			for _, v := range state.Limits {
				if v.Unknown && v.LimitName == "apps."+name+".budget.usd_per_day" {
					item.DayPressure = Pressure{}
				}
				if v.Unknown && v.LimitName == "apps."+name+".budget.usd_per_month" {
					item.MonthPressure = Pressure{}
				}
			}
		}
		out.Apps = append(out.Apps, item)
		out.TodayCost += row.today
		out.MonthCost += row.month
		out.TodayRequests += row.todayRequests
		out.TodayTokens += row.todayTokens
		out.Incomplete += row.incomplete
		out.CacheSaved += row.cacheSaved
		out.DowngradeSaved += row.downgradeSaved
	}
	out.Alerts = s.recentAlerts(ctx)
	return out
}

func (s *Service) recentAlerts(ctx context.Context) AlertsView {
	if s.alerts == nil {
		return AlertsView{}
	}
	var items []Alert
	err := s.query(ctx, ScreenOverview, func(ctx context.Context) error {
		var err error
		items, err = s.alerts.RecentAlerts(ctx, MaxAlerts)
		return err
	})
	if err != nil {
		return AlertsView{Enabled: true, Failed: true}
	}
	if len(items) > MaxAlerts {
		items = items[:MaxAlerts]
	}
	return AlertsView{Enabled: true, Items: items}
}
