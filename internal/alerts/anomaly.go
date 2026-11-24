package alerts

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// Anomalies evaluates every configured app at an instant aligned to five
// minutes. The last hour is the twelve closed buckets before it; the baseline
// is the seven days before that hour divided by 168, hours without spend
// included. An app fires when its last hour reaches the minimum spend and is
// strictly greater than the multiplier times the hourly average; with a zero
// baseline, reaching the minimum is enough.
func (e Evaluator) Anomalies(ctx context.Context, snapshot *config.Snapshot, at time.Time) ([]storage.AlertOccurrence, error) {
	at = at.UTC().Truncate(AnomalyStep)
	recentStart := at.Add(-AnomalyRecent)
	baselineStart := recentStart.Add(-AnomalyBaseline)
	buckets, err := e.Store.UsageBuckets(ctx, baselineStart, at)
	if err != nil {
		return nil, err
	}
	recent, baseline := map[string]int64{}, map[string]int64{}
	for _, b := range buckets {
		if b.Start.Before(recentStart) {
			baseline[b.App] += b.Cost
		} else {
			recent[b.App] += b.Cost
		}
	}
	settings := snapshot.Alerts()
	var out []storage.AlertOccurrence
	for _, app := range snapshot.AppNames() {
		last, total := recent[app], baseline[app]
		if !anomalous(last, total, settings.AnomalyMultiplier, int64(settings.AnomalyMinUSD)) {
			continue
		}
		average := total / AnomalyBaselineHrs
		p := Payload{Type: TriggerAnomaly, OccurredAt: stamp(at), App: app, Scope: "app", Unit: "microusd", Multiplier: settings.AnomalyMultiplier,
			BaselineMicrousd: ptr(average), LastHourMicrousd: ptr(last), WindowStart: stamp(recentStart), WindowEnd: stamp(at)}
		o := storage.AlertOccurrence{Trigger: string(TriggerAnomaly), OccurredAt: at, App: app, Scope: "app", WindowStart: &recentStart, WindowEnd: &at}
		occurrence, err := build(fmt.Sprintf("%s|%s|%d", TriggerAnomaly, app, quarter(at).UnixMicro()), p, o)
		if err != nil {
			return nil, err
		}
		out = append(out, occurrence)
	}
	return out, nil
}

// anomalous compares last > multiplier * total / 168 without division or
// overflow: last * 168 > multiplier * total.
func anomalous(last, total, multiplier, minimum int64) bool {
	if last <= 0 || last < minimum {
		return false
	}
	left := new(big.Int).Mul(big.NewInt(last), big.NewInt(AnomalyBaselineHrs))
	right := new(big.Int).Mul(big.NewInt(multiplier), big.NewInt(total))
	return left.Cmp(right) > 0
}
