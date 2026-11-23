package panel

import (
	"context"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/limits"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// Apps lists the configured apps with the pressure of their tightest limit.
// Spend comes from one query; limit state is read from memory per app.
func (s *Service) Apps(ctx context.Context) ([]AppSummary, Availability) {
	snapshot := s.config.Capture()
	pause := s.pause.PauseState()
	apps := configuredApps(snapshot)
	spend, _, err := s.spendByApp(ctx, ScreenApps, snapshot, apps)
	availability := Available
	if err != nil {
		availability = Unavailable
	}
	var out []AppSummary
	for _, name := range apps {
		app, _ := snapshot.App(name)
		row := AppSummary{App: name, Paused: paused(pause, name), Policy: policyName(app.OnLimit), Today: spend[name].today, Month: spend[name].month}
		if len(app.AllowedModels) > 0 {
			row.PrimaryModel = app.AllowedModels[0]
		}
		state, limitsAvailable := s.limitState(snapshot, name, nil)
		row.Limits = limitsAvailable
		if limitsAvailable == Available {
			for _, v := range state.Limits {
				p := PressureOf(v.Used, v.Reserved, v.Limit, v.Unknown)
				if !p.Known {
					row.Pressure, row.PressureName = p, v.LimitName
					break
				}
				if p.Percent >= row.Pressure.Percent {
					row.Pressure, row.PressureName = p, v.LimitName
				}
			}
			if row.PressureName == "" {
				row.Pressure = Pressure{Known: true}
			}
		}
		out = append(out, row)
	}
	return out, availability
}

func policyName(p config.OnLimit) string {
	if p == "" {
		return string(config.OnLimitBlock)
	}
	return string(p)
}

// App builds the screen of one configured app. The period of the model mix
// and the top users is the app's current month.
func (s *Service) App(ctx context.Context, name string) (AppDetail, error) {
	snapshot := s.config.Capture()
	app, ok := snapshot.App(name)
	if !ok {
		return AppDetail{}, ErrNotFound
	}
	pause := s.pause.PauseState()
	out := AppDetail{App: name, Paused: appPaused(pause, name), GlobalPaused: pause.Global, Policy: policyName(app.OnLimit), DowngradeTo: app.DowngradeTo,
		FailMode: string(app.FailMode), Timezone: appTimezone(snapshot, app)}
	state, availability := s.limitState(snapshot, name, nil)
	out.Limits = availability
	if s.limits != nil {
		out.ActiveTimezone, out.PendingTimezone, out.PendingAt, out.Degraded = state.ActiveTimezone, state.PendingTimezone, state.PendingAt, state.Degraded
		out.LimitRows = limitRows(state.Limits, limits.ScopeApp)
	}
	spend, windows, err := s.spendByApp(ctx, ScreenApps, snapshot, []string{name})
	if err != nil {
		out.Spend = Unavailable
		return out, nil
	}
	out.Today, out.Month = spend[name].today, spend[name].month
	w := windows[name]
	out.Period = w.monthStart + " to " + w.day
	start, _ := time.ParseInLocation(time.DateOnly, w.monthStart, w.loc)
	var mix []storage.PanelModelUse
	var top []storage.PanelUserCost
	err = s.query(ctx, ScreenApps, func(ctx context.Context) error {
		var err error
		if mix, err = s.store.PanelModelMix(ctx, name, start, s.now().Add(time.Minute)); err != nil {
			return err
		}
		top, err = s.store.PanelTopUsers(ctx, name, w.monthStart, w.day)
		return err
	})
	if err != nil {
		out.Spend = Unavailable
		return out, nil
	}
	var total int64
	for _, m := range mix {
		total += m.Attempts
	}
	for _, m := range mix {
		share := ModelShare{Model: m.Model, Attempts: m.Attempts, Cost: m.Cost}
		if total > 0 {
			share.Percent = int(m.Attempts * 100 / total)
		}
		out.Models = append(out.Models, share)
	}
	for _, u := range top {
		out.TopUsers = append(out.TopUsers, UserCost{Hash: ShortHash(u.Hash), Anonymous: u.Anonymous, Cost: u.Cost, Requests: u.Requests})
	}
	return out, nil
}
