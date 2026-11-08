package limits

import (
	"context"
	"log/slog"
	"math"
	"math/bits"
	"sync"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

const outboxLimit = 1000

// Options configures a Manager.
type Options struct {
	Store     storage.LimitStore
	Clock     Clock
	Logger    *slog.Logger
	Publisher events.Publisher
	// CheckpointInterval defaults to 5 seconds.
	CheckpointInterval time.Duration
}

type bucketKey struct {
	app       string
	scope     Scope
	subject   string
	anonymous bool
	dim       Dimension
	start     int64
	end       int64
}

type bucket struct {
	spent    int64
	reserved int64
	unknown  bool
	dirty    bool
}

type zone struct {
	active     string
	pending    string
	activateAt time.Time
}

type handleKey struct {
	requestID string
	index     int
}

// Handle is the opaque reservation of one attempt. It is internal: it is
// never a bearer value and never comes from a client.
type Handle struct {
	key      handleKey
	request  Request
	app      string
	items    []storage.LimitItem
	event    *rateEvent
	state    string
	finished bool
	degraded bool
}

// Degraded reports whether the attempt was admitted while limits were degraded
// under fail_open.
func (h *Handle) Degraded() bool { return h != nil && h.degraded }

type pendingSettlement struct {
	handle     *Handle
	settlement storage.Settlement
}

// Manager serializes every decision that touches an app and its users. Lock
// order is always the manager first, then the store: the store never calls
// back into the manager.
type Manager struct {
	mu         sync.Mutex
	store      storage.LimitStore
	clock      Clock
	logger     *slog.Logger
	publisher  events.Publisher
	interval   time.Duration
	guard      civilGuard
	windows    map[Subject]*slidingWindow
	concurrent map[Subject]int64
	buckets    map[bucketKey]*bucket
	removed    []bucketKey
	handles    map[handleKey]*Handle
	zones      map[string]*zone
	seq        int64
	retry      []pendingSettlement
	generation int64
	onLimit    map[string]config.OnLimit

	degraded      bool
	degradedSince time.Time
	affected      map[string]bool
	warnedAt      time.Duration
	warned        bool
	outbox        []events.Event
}

func New(options Options) *Manager {
	if options.Clock == nil {
		options.Clock = SystemClock()
	}
	if options.Logger == nil {
		options.Logger = slog.New(slog.DiscardHandler)
	}
	if options.CheckpointInterval <= 0 {
		options.CheckpointInterval = 5 * time.Second
	}
	return &Manager{
		store: options.Store, clock: options.Clock, logger: options.Logger, publisher: options.Publisher,
		interval: options.CheckpointInterval,
		windows:  map[Subject]*slidingWindow{}, concurrent: map[Subject]int64{}, buckets: map[bucketKey]*bucket{},
		handles: map[handleKey]*Handle{}, zones: map[string]*zone{}, onLimit: map[string]config.OnLimit{},
		affected: map[string]bool{},
	}
}

func checkedAdd(a, b int64) (int64, bool) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, false
	}
	return a + b, true
}

// now returns the effective civil instant and the monotonic elapsed time.
func (m *Manager) now() (time.Time, time.Duration) {
	civil, elapsed := m.clock.Now(), m.clock.Elapsed()
	effective, warn := m.guard.observe(civil, elapsed)
	if warn {
		m.logger.Warn("system clock moved backwards by more than 5 seconds; limit windows keep monotonic time")
	}
	return effective, elapsed
}

func (m *Manager) window(s Subject, now time.Duration) *slidingWindow {
	w := m.windows[s]
	if w == nil {
		w = newSlidingWindow()
		m.windows[s] = w
	}
	w.prune(now)
	return w
}

func keyFor(app string, item storage.LimitItem) bucketKey {
	return bucketKey{app: app, scope: Scope(item.Scope), subject: item.Subject, anonymous: item.Anonymous, dim: Dimension(item.Dimension), start: item.WindowStart, end: item.WindowEnd}
}

func subjectKey(app string, s Subject, d Dimension, w window) bucketKey {
	return bucketKey{app: app, scope: s.scope(), subject: s.Hash, anonymous: s.Anonymous, dim: d, start: w.start.UnixMicro(), end: w.end.UnixMicro()}
}

func (m *Manager) bucket(k bucketKey) *bucket {
	b := m.buckets[k]
	if b == nil {
		b = &bucket{}
		m.buckets[k] = b
	}
	return b
}

func (m *Manager) peek(k bucketKey) bucket {
	if b := m.buckets[k]; b != nil {
		return *b
	}
	return bucket{}
}

// zoneFor returns the timezone in force for an app. A new timezone waits until
// every active calendar window of the app ends, so a change never renews a
// budget early or opens two windows over the same period.
func (m *Manager) zoneFor(p Policy, at time.Time) string {
	z := m.zones[p.App]
	if z == nil {
		z = &zone{active: p.Timezone}
		m.zones[p.App] = z
	}
	switch {
	case p.Timezone == z.active:
		z.pending, z.activateAt = "", time.Time{}
	case z.pending != p.Timezone:
		z.pending = p.Timezone
		z.activateAt = m.activeEnd(p, at)
	}
	if z.pending != "" && !at.Before(z.activateAt) {
		m.activate(p.App, z, at)
	}
	return z.active
}

// activeEnd is the latest end among the windows of the configured budgets that
// already hold spend or reservations. Dimensions tracked without a ceiling do
// not delay the change: activation seeds them conservatively anyway.
func (m *Manager) activeEnd(p Policy, at time.Time) time.Time {
	end := at
	for k, b := range m.buckets {
		if k.app != p.App || (b.spent == 0 && b.reserved == 0 && !b.unknown) {
			continue
		}
		set := p.set(k.scope)
		if ceiling(set.Budget, k.dim) <= 0 && ceiling(set.HardCap, k.dim) <= 0 {
			continue
		}
		if e := time.UnixMicro(k.end).UTC(); e.After(end) {
			end = e
		}
	}
	return end
}

// activate switches an app to its pending timezone. The window containing the
// cut is seeded with whatever the old windows overlapping it already hold, so
// the change never grants an empty window over a period already charged.
func (m *Manager) activate(app string, z *zone, at time.Time) {
	type subjectDim struct {
		subject Subject
		dim     Dimension
	}
	seeds := map[subjectDim]bucket{}
	for k, b := range m.buckets {
		if k.app != app {
			continue
		}
		s := Subject{App: app, Hash: k.subject, Anonymous: k.anonymous}
		next, err := windowFor(at, k.dim, z.pending)
		if err != nil {
			continue
		}
		old := window{time.UnixMicro(k.start), time.UnixMicro(k.end)}
		if k == subjectKey(app, s, k.dim, next) || !overlaps(old, next) {
			continue
		}
		seed := seeds[subjectDim{s, k.dim}]
		total, ok := checkedAdd(b.spent, b.reserved)
		if ok {
			total, ok = checkedAdd(seed.spent, total)
		}
		if !ok {
			total = math.MaxInt64
		}
		seed.spent = total
		seed.unknown = seed.unknown || b.unknown
		seeds[subjectDim{s, k.dim}] = seed
	}
	for sd, seed := range seeds {
		next, _ := windowFor(at, sd.dim, z.pending)
		b := m.bucket(subjectKey(app, sd.subject, sd.dim, next))
		total, ok := checkedAdd(b.spent, seed.spent)
		if !ok {
			total = math.MaxInt64
		}
		b.spent = total
		b.unknown = b.unknown || seed.unknown
		b.dirty = true
	}
	m.logger.Info("app timezone change applied to limits", "app", app, "timezone", z.pending)
	z.active, z.pending, z.activateAt = z.pending, "", time.Time{}
}

func (m *Manager) observeGeneration(p Policy, generation int64) error {
	if generation < m.generation {
		return ErrStale
	}
	m.generation = generation
	if p.OnLimit != "" && p.OnLimit != config.OnLimitBlock && m.onLimit[p.App] != p.OnLimit {
		m.logger.Warn("on_limit policy is not implemented yet; limits block requests", "app", p.App, "on_limit", string(p.OnLimit))
	}
	m.onLimit[p.App] = p.OnLimit
	return nil
}

// Evaluate decides without effects. It is observational and never authorizes
// a send: only Reserve does.
func (m *Manager) Evaluate(p Policy, r Request) (Decision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	civil, elapsed := m.now()
	return m.evaluate(p, r, civil, elapsed, m.zoneFor(p, civil))
}

func (m *Manager) evaluate(p Policy, r Request, civil time.Time, now time.Duration, tz string) (Decision, error) {
	if r.EstimatedTokens < 0 || r.ReservedCost < 0 || r.Subject.Hash == "" || r.Subject.App != p.App {
		return Decision{}, ErrIntegrity
	}
	var out []Violation
	subjects := []Subject{r.Subject.appSubject(), r.Subject}
	generation := r.Operation == OperationGeneration
	violation := func(s Subject, kind Kind, reason Reason, d Dimension, used, reserved, requested, limit int64) Violation {
		return Violation{Kind: kind, Reason: reason, Scope: s.scope(), LimitName: p.LimitName(s.scope(), kind, d), Dimension: d, Unit: d.Unit(),
			Used: used, Reserved: reserved, Requested: requested, Limit: limit, TooLarge: requested > limit, App: p.App}
	}
	for _, s := range subjects {
		limit := p.set(s.scope()).rate(RequestsPerMinute)
		if limit <= 0 {
			continue
		}
		w := m.window(s, now)
		if w.requests()+1 > limit {
			v := violation(s, KindRate, ReasonSpent, RequestsPerMinute, w.requests(), 0, 1, limit)
			if wait := w.requestRetry(limit, now); wait > 0 {
				v.RetryAfter = ceilSeconds(wait)
				renew := civil.Add(time.Duration(v.RetryAfter) * time.Second)
				v.RenewsAt = &renew
			}
			out = append(out, v)
		}
	}
	if generation {
		for _, s := range subjects {
			limit := p.set(s.scope()).rate(TokensPerMinute)
			if limit <= 0 {
				continue
			}
			w := m.window(s, now)
			used, reserved := w.usedTokens(), w.reservedTokens()
			total, ok := checkedAdd(used, r.EstimatedTokens)
			if !ok {
				return Decision{}, ErrIntegrity
			}
			if total > limit {
				reason := ReasonReservation
				if used-reserved+r.EstimatedTokens > limit {
					reason = ReasonSpent
				}
				v := violation(s, KindRate, reason, TokensPerMinute, used-reserved, reserved, r.EstimatedTokens, limit)
				if wait := w.tokenRetry(limit, r.EstimatedTokens, now); wait > 0 {
					v.RetryAfter = ceilSeconds(wait)
					renew := civil.Add(time.Duration(v.RetryAfter) * time.Second)
					v.RenewsAt = &renew
				}
				out = append(out, v)
			}
		}
	}
	for _, s := range subjects {
		limit := p.set(s.scope()).rate(MaxConcurrent)
		if limit <= 0 {
			continue
		}
		if current := m.concurrent[s]; current+1 > limit {
			out = append(out, violation(s, KindConcurrency, ReasonSpent, MaxConcurrent, current, 0, 1, limit))
		}
	}
	if generation {
		for _, hard := range []bool{true, false} {
			for _, s := range subjects {
				set := p.set(s.scope())
				ceilings := set.Budget
				if hard {
					ceilings = set.HardCap
				}
				for _, d := range calendarDimensions {
					limit := ceiling(ceilings, d)
					if limit <= 0 {
						continue
					}
					w, err := windowFor(civil, d, tz)
					if err != nil {
						return Decision{}, ErrIntegrity
					}
					b := m.peek(subjectKey(p.App, s, d, w))
					requested := r.EstimatedTokens
					if d.money() {
						requested = int64(r.ReservedCost)
					}
					total, ok := checkedAdd(b.spent, b.reserved)
					if ok {
						total, ok = checkedAdd(total, requested)
					}
					if !ok {
						return Decision{}, ErrIntegrity
					}
					if total <= limit && !b.unknown {
						continue
					}
					reason := ReasonReservation
					if b.unknown || b.spent+requested > limit {
						reason = ReasonSpent
					}
					kind := KindBudget
					switch {
					case hard:
						kind = KindHardCap
					case reason == ReasonReservation:
						kind = KindReservation
					}
					v := violation(s, kind, reason, d, b.spent, b.reserved, requested, limit)
					if hard {
						v.LimitName = p.LimitName(s.scope(), KindHardCap, d)
					}
					v.hardCap = hard
					renew := w.end
					v.RenewsAt = &renew
					v.Timezone = tz
					out = append(out, v)
				}
			}
		}
	}
	decision := Decision{Allowed: len(out) == 0, Exceeded: out}
	if len(out) > 0 {
		decision.Primary = &out[0]
	}
	return decision, nil
}

// Reserve admits an attempt. Under the manager lock it re-evaluates every
// limit, builds the frozen reservation and hands it to commit, which must make
// it durable together with the usage journal. Memory changes only after the
// commit succeeds, and nothing may be sent upstream before Reserve returns.
func (m *Manager) Reserve(p Policy, r Request, commit func(storage.Reservation) (int64, error)) (*Handle, Decision, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.observeGeneration(p, r.Generation); err != nil {
		return nil, Decision{}, err
	}
	key := handleKey{r.RequestID, r.AttemptIndex}
	if existing := m.handles[key]; existing != nil {
		if existing.request == r {
			return existing, Decision{Allowed: true, Degraded: existing.degraded}, nil
		}
		return nil, Decision{}, ErrConflict
	}
	civil, elapsed := m.now()
	degraded := m.degraded
	if degraded {
		m.noteAffected(p.App, civil)
		if p.FailMode != config.FailOpen {
			return nil, Decision{Degraded: true}, ErrUnavailable
		}
	}
	tz := m.zoneFor(p, civil)
	decision, err := m.evaluate(p, r, civil, elapsed, tz)
	if err != nil || !decision.Allowed {
		decision.Degraded = degraded
		return nil, decision, err
	}
	decision.Degraded = degraded
	reservation, err := m.buildReservation(p, r, civil, tz, degraded)
	if err != nil {
		return nil, decision, err
	}
	seq, err := commit(reservation)
	if err != nil {
		return nil, decision, err
	}
	h := &Handle{key: key, request: r, app: p.App, items: reservation.Items, state: storage.ReservationReserved, degraded: degraded}
	if err := m.applyAdmission(h, elapsed, civil, seq); err != nil {
		m.markDegraded(civil, "integrity")
		return nil, decision, err
	}
	decision.Budget = m.budgetStatus(p, r.Subject, civil, tz)
	return h, decision, nil
}

func (m *Manager) buildReservation(p Policy, r Request, civil time.Time, tz string, degraded bool) (storage.Reservation, error) {
	res := storage.Reservation{
		RequestID: r.RequestID, AttemptIndex: r.AttemptIndex, AppName: p.App, Subject: r.Subject.Hash, Anonymous: r.Subject.Anonymous,
		AdmittedAt: civil, State: storage.ReservationReserved,
		Quote: storage.LimitQuote{Operation: string(r.Operation), EstimatedTokens: r.EstimatedTokens, ReservedCost: r.ReservedCost, Generation: r.Generation, Degraded: degraded},
	}
	tokens := int64(0)
	if r.Operation == OperationGeneration {
		tokens = r.EstimatedTokens
		for _, s := range []Subject{r.Subject.appSubject(), r.Subject} {
			for _, d := range calendarDimensions {
				w, err := windowFor(civil, d, tz)
				if err != nil {
					return res, ErrIntegrity
				}
				amount := r.EstimatedTokens
				if d.money() {
					amount = int64(r.ReservedCost)
				}
				res.Items = append(res.Items, storage.LimitItem{Scope: string(s.scope()), Subject: s.Hash, Anonymous: s.Anonymous, Dimension: string(d),
					WindowStart: w.start.UnixMicro(), WindowEnd: w.end.UnixMicro(), Timezone: tz, Reserved: amount})
			}
		}
	}
	if r.AttemptIndex == 1 {
		res.Rate = &storage.RateEvent{RequestID: r.RequestID, AppName: p.App, Subject: r.Subject.Hash, Anonymous: r.Subject.Anonymous,
			AdmittedAt: civil, EstimatedTokens: tokens, Active: true}
	}
	return res, nil
}

func (m *Manager) applyAdmission(h *Handle, elapsed time.Duration, civil time.Time, seq int64) error {
	var failure error
	for _, item := range h.items {
		b := m.bucket(keyFor(h.app, item))
		total, ok := checkedAdd(b.reserved, item.Reserved)
		if !ok {
			failure = ErrIntegrity
			total = math.MaxInt64
		}
		b.reserved = total
		b.dirty = true
	}
	subjects := []Subject{h.request.Subject.appSubject(), h.request.Subject}
	if h.request.AttemptIndex == 1 {
		tokens := int64(0)
		if h.request.Operation == OperationGeneration {
			tokens = h.request.EstimatedTokens
		}
		h.event = &rateEvent{requestID: h.request.RequestID, at: elapsed, civil: civil, tokens: tokens, active: true}
		for _, s := range subjects {
			m.window(s, elapsed).add(h.event)
		}
	}
	for _, s := range subjects {
		m.concurrent[s]++
	}
	m.handles[h.key] = h
	m.seq = max(m.seq, seq)
	return failure
}

// finish frees the concurrency slot exactly once, even when the financial
// settlement still has to be retried.
func (m *Manager) finish(h *Handle) {
	if h.finished {
		return
	}
	h.finished = true
	for _, s := range []Subject{h.request.Subject.appSubject(), h.request.Subject} {
		if m.concurrent[s] > 0 {
			m.concurrent[s]--
		}
		if m.concurrent[s] == 0 {
			delete(m.concurrent, s)
		}
	}
}

func (h *Handle) settlement(transition string, u Usage, at time.Time) storage.Settlement {
	st := storage.Settlement{RequestID: h.request.RequestID, AttemptIndex: h.request.AttemptIndex, Transition: transition, At: at, Settled: make([]int64, len(h.items))}
	tokens := int64(0)
	if transition == storage.TransitionSettle && h.request.Operation == OperationGeneration {
		tokens = u.Tokens
		if !u.Complete {
			tokens = max(u.Tokens, h.request.EstimatedTokens)
		}
		for i, item := range h.items {
			switch {
			case Dimension(item.Dimension).money():
				st.Settled[i] = int64(u.Cost)
			case u.Complete:
				st.Settled[i] = u.Tokens
			default:
				st.Settled[i] = max(u.Tokens, item.Reserved)
			}
		}
	}
	if h.event != nil {
		st.RateTokens = &tokens
	}
	return st
}

// Settle replaces the reservation with the real consumption. persist must make
// the settlement durable, normally together with the usage result; nil uses
// the limit store alone. On failure the slot is still freed, the financial
// obligation stays reserved and the settlement is retried by the worker.
func (m *Manager) Settle(h *Handle, u Usage, persist func(storage.Settlement) (int64, error)) error {
	if u.Cost < 0 || u.Tokens < 0 {
		return ErrIntegrity
	}
	return m.end(h, storage.TransitionSettle, u, persist)
}

// Release returns a reservation that consumed nothing. It never invents a zero
// consumption for an attempt that was sent: the request event stays counted.
func (m *Manager) Release(h *Handle, _ ReleaseReason, persist func(storage.Settlement) (int64, error)) error {
	return m.end(h, storage.TransitionRelease, Usage{}, persist)
}

func (m *Manager) end(h *Handle, transition string, u Usage, persist func(storage.Settlement) (int64, error)) error {
	if h == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.finish(h)
	if h.state != storage.ReservationReserved {
		return nil
	}
	for _, pending := range m.retry {
		if pending.handle == h {
			return nil
		}
	}
	civil, _ := m.now()
	st := h.settlement(transition, u, civil)
	if persist == nil {
		persist = func(st storage.Settlement) (int64, error) {
			return m.store.SettleReservation(context.Background(), st)
		}
	}
	seq, err := persist(st)
	if err != nil {
		// The obligation stays reserved, which is conservative. Limit state only
		// becomes degraded if the worker's retry fails as well.
		m.retry = append(m.retry, pendingSettlement{h, st})
		return err
	}
	m.applySettlement(h, st, seq)
	return nil
}

func (m *Manager) applySettlement(h *Handle, st storage.Settlement, seq int64) {
	for i, item := range h.items {
		b := m.bucket(keyFor(h.app, item))
		b.reserved -= item.Reserved
		if b.reserved < 0 {
			m.logger.Error("limit reservation underflow; state kept conservative")
			b.reserved = 0
		}
		total, ok := checkedAdd(b.spent, st.Settled[i])
		if !ok {
			total = math.MaxInt64
		}
		b.spent = total
		b.dirty = true
	}
	if e := h.event; e != nil && e.active {
		tokens := int64(0)
		if st.RateTokens != nil {
			tokens = *st.RateTokens
		}
		for _, s := range []Subject{h.request.Subject.appSubject(), h.request.Subject} {
			if w := m.windows[s]; w != nil {
				w.settle(e, tokens, true)
			}
		}
		e.tokens, e.active, e.settled = tokens, false, true
	}
	h.state = storage.ReservationSettled
	if st.Transition == storage.TransitionRelease {
		h.state = storage.ReservationReleased
	}
	delete(m.handles, h.key)
	m.seq = max(m.seq, seq)
}

// budgetStatus picks the applicable budget with the smallest fraction left,
// compared exactly. Ties keep the stable order: hard caps, then budgets; app
// before user; then the calendar dimension order.
func (m *Manager) budgetStatus(p Policy, user Subject, civil time.Time, tz string) *BudgetStatus {
	var best *BudgetStatus
	for _, hard := range []bool{true, false} {
		for _, s := range []Subject{user.appSubject(), user} {
			set := p.set(s.scope())
			ceilings := set.Budget
			kind := KindBudget
			if hard {
				ceilings, kind = set.HardCap, KindHardCap
			}
			for _, d := range calendarDimensions {
				limit := ceiling(ceilings, d)
				if limit <= 0 {
					continue
				}
				w, err := windowFor(civil, d, tz)
				if err != nil {
					return nil
				}
				b := m.peek(subjectKey(p.App, s, d, w))
				remaining := int64(0)
				if used, ok := checkedAdd(b.spent, b.reserved); ok && used < limit && !b.unknown {
					remaining = limit - used
				}
				candidate := &BudgetStatus{LimitName: p.LimitName(s.scope(), kind, d), Unit: d.Unit(), Remaining: remaining, Limit: limit, ResetAt: w.end}
				if best == nil || smallerFraction(candidate, best) {
					best = candidate
				}
			}
		}
	}
	return best
}

func smallerFraction(a, b *BudgetStatus) bool {
	ahi, alo := bits.Mul64(uint64(a.Remaining), uint64(b.Limit))
	bhi, blo := bits.Mul64(uint64(b.Remaining), uint64(a.Limit))
	return ahi < bhi || (ahi == bhi && alo < blo)
}

// State returns a consistent copy of the limits of an app and, optionally, of
// one of its users.
func (m *Manager) State(p Policy, user *Subject) State {
	m.mu.Lock()
	defer m.mu.Unlock()
	civil, elapsed := m.now()
	tz := m.zoneFor(p, civil)
	state := State{App: p.App, Degraded: m.degraded, ActiveTimezone: tz, Generation: m.generation}
	if z := m.zones[p.App]; z != nil && z.pending != "" {
		state.PendingTimezone = z.pending
		at := z.activateAt
		state.PendingAt = &at
	}
	subjects := []Subject{{App: p.App}}
	state.Concurrent = m.concurrent[subjects[0]]
	if user != nil {
		u := *user
		u.App = p.App
		subjects = append(subjects, u)
		state.UserConcurrent = m.concurrent[u]
	}
	for _, s := range subjects {
		set := p.set(s.scope())
		w := m.window(s, elapsed)
		if limit := set.rate(RequestsPerMinute); limit > 0 {
			state.Limits = append(state.Limits, LimitView{LimitName: p.LimitName(s.scope(), KindRate, RequestsPerMinute), Scope: s.scope(), Kind: KindRate, Dimension: RequestsPerMinute, Unit: UnitRequests, Used: w.requests(), Limit: limit})
		}
		if limit := set.rate(TokensPerMinute); limit > 0 {
			state.Limits = append(state.Limits, LimitView{LimitName: p.LimitName(s.scope(), KindRate, TokensPerMinute), Scope: s.scope(), Kind: KindRate, Dimension: TokensPerMinute, Unit: UnitTokens, Used: w.usedTokens() - w.reservedTokens(), Reserved: w.reservedTokens(), Limit: limit})
		}
		if limit := set.rate(MaxConcurrent); limit > 0 {
			state.Limits = append(state.Limits, LimitView{LimitName: p.LimitName(s.scope(), KindConcurrency, MaxConcurrent), Scope: s.scope(), Kind: KindConcurrency, Dimension: MaxConcurrent, Unit: UnitConcurrent, Used: m.concurrent[s], Limit: limit})
		}
		for _, hard := range []bool{true, false} {
			ceilings, kind := set.Budget, KindBudget
			if hard {
				ceilings, kind = set.HardCap, KindHardCap
			}
			for _, d := range calendarDimensions {
				limit := ceiling(ceilings, d)
				if limit <= 0 {
					continue
				}
				win, err := windowFor(civil, d, tz)
				if err != nil {
					continue
				}
				b := m.peek(subjectKey(p.App, s, d, win))
				renew := win.end
				state.Limits = append(state.Limits, LimitView{LimitName: p.LimitName(s.scope(), kind, d), Scope: s.scope(), Kind: kind, Dimension: d, Unit: d.Unit(), Used: b.spent, Reserved: b.reserved, Limit: limit, RenewsAt: &renew, Unknown: b.unknown})
			}
		}
	}
	return state
}

// Degraded reports whether limit state is currently unverifiable.
func (m *Manager) Degraded() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.degraded
}

func (m *Manager) queue(e events.Event) {
	if len(m.outbox) >= outboxLimit {
		m.outbox = m.outbox[1:]
	}
	m.outbox = append(m.outbox, e)
}

func (m *Manager) markDegraded(civil time.Time, reason string) {
	elapsed := m.clock.Elapsed()
	if !m.degraded {
		m.degraded = true
		m.degradedSince = civil
		m.affected = map[string]bool{}
		e := events.New(events.LimitStateUnavailable, civil)
		e.Payload.Severity = "warning"
		e.Payload.Reason = reason
		m.queue(e)
	}
	if !m.warned || elapsed-m.warnedAt >= warningInterval {
		m.warned = true
		m.warnedAt = elapsed
		m.logger.Warn("limit state unavailable; applying each app's fail mode", "reason", reason)
	}
}

func (m *Manager) noteAffected(app string, civil time.Time) {
	if m.affected[app] {
		return
	}
	m.affected[app] = true
	e := events.New(events.LimitStateUnavailable, civil)
	e.App = app
	e.Payload.Severity = "warning"
	e.Payload.DegradedSeconds = int64(civil.Sub(m.degradedSince) / time.Second)
	m.queue(e)
	m.logger.Warn("limit state unavailable for app; applying its fail mode", "app", app)
}

func (m *Manager) markRecovered(civil time.Time) {
	if !m.degraded {
		return
	}
	m.degraded = false
	duration := int64(civil.Sub(m.degradedSince) / time.Second)
	e := events.New(events.LimitStateRecovered, civil)
	e.Payload.Severity = "info"
	e.Payload.DegradedSeconds = duration
	m.queue(e)
	for app := range m.affected {
		e := events.New(events.LimitStateRecovered, civil)
		e.App = app
		e.Payload.Severity = "info"
		e.Payload.DegradedSeconds = duration
		m.queue(e)
	}
	m.affected = map[string]bool{}
	m.logger.Info("limit state recovered", "degraded_seconds", duration)
}
