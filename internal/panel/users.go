package panel

import (
	"context"
	"slices"

	"github.com/JoaoVictorVM/comporta/internal/limits"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/useridentity"
)

// UserQuery is one search. ID is the raw identifier as typed; it lives only
// in memory for the duration of the search and is never returned, logged or
// put in a URL. Anonymous selects the anonymous bucket of App instead.
type UserQuery struct {
	ID        string
	App       string
	Anonymous bool
}

type subject struct {
	app       string
	hash      string
	anonymous bool
	removed   bool
}

// SearchUser computes the app-scoped pseudonym of the identifier for every
// app, current and historical, and reads the user's day and month in one
// query. Results are grouped by app.
func (s *Service) SearchUser(ctx context.Context, q UserQuery) UserSearch {
	snapshot := s.config.Capture()
	if q.App != "" && !storage.RecordName.MatchString(q.App) {
		return UserSearch{Invalid: true}
	}
	if !q.Anonymous && (s.identity == nil || useridentity.ValidateID(q.ID) != nil) {
		return UserSearch{Invalid: true}
	}
	if q.Anonymous && (q.App == "" || q.ID != "" || s.identity == nil) {
		return UserSearch{Invalid: true}
	}
	configured := configuredApps(snapshot)
	apps := configured
	if q.App != "" {
		apps = []string{q.App}
	} else if s.history != nil {
		var historical []string
		err := s.query(ctx, ScreenUsers, func(ctx context.Context) error {
			var err error
			historical, err = s.history.HistoricalApps(ctx)
			return err
		})
		if err != nil {
			return UserSearch{Spend: Unavailable}
		}
		for _, app := range historical {
			if storage.RecordName.MatchString(app) && !slices.Contains(apps, app) {
				apps = append(apps, app)
			}
		}
		slices.Sort(apps)
	}
	var subjects []subject
	for _, app := range apps {
		var hash useridentity.Hash
		if q.Anonymous {
			hash = s.identity.AnonymousHash(app)
		} else {
			h, err := s.identity.LookupHash(app, q.ID)
			if err != nil {
				return UserSearch{Invalid: true}
			}
			hash = h
		}
		_, ok := snapshot.App(app)
		subjects = append(subjects, subject{app: app, hash: hash.Hex(), anonymous: q.Anonymous, removed: !ok})
	}
	out := UserSearch{At: s.now().UTC()}
	if len(subjects) == 0 {
		return out
	}
	now := s.now()
	selector := storage.UsageSelector{Scope: "user"}
	windows := map[string]window{}
	for _, sub := range subjects {
		tz := "UTC"
		if app, ok := snapshot.App(sub.app); ok {
			tz = appTimezone(snapshot, app)
		}
		w := windowFor(now, tz)
		windows[sub.app] = w
		if selector.From == "" || w.monthStart < selector.From {
			selector.From = w.monthStart
		}
		if w.day > selector.Through {
			selector.Through = w.day
		}
		selector.Subjects = append(selector.Subjects, storage.UsageSubject{App: sub.app, Subject: sub.hash, Anonymous: sub.anonymous})
	}
	var rows []storage.Daily
	err := s.query(ctx, ScreenUsers, func(ctx context.Context) error {
		var err error
		rows, err = s.store.QueryDaily(ctx, selector)
		return err
	})
	if err != nil {
		out.Spend = Unavailable
	}
	for _, sub := range subjects {
		w := windows[sub.app]
		group := UserGroup{App: sub.app, Hash: ShortHash(sub.hash), Anonymous: sub.anonymous, Removed: sub.removed, Limits: NotVerifiable}
		days := map[string]*UserDay{}
		var order []string
		for _, d := range rows {
			if d.AppName != sub.app || d.Subject != sub.hash || d.Anonymous != sub.anonymous || d.Day < w.monthStart || d.Day > w.day {
				continue
			}
			day, ok := days[d.Day]
			if !ok {
				day = &UserDay{Day: d.Day}
				days[d.Day] = day
				order = append(order, d.Day)
			}
			day.Requests += d.Requests
			day.Cost += d.Cost
			day.Tokens += d.Input + d.Output + d.CacheWrite + d.CacheRead
			if d.Day == w.day {
				group.Today += d.Cost
			}
		}
		slices.Sort(order)
		for i := len(order) - 1; i >= 0; i-- {
			group.History = append(group.History, *days[order[i]])
		}
		if !sub.removed {
			state, availability := s.limitState(snapshot, sub.app, &limits.Subject{App: sub.app, Hash: sub.hash, Anonymous: sub.anonymous})
			group.Limits = availability
			if s.limits != nil {
				group.LimitRows = limitRows(state.Limits, limits.ScopeUser)
			}
		}
		if sub.removed && len(group.History) == 0 {
			continue
		}
		out.Groups = append(out.Groups, group)
	}
	return out
}
