package limits

import (
	"context"
	"errors"
	"math"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// Recover rebuilds the projection before any listener opens: checkpoint, then
// the ledger after it, then the reservations a crash left open, which are
// settled from their recorded result or, without one, at their reserved value.
// Nothing is ever sent upstream again. The first start on a database without a
// checkpoint seeds budgets from the usage history and writes one at once, so a
// later restart never imports that history a second time.
func (m *Manager) Recover(ctx context.Context, snapshot *config.Snapshot) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	civil, elapsed := m.clock.Now(), m.clock.Elapsed()
	state, err := m.store.LoadLimits(ctx, civil.Add(-slidingSpan))
	if err != nil {
		return err
	}
	covered := int64(0)
	if cp := state.Checkpoint; cp != nil {
		m.guard.restore(cp.Watermark)
		covered = cp.CoveredSeq
		m.seq = covered
		for _, row := range cp.Buckets {
			k := bucketKey{app: row.AppName, scope: Scope(row.Scope), subject: row.Subject, anonymous: row.Anonymous, dim: Dimension(row.Dimension), start: row.WindowStart, end: row.WindowEnd}
			m.buckets[k] = &bucket{spent: row.Spent, reserved: row.Reserved}
		}
		for _, ref := range cp.Metadata.Unknown {
			k := bucketKey{app: ref.AppName, scope: Scope(ref.Scope), subject: ref.Subject, anonymous: ref.Anonymous, dim: Dimension(ref.Dimension), start: ref.WindowStart, end: ref.WindowEnd}
			m.bucket(k).unknown = true
		}
		for app, z := range cp.Metadata.Zones {
			restored := &zone{active: z.Active, pending: z.Pending}
			if z.Pending != "" {
				restored.activateAt = time.UnixMicro(z.ActivateAt).UTC()
			}
			m.zones[app] = restored
		}
	}
	effective, _ := m.guard.observe(civil, elapsed)
	for _, entry := range state.Ledger {
		if entry.Seq <= covered {
			continue
		}
		for _, d := range entry.Deltas {
			k := bucketKey{app: entry.AppName, scope: Scope(d.Scope), subject: d.Subject, anonymous: d.Anonymous, dim: Dimension(d.Dimension), start: d.WindowStart, end: d.WindowEnd}
			b := m.bucket(k)
			spent, ok1 := checkedAdd(b.spent, d.Spent)
			reserved, ok2 := checkedAdd(b.reserved, d.Reserved)
			if !ok1 || !ok2 || spent < 0 || reserved < 0 {
				return ErrIntegrity
			}
			b.spent, b.reserved = spent, reserved
		}
		m.seq = max(m.seq, entry.Seq)
	}
	type recovering struct {
		event    *rateEvent
		subjects []Subject
	}
	active := map[string]recovering{}
	for _, e := range state.RateEvents {
		age := effective.Sub(e.AdmittedAt)
		if age < 0 {
			age = 0
		}
		tokens := e.EstimatedTokens
		if e.ActualTokens != nil {
			tokens = *e.ActualTokens
		}
		event := &rateEvent{requestID: e.RequestID, at: elapsed - age, civil: e.AdmittedAt, tokens: tokens, active: e.Active, settled: !e.Active}
		subjects := []Subject{{App: e.AppName}}
		if e.Subject != "" {
			subjects = append(subjects, Subject{App: e.AppName, Hash: e.Subject, Anonymous: e.Anonymous})
		}
		for _, s := range subjects {
			w := m.windows[s]
			if w == nil {
				w = newSlidingWindow()
				m.windows[s] = w
			}
			w.add(event)
		}
		if e.Active {
			active[e.RequestID] = recovering{event, subjects}
		}
	}
	for _, w := range m.windows {
		w.prune(elapsed)
	}
	for _, open := range state.Open {
		r := open.Reservation
		h := &Handle{
			key: handleKey{r.RequestID, r.AttemptIndex}, app: r.AppName, items: r.Items, state: storage.ReservationReserved, finished: true,
			request: Request{RequestID: r.RequestID, AttemptIndex: r.AttemptIndex, Subject: Subject{App: r.AppName, Hash: r.Subject, Anonymous: r.Anonymous},
				Operation: Operation(r.Quote.Operation), EstimatedTokens: r.Quote.EstimatedTokens, ReservedCost: r.Quote.ReservedCost, Generation: r.Quote.Generation},
		}
		if r.AttemptIndex == 1 {
			h.event = active[r.RequestID].event
		}
		transition, usage := recoveredUsage(r, open.Result)
		st := h.settlement(transition, usage, effective)
		seq, err := m.store.SettleReservation(ctx, st)
		if err != nil {
			return err
		}
		m.applySettlement(h, st, seq)
	}
	// Any event still marked active has no reservation left to end it.
	for _, open := range active {
		e := open.event
		if !e.active {
			continue
		}
		for _, s := range open.subjects {
			if w := m.windows[s]; w != nil {
				w.settle(e, e.tokens, true)
			}
		}
		e.active, e.settled = false, true
	}
	if state.Checkpoint == nil {
		if err := m.bootstrap(snapshot, state, effective); err != nil {
			return err
		}
	}
	if snapshot != nil {
		for _, name := range snapshot.AppNames() {
			if p, ok := PolicyFor(snapshot, name); ok {
				m.zoneFor(p, effective)
			}
		}
	}
	for _, b := range m.buckets {
		b.dirty = true
	}
	return m.store.SaveCheckpoint(ctx, m.checkpoint(effective))
}

// recoveredUsage derives the end of an open reservation from the evidence a
// crash left. Without a result, the reserved value is charged.
func recoveredUsage(r storage.Reservation, result *storage.Record) (string, Usage) {
	if result == nil {
		return storage.TransitionSettle, Usage{Cost: r.Quote.ReservedCost}
	}
	if result.Outcome == "blocked" {
		return storage.TransitionRelease, Usage{}
	}
	tokens := int64(0)
	for _, p := range []*int64{result.Input, result.Output, result.CacheWrite, result.CacheRead} {
		if p != nil {
			if sum, ok := checkedAdd(tokens, *p); ok {
				tokens = sum
			} else {
				tokens = math.MaxInt64
			}
		}
	}
	return storage.TransitionSettle, Usage{Cost: result.Cost, Tokens: tokens, Complete: !result.UsageMissing}
}

// bootstrap seeds the current windows from the daily aggregates of an existing
// usage database. A daily row only partly inside a new window is counted in
// full. Token history that cannot be proven blocks that budget until its next
// boundary instead of being read as zero.
func (m *Manager) bootstrap(snapshot *config.Snapshot, state storage.LimitState, at time.Time) error {
	type dayKey struct {
		day, tz, app, scope, subject string
		anonymous                    bool
	}
	missing := map[dayKey]storage.MissingUsage{}
	for _, mu := range state.Missing {
		missing[dayKey{mu.Day, mu.Timezone, mu.AppName, mu.Scope, mu.Subject, mu.Anonymous}] = mu
	}
	for _, d := range state.Daily {
		tz := d.Timezone
		if snapshot != nil {
			if p, ok := PolicyFor(snapshot, d.AppName); ok {
				tz = p.Timezone
			}
		}
		rowLocation, err := loadLocation(d.Timezone)
		if err != nil {
			return ErrIntegrity
		}
		day, err := time.ParseInLocation("2006-01-02", d.Day, time.UTC)
		if err != nil {
			return ErrIntegrity
		}
		span := window{firstInstant(day.Year(), day.Month(), day.Day(), rowLocation), firstInstant(day.Year(), day.Month(), day.Day()+1, rowLocation)}
		s := Subject{App: d.AppName}
		if d.Scope == string(ScopeUser) {
			s = Subject{App: d.AppName, Hash: d.Subject, Anonymous: d.Anonymous}
		}
		tokens := int64(0)
		for _, n := range []int64{d.Input, d.Output, d.CacheWrite, d.CacheRead} {
			sum, ok := checkedAdd(tokens, n)
			if !ok {
				return ErrIntegrity
			}
			tokens = sum
		}
		unknown := false
		if d.UsageMissing > 0 {
			evidence, ok := missing[dayKey{d.Day, d.Timezone, d.AppName, d.Scope, d.Subject, d.Anonymous}]
			if ok && evidence.Count == d.UsageMissing {
				sum, ok := checkedAdd(tokens, evidence.EstimatedTokens)
				if !ok {
					return ErrIntegrity
				}
				tokens = sum
			} else {
				unknown = true
			}
		}
		for _, dim := range calendarDimensions {
			w, err := windowFor(at, dim, tz)
			if err != nil {
				return ErrIntegrity
			}
			if !overlaps(span, w) {
				continue
			}
			b := m.bucket(subjectKey(d.AppName, s, dim, w))
			amount := tokens
			if dim.money() {
				amount = int64(d.Cost)
			} else if unknown {
				b.unknown = true
				m.logger.Warn("token history incomplete; token budget blocked until its next boundary", "app", d.AppName, "dimension", string(dim))
			}
			sum, ok := checkedAdd(b.spent, amount)
			if !ok {
				return ErrIntegrity
			}
			b.spent = sum
		}
	}
	return nil
}

func (m *Manager) gc(civil time.Time, elapsed time.Duration) {
	for s, w := range m.windows {
		w.prune(elapsed)
		if w.empty() {
			delete(m.windows, s)
		}
	}
	now := civil.UnixMicro()
	for k, b := range m.buckets {
		if k.end <= now && b.reserved == 0 {
			if z := m.zones[k.app]; z != nil && z.pending != "" {
				continue
			}
			delete(m.buckets, k)
			m.removed = append(m.removed, k)
		}
	}
}

func (m *Manager) checkpoint(civil time.Time) storage.LimitCheckpoint {
	cp := storage.LimitCheckpoint{
		CoveredSeq: m.seq, CreatedAt: m.clock.Now(), Watermark: civil, RateCutoff: civil.Add(-slidingSpan),
		Metadata: storage.CheckpointMetadata{Version: 1, Zones: map[string]storage.ZoneState{}},
	}
	for app, z := range m.zones {
		state := storage.ZoneState{Active: z.active, Pending: z.pending}
		if z.pending != "" {
			state.ActivateAt = z.activateAt.UnixMicro()
		}
		cp.Metadata.Zones[app] = state
	}
	for k, b := range m.buckets {
		if b.unknown {
			cp.Metadata.Unknown = append(cp.Metadata.Unknown, storage.BucketRef{AppName: k.app, Scope: string(k.scope), Subject: k.subject, Anonymous: k.anonymous, Dimension: string(k.dim), WindowStart: k.start, WindowEnd: k.end})
		}
		if b.dirty {
			cp.Buckets = append(cp.Buckets, storage.BudgetBucket{AppName: k.app, Scope: string(k.scope), Subject: k.subject, Anonymous: k.anonymous, Dimension: string(k.dim), WindowStart: k.start, WindowEnd: k.end, Spent: b.spent, Reserved: b.reserved})
		}
	}
	for _, k := range m.removed {
		cp.Removed = append(cp.Removed, storage.BudgetBucket{AppName: k.app, Scope: string(k.scope), Subject: k.subject, Anonymous: k.anonymous, Dimension: string(k.dim), WindowStart: k.start, WindowEnd: k.end})
	}
	return cp
}

// Tick retries pending settlements, applies due timezone changes and writes a
// checkpoint. A failure marks limit state degraded until a later tick succeeds.
func (m *Manager) Tick(ctx context.Context) error {
	m.mu.Lock()
	civil, elapsed := m.now()
	var failure error
	remaining := m.retry[:0:0]
	for _, p := range m.retry {
		seq, err := m.store.SettleReservation(ctx, p.settlement)
		if err != nil {
			failure = errors.Join(failure, err)
			remaining = append(remaining, p)
			continue
		}
		m.applySettlement(p.handle, p.settlement, seq)
	}
	m.retry = remaining
	for app, z := range m.zones {
		if z.pending != "" && !civil.Before(z.activateAt) {
			m.activate(app, z, civil)
		}
	}
	m.gc(civil, elapsed)
	cp := m.checkpoint(civil)
	if err := m.store.SaveCheckpoint(ctx, cp); err != nil {
		failure = errors.Join(failure, err)
	} else {
		for _, b := range m.buckets {
			b.dirty = false
		}
		m.removed = nil
	}
	if failure != nil {
		m.markDegraded(civil, "checkpoint")
	} else {
		m.markRecovered(civil)
	}
	outbox := m.outbox
	m.outbox = nil
	m.mu.Unlock()
	m.publish(ctx, outbox)
	return failure
}

// publish runs outside the manager lock. Undelivered events keep their IDs and
// are retried, within a bounded queue; the slog warning never depends on it.
func (m *Manager) publish(ctx context.Context, pending []events.Event) {
	if m.publisher == nil || len(pending) == 0 {
		return
	}
	var failed []events.Event
	for _, e := range pending {
		if err := m.publisher.Publish(ctx, e); err != nil {
			failed = append(failed, e)
		}
	}
	if len(failed) == 0 {
		return
	}
	m.mu.Lock()
	for _, e := range failed {
		m.queue(e)
	}
	m.mu.Unlock()
}

// Run writes a checkpoint every interval and once more when ctx ends, as part
// of the ordered shutdown.
func (m *Manager) Run(ctx context.Context) error {
	timer := m.clock.NewTimer(m.interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return m.Tick(shutdown)
		case <-timer.C():
			_ = m.Tick(ctx)
			timer.Reset(m.interval)
		}
	}
}
