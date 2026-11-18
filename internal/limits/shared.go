package limits

import (
	"slices"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// SharedBucket is one calendar bucket read from a shared projection.
type SharedBucket struct {
	storage.BudgetBucket
	Unknown bool
}

// SharedRate is one admitted attempt in a shared sliding window.
type SharedRate struct {
	RequestID string
	Attempt   int
	At        time.Time
	Tokens    int64
	Active    bool
}

// SharedState is the slice of a shared projection one admission of one app
// needs: the buckets of the app and of the user, both sliding windows, both
// concurrency counts and the timezone state of the app. Now is the shared
// clock, so replicas with drifting clocks never see different windows.
type SharedState struct {
	Now        time.Time
	Buckets    []SharedBucket
	Rates      map[Subject][]SharedRate
	Concurrent map[Subject]int64
	Zone       *storage.ZoneState
	Degraded   bool
}

// Plan is the outcome of one shared decision. Reservation is what must be
// applied to the shared projection and made durable; Seeds are absolute bucket
// values written by a timezone activation; Zone is set when the timezone state
// of the app changed.
type Plan struct {
	Reservation storage.Reservation
	Selection   Selection
	Decision    Decision
	Slot        bool
	Zone        *storage.ZoneState
	Seeds       []SharedBucket
	// Activated reports that a pending timezone took effect in this decision.
	// The seeds then cover only the subjects read; ActivationSeeds covers the
	// rest of the app.
	Activated bool
}

type fixedClock struct {
	civil   time.Time
	elapsed time.Duration
}

func (c fixedClock) Now() time.Time               { return c.civil }
func (c fixedClock) Elapsed() time.Duration       { return c.elapsed }
func (c fixedClock) NewTimer(time.Duration) Timer { return nil }

// Decide applies exactly the rules of the local coordinator to a shared state:
// the same evaluation, the same on_limit policy, the same reservation shape
// and the same budget status. It has no effects; the caller applies the plan
// atomically and only if the state it read is still current. A refusal
// returns a plan without a reservation.
func Decide(p Policy, r Request, candidates []Candidate, state SharedState, continuation bool) (Plan, error) {
	elapsed := time.Duration(state.Now.UnixMilli()) * time.Millisecond
	m := New(Options{Clock: fixedClock{civil: state.Now.UTC(), elapsed: elapsed}})
	m.degraded = state.Degraded
	for _, b := range state.Buckets {
		k := bucketKey{app: b.AppName, scope: Scope(b.Scope), subject: b.Subject, anonymous: b.Anonymous, dim: Dimension(b.Dimension), start: b.WindowStart, end: b.WindowEnd}
		m.buckets[k] = &bucket{spent: b.Spent, reserved: b.Reserved, unknown: b.Unknown}
	}
	for s, rates := range state.Rates {
		ordered := slices.Clone(rates)
		slices.SortStableFunc(ordered, func(a, b SharedRate) int { return a.At.Compare(b.At) })
		w := newSlidingWindow()
		for _, e := range ordered {
			w.add(&rateEvent{requestID: e.RequestID, at: time.Duration(e.At.UnixMilli()) * time.Millisecond, civil: e.At,
				tokens: e.Tokens, active: e.Active, settled: !e.Active, request: e.Attempt <= 1})
		}
		w.prune(elapsed)
		m.windows[s] = w
	}
	for s, n := range state.Concurrent {
		m.concurrent[s] = n
	}
	var plan Plan
	var before storage.ZoneState
	if z := state.Zone; z != nil {
		restored := &zone{active: z.Active, pending: z.Pending}
		if z.Pending != "" {
			restored.activateAt = time.UnixMicro(z.ActivateAt).UTC()
		}
		m.zones[p.App] = restored
		before = *z
	}
	civil, _ := m.now()
	m.zoneFor(p, civil)
	plan.Activated = state.Zone != nil && m.zones[p.App] != nil && m.zones[p.App].active != before.Active
	for k, b := range m.buckets {
		if b.dirty {
			plan.Seeds = append(plan.Seeds, SharedBucket{BudgetBucket: storage.BudgetBucket{AppName: k.app, Scope: string(k.scope), Subject: k.subject, Anonymous: k.anonymous,
				Dimension: string(k.dim), WindowStart: k.start, WindowEnd: k.end, Spent: b.spent, Reserved: b.reserved}, Unknown: b.unknown})
			b.dirty = false
		}
	}
	if z := m.zones[p.App]; z != nil {
		after := storage.ZoneState{Active: z.active, Pending: z.pending}
		if z.pending != "" {
			after.ActivateAt = z.activateAt.UnixMicro()
		}
		if after != before || state.Zone == nil {
			plan.Zone = &after
		}
	}
	var op *Session
	if continuation {
		op = &Session{policy: p, request: r}
	}
	h, decision, selection, err := m.reserveLocked(p, r, candidates, func(res storage.Reservation, _ Selection) (int64, error) {
		plan.Reservation = res
		return 0, nil
	}, op)
	plan.Decision, plan.Selection = decision, selection
	if err != nil || h == nil {
		plan.Reservation = storage.Reservation{}
		return plan, err
	}
	plan.Slot = !continuation
	return plan, nil
}

// Export returns the whole projection, for a shared store rebuilt from the
// durable ledger. It runs after Recover and before any admission.
func (m *Manager) Export() (buckets []SharedBucket, zones map[string]storage.ZoneState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	zones = map[string]storage.ZoneState{}
	for app, z := range m.zones {
		state := storage.ZoneState{Active: z.active, Pending: z.pending}
		if z.pending != "" {
			state.ActivateAt = z.activateAt.UnixMicro()
		}
		zones[app] = state
	}
	for k, b := range m.buckets {
		buckets = append(buckets, SharedBucket{BudgetBucket: storage.BudgetBucket{AppName: k.app, Scope: string(k.scope), Subject: k.subject, Anonymous: k.anonymous,
			Dimension: string(k.dim), WindowStart: k.start, WindowEnd: k.end, Spent: b.spent, Reserved: b.reserved}, Unknown: b.unknown})
	}
	return buckets, zones
}

// SettlementFor computes the settlement of a reservation exactly as the local
// coordinator does, so both modes charge the same amounts.
func SettlementFor(res storage.Reservation, transition string, u Usage, at time.Time) storage.Settlement {
	h := &Handle{items: res.Items, request: Request{RequestID: res.RequestID, AttemptIndex: res.AttemptIndex,
		Operation: Operation(res.Quote.Operation), EstimatedTokens: res.Quote.EstimatedTokens, ReservedCost: res.Quote.ReservedCost}}
	if res.Rate != nil {
		h.event = &rateEvent{}
	}
	return h.settlement(transition, u, at)
}

// RecoveredUsage derives the end of an open reservation from the evidence a
// crash left, as recovery does locally.
func RecoveredUsage(res storage.Reservation, result *storage.Record) (string, Usage) {
	return recoveredUsage(res, result)
}

// SubjectOf returns the subject a reservation charges.
func SubjectOf(res storage.Reservation) Subject {
	return Subject{App: res.AppName, Hash: res.Subject, Anonymous: res.Anonymous}
}

// ActivationSeeds applies a timezone change to every bucket of an app, as the
// local coordinator does at the activation instant. It returns the buckets of
// the new windows with the spend already charged to overlapping old windows.
func ActivationSeeds(app, timezone string, at time.Time, buckets []SharedBucket) []SharedBucket {
	m := New(Options{Clock: fixedClock{civil: at.UTC()}})
	for _, b := range buckets {
		k := bucketKey{app: b.AppName, scope: Scope(b.Scope), subject: b.Subject, anonymous: b.Anonymous, dim: Dimension(b.Dimension), start: b.WindowStart, end: b.WindowEnd}
		m.buckets[k] = &bucket{spent: b.Spent, reserved: b.Reserved, unknown: b.Unknown}
	}
	m.activate(app, &zone{pending: timezone}, at.UTC())
	var out []SharedBucket
	for k, b := range m.buckets {
		if b.dirty {
			out = append(out, SharedBucket{BudgetBucket: storage.BudgetBucket{AppName: k.app, Scope: string(k.scope), Subject: k.subject, Anonymous: k.anonymous,
				Dimension: string(k.dim), WindowStart: k.start, WindowEnd: k.end, Spent: b.spent, Reserved: b.reserved}, Unknown: b.unknown})
		}
	}
	return out
}
