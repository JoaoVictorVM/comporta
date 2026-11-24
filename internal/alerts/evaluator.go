package alerts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/bits"
	"strings"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// Evaluator turns one durable input into zero or more occurrences. It is pure
// over the store and the snapshot it is given: evaluating the same input again
// yields the same keys, which the store deduplicates.
type Evaluator struct {
	Store storage.AlertStore
}

// Input derives the occurrences of one inbox entry.
func (e Evaluator) Input(ctx context.Context, snapshot *config.Snapshot, in storage.AlertInput) ([]storage.AlertOccurrence, error) {
	ev := in.Event
	if ev == nil {
		return nil, nil
	}
	switch ev.Type {
	case events.RequestCompleted:
		return e.completion(ctx, snapshot, *ev)
	case events.Blocked:
		return e.hardCap(ctx, snapshot, *ev)
	case events.LoopCut:
		return loopCut(snapshot, *ev)
	case events.Fallback:
		return e.fallbackBurst(ctx, snapshot, *ev)
	case events.ConfigChanged:
		if ev.Payload.Result != config.ResultInitial && ev.Payload.Result != config.ResultApplied {
			return nil, nil
		}
		return e.Budgets(ctx, snapshot, ev.At)
	case events.ConfigReloadFailed:
		return reloadFailed(*ev)
	case events.LimitStateUnavailable:
		return failOpen(snapshot, *ev)
	}
	return nil, nil
}

// Budgets evaluates the app scope budgets of every configured app at one
// instant. It covers startup, a changed ceiling or thresholds, and settlements
// that reached the store after their completion was evaluated.
func (e Evaluator) Budgets(ctx context.Context, snapshot *config.Snapshot, at time.Time) ([]storage.AlertOccurrence, error) {
	var out []storage.AlertOccurrence
	for _, name := range snapshot.AppNames() {
		found, err := e.budgetScope(ctx, snapshot, name, "app", "", false, at)
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	return out, nil
}

func (e Evaluator) completion(ctx context.Context, snapshot *config.Snapshot, ev events.Event) ([]storage.AlertOccurrence, error) {
	if _, ok := snapshot.App(ev.App); !ok {
		return nil, nil
	}
	out, err := e.budgetScope(ctx, snapshot, ev.App, "app", "", false, ev.At)
	if err != nil {
		return nil, err
	}
	record, err := e.Store.AlertRecord(ctx, ev.RequestID, ev.Payload.AttemptIndex)
	if errors.Is(err, storage.ErrAlertSourceMissing) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	user, err := e.budgetScope(ctx, snapshot, ev.App, "user", record.UserHash, record.Anonymous, ev.At)
	if err != nil {
		return nil, err
	}
	return append(out, user...), nil
}

// budgetScope emits every configured threshold the confirmed spend of a
// bucket already reached. A jump from 49% to 101% produces 50, 80 and 100.
func (e Evaluator) budgetScope(ctx context.Context, snapshot *config.Snapshot, appName, scope, subject string, anonymous bool, at time.Time) ([]storage.AlertOccurrence, error) {
	app, ok := snapshot.App(appName)
	if !ok {
		return nil, nil
	}
	ceilings := app.Budget
	if scope == "user" {
		ceilings = app.PerUser.Budget
	}
	if ceilings == (config.Budget{}) {
		return nil, nil
	}
	thresholds := snapshot.Alerts().Thresholds
	if len(thresholds) == 0 {
		return nil, nil
	}
	buckets, err := e.Store.AlertBudgets(ctx, appName, scope, subject, anonymous, at)
	if err != nil {
		return nil, err
	}
	var out []storage.AlertOccurrence
	for _, b := range buckets {
		limit := ceiling(ceilings, b.Dimension)
		if limit <= 0 || b.Spent <= 0 {
			continue
		}
		start, end := time.UnixMicro(b.WindowStart).UTC(), time.UnixMicro(b.WindowEnd).UTC()
		for _, threshold := range thresholds {
			if !reached(b.Spent, limit, threshold) {
				continue
			}
			key := fmt.Sprintf("%s|%s|%s|%s|%t|%s|%d|%d", TriggerBudget, appName, scope, subject, anonymous, b.Dimension, b.WindowStart, threshold)
			p := Payload{Type: TriggerBudget, OccurredAt: stamp(at), App: appName, Scope: scope, Dimension: b.Dimension, Unit: unitOf(b.Dimension),
				Used: ptr(b.Spent), Limit: ptr(limit), ThresholdPercent: int(threshold), WindowStart: stamp(start), WindowEnd: stamp(end), RenewsAt: stamp(end)}
			o := storage.AlertOccurrence{Trigger: string(TriggerBudget), OccurredAt: at, App: appName, Scope: scope, Dimension: b.Dimension,
				WindowStart: &start, WindowEnd: &end, ThresholdPercent: int(threshold)}
			if scope == "user" {
				p.UserHash = subject
				o.UserHash, o.Anonymous = subject, &anonymous
			}
			occurrence, err := build(key, p, o)
			if err != nil {
				return nil, err
			}
			out = append(out, occurrence)
		}
	}
	return out, nil
}

// reached compares spent/limit >= threshold/100 exactly, in 128 bits.
func reached(spent, limit, threshold int64) bool {
	hi1, lo1 := bits.Mul64(uint64(spent), 100)
	hi2, lo2 := bits.Mul64(uint64(limit), uint64(threshold))
	return hi1 > hi2 || (hi1 == hi2 && lo1 >= lo2)
}

func (e Evaluator) hardCap(ctx context.Context, snapshot *config.Snapshot, ev events.Event) ([]storage.AlertOccurrence, error) {
	if ev.Payload.ErrorCode != "hard_cap_exceeded" || ev.RequestID == "" {
		return nil, nil
	}
	app, ok := snapshot.App(ev.App)
	if !ok {
		return nil, nil
	}
	record, err := e.Store.AlertRecord(ctx, ev.RequestID, 0)
	if errors.Is(err, storage.ErrAlertSourceMissing) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if record.BlockLimitName == "" || record.BlockRenewsAt == nil {
		return nil, nil
	}
	dimension := record.BlockLimitName[strings.LastIndexByte(record.BlockLimitName, '.')+1:]
	if !storage.ValidDimension(dimension) {
		return nil, nil
	}
	scope, subject, anonymous := record.BlockScope, "", false
	ceilings := app.HardCap
	if scope == "user" {
		subject, anonymous, ceilings = record.UserHash, record.Anonymous, app.PerUser.HardCap
	}
	end := record.BlockRenewsAt.UTC()
	p := Payload{Type: TriggerHardCap, OccurredAt: stamp(ev.At), App: ev.App, Scope: scope, Dimension: dimension, Unit: unitOf(dimension), WindowEnd: stamp(end), RenewsAt: stamp(end)}
	o := storage.AlertOccurrence{Trigger: string(TriggerHardCap), OccurredAt: ev.At, App: ev.App, Scope: scope, Dimension: dimension, WindowEnd: &end}
	if limit := ceiling(ceilings, dimension); limit > 0 {
		p.Limit = ptr(limit)
	}
	buckets, err := e.Store.AlertBudgets(ctx, ev.App, scope, subject, anonymous, ev.At)
	if err != nil {
		return nil, err
	}
	for _, b := range buckets {
		if b.Dimension == dimension && b.WindowEnd == end.UnixMicro() {
			start := time.UnixMicro(b.WindowStart).UTC()
			p.Used, p.WindowStart, o.WindowStart = ptr(b.Spent), stamp(start), &start
		}
	}
	if scope == "user" {
		p.UserHash = subject
		o.UserHash, o.Anonymous = subject, &anonymous
	}
	key := fmt.Sprintf("%s|%s|%s|%s|%t|%s|%d", TriggerHardCap, ev.App, scope, subject, anonymous, dimension, end.UnixMicro())
	occurrence, err := build(key, p, o)
	if err != nil {
		return nil, err
	}
	return []storage.AlertOccurrence{occurrence}, nil
}

// loopCut alerts on the first loop of an app in each quarter hour; later loops
// of the same quarter are absorbed.
func loopCut(snapshot *config.Snapshot, ev events.Event) ([]storage.AlertOccurrence, error) {
	if _, ok := snapshot.App(ev.App); !ok || ev.App == "" {
		return nil, nil
	}
	q := quarter(ev.At)
	p := Payload{Type: TriggerLoopCut, OccurredAt: stamp(ev.At), App: ev.App, Scope: "app", Count: ev.Payload.Count}
	o := storage.AlertOccurrence{Trigger: string(TriggerLoopCut), OccurredAt: ev.At, App: ev.App, Scope: "app"}
	if ev.Payload.WindowMS > 0 {
		start := ev.At.Add(-time.Duration(ev.Payload.WindowMS) * time.Millisecond)
		p.WindowStart, p.WindowEnd = stamp(start), stamp(ev.At)
	}
	if ev.UserHash != "" && ev.Anonymous != nil {
		anonymous := *ev.Anonymous
		p.Scope, p.UserHash = "user", ev.UserHash
		o.Scope, o.UserHash, o.Anonymous = "user", ev.UserHash, &anonymous
	}
	occurrence, err := build(fmt.Sprintf("%s|%s|%d", TriggerLoopCut, ev.App, q.UnixMicro()), p, o)
	if err != nil {
		return nil, err
	}
	return []storage.AlertOccurrence{occurrence}, nil
}

// fallbackBurst counts the fallbacks of the app in the moving five minutes that
// end with this one, itself included.
func (e Evaluator) fallbackBurst(ctx context.Context, snapshot *config.Snapshot, ev events.Event) ([]storage.AlertOccurrence, error) {
	if _, ok := snapshot.App(ev.App); !ok || ev.App == "" {
		return nil, nil
	}
	from := ev.At.Add(-FallbackBurstSpan)
	n, err := e.Store.CountAlertEvents(ctx, ev.App, events.Fallback, from, ev.At)
	if err != nil {
		return nil, err
	}
	if n < FallbackBurstSize {
		return nil, nil
	}
	p := Payload{Type: TriggerFallback, OccurredAt: stamp(ev.At), App: ev.App, Scope: "app", Count: n, WindowStart: stamp(from), WindowEnd: stamp(ev.At)}
	o := storage.AlertOccurrence{Trigger: string(TriggerFallback), OccurredAt: ev.At, App: ev.App, Scope: "app"}
	occurrence, err := build(fmt.Sprintf("%s|%s|%d", TriggerFallback, ev.App, quarter(ev.At).UnixMicro()), p, o)
	if err != nil {
		return nil, err
	}
	return []storage.AlertOccurrence{occurrence}, nil
}

// reloadFailed carries the active generation and the number of diagnostics,
// never their text, the YAML or a secret path.
func reloadFailed(ev events.Event) ([]storage.AlertOccurrence, error) {
	p := Payload{Type: TriggerReloadFailed, OccurredAt: stamp(ev.At), Scope: "global", Count: ev.Payload.Count, Generation: ev.Payload.Generation}
	o := storage.AlertOccurrence{Trigger: string(TriggerReloadFailed), OccurredAt: ev.At, Scope: "global"}
	occurrence, err := build(fmt.Sprintf("%s|%d", TriggerReloadFailed, quarter(ev.At).UnixMicro()), p, o)
	if err != nil {
		return nil, err
	}
	return []storage.AlertOccurrence{occurrence}, nil
}

// failOpen alerts only for apps that admit requests while limits are
// unverifiable. The global event without an app is not delivered.
func failOpen(snapshot *config.Snapshot, ev events.Event) ([]storage.AlertOccurrence, error) {
	if ev.App == "" {
		return nil, nil
	}
	app, ok := snapshot.App(ev.App)
	if !ok || app.FailMode != config.FailOpen {
		return nil, nil
	}
	since := ev.At.Add(-time.Duration(ev.Payload.DegradedSeconds) * time.Second)
	p := Payload{Type: TriggerFailOpen, OccurredAt: stamp(ev.At), App: ev.App, Scope: "app", WindowStart: stamp(since)}
	o := storage.AlertOccurrence{Trigger: string(TriggerFailOpen), OccurredAt: ev.At, App: ev.App, Scope: "app"}
	occurrence, err := build(fmt.Sprintf("%s|%s|%d", TriggerFailOpen, ev.App, quarter(ev.At).UnixMicro()), p, o)
	if err != nil {
		return nil, err
	}
	return []storage.AlertOccurrence{occurrence}, nil
}

// build seals an occurrence: its ID comes from the key and the payload carries
// the same ID, so the projection is identical on every evaluation.
func build(key string, p Payload, o storage.AlertOccurrence) (storage.AlertOccurrence, error) {
	o.DedupeKey = key
	o.ID = storage.OccurrenceID(key)
	p.ID = o.ID
	data, err := json.Marshal(p)
	if err != nil {
		return o, storage.ErrInvalid
	}
	o.Payload = data
	return o, storage.ValidateOccurrence(o)
}
