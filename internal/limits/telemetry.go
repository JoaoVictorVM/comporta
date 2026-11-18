package limits

// Aggregate is a view of budget pressure without any subject: the app totals
// and, for per-user ceilings, the largest ratio among the users active in the
// current window. Nothing in it identifies a user.
type Aggregate struct {
	Generation int64
	Degraded   bool
	Apps       []AppAggregate
}

// AppAggregate is the budget view of one app.
type AppAggregate struct {
	App string
	// UsersObserved counts the users active in a current window of a
	// dimension that has a per-user ceiling.
	UsersObserved int
	Entries       []AggregateEntry
}

// AggregateScope says whose ceiling a ratio refers to.
type AggregateScope string

const (
	AggregateApp     AggregateScope = "app"
	AggregateUserMax AggregateScope = "user_max"
)

// AggregateEntry is the pressure on one ceiling. Used and Reserved are
// fractions of the ceiling, kept apart. For user_max each one is the largest
// fraction of any single user, never a ratio between sums. Unknown means the
// window holds an unverifiable amount, so its ratios must not be shown.
type AggregateEntry struct {
	Scope     AggregateScope
	Dimension Dimension
	Kind      Kind
	Used      float64
	Reserved  float64
	Unknown   bool
}

// Aggregate reads the budget state of the given apps under the coordinator
// lock and returns copies only. Ceilings that are not configured produce no
// entry; a configured per-user ceiling without active users yields zero.
func (m *Manager) Aggregate(policies []Policy) Aggregate {
	m.mu.Lock()
	defer m.mu.Unlock()
	civil, _ := m.now()
	out := Aggregate{Generation: m.generation, Degraded: m.degraded}
	type userKey struct {
		app       string
		subject   string
		anonymous bool
	}
	for _, p := range policies {
		tz := m.zoneFor(p, civil)
		view := AppAggregate{App: p.App}
		users := map[userKey]bool{}
		for _, hard := range []bool{true, false} {
			for _, d := range calendarDimensions {
				w, err := windowFor(civil, d, tz)
				if err != nil {
					continue
				}
				start, end := w.start.UnixMicro(), w.end.UnixMicro()
				kind := KindBudget
				appCeiling, userCeiling := ceiling(p.AppSet.Budget, d), ceiling(p.UserSet.Budget, d)
				if hard {
					kind = KindHardCap
					appCeiling, userCeiling = ceiling(p.AppSet.HardCap, d), ceiling(p.UserSet.HardCap, d)
				}
				if appCeiling > 0 {
					b := m.peek(subjectKey(p.App, Subject{App: p.App}, d, w))
					view.Entries = append(view.Entries, AggregateEntry{Scope: AggregateApp, Dimension: d, Kind: kind,
						Used: fraction(b.spent, appCeiling), Reserved: fraction(b.reserved, appCeiling), Unknown: b.unknown})
				}
				if userCeiling <= 0 {
					continue
				}
				entry := AggregateEntry{Scope: AggregateUserMax, Dimension: d, Kind: kind}
				for k, b := range m.buckets {
					if k.app != p.App || k.scope != ScopeUser || k.dim != d || k.start != start || k.end != end {
						continue
					}
					users[userKey{k.app, k.subject, k.anonymous}] = true
					entry.Used = max(entry.Used, fraction(b.spent, userCeiling))
					entry.Reserved = max(entry.Reserved, fraction(b.reserved, userCeiling))
					entry.Unknown = entry.Unknown || b.unknown
				}
				view.Entries = append(view.Entries, entry)
			}
		}
		view.UsersObserved = len(users)
		out.Apps = append(out.Apps, view)
	}
	return out
}

func fraction(value, limit int64) float64 {
	if limit <= 0 || value <= 0 {
		return 0
	}
	return float64(value) / float64(limit)
}
