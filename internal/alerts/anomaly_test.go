package alerts

import (
	"context"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// usageStore answers only the five minute aggregate; any other call panics
// through the nil interface, which proves the rule reads nothing else.
type usageStore struct {
	storage.AlertStore
	buckets []storage.UsageBucket
}

func (u usageStore) UsageBuckets(_ context.Context, from, until time.Time) ([]storage.UsageBucket, error) {
	var out []storage.UsageBucket
	for _, b := range u.buckets {
		if !b.Start.Before(from) && b.Start.Before(until) {
			out = append(out, b)
		}
	}
	return out, nil
}

var evaluation = time.Date(2026, 11, 23, 18, 30, 0, 0, time.UTC)

// history spreads hourly spend evenly over the 168 hours before the last hour,
// one bucket per hour, and the last hour's spend over its twelve buckets.
func history(app string, hourly, lastHour int64) []storage.UsageBucket {
	var out []storage.UsageBucket
	recent := evaluation.Add(-time.Hour)
	for h := 1; h <= AnomalyBaselineHrs; h++ {
		if hourly > 0 {
			out = append(out, storage.UsageBucket{Start: recent.Add(-time.Duration(h) * time.Hour), App: app, Cost: hourly})
		}
	}
	for i := range 12 {
		share := lastHour / 12
		if i == 0 {
			share += lastHour % 12
		}
		out = append(out, storage.UsageBucket{Start: recent.Add(time.Duration(i) * AnomalyStep), App: app, Cost: share})
	}
	return out
}

func anomalies(t *testing.T, buckets []storage.UsageBucket, at time.Time) []Payload {
	t.Helper()
	e := newEnv(t, defaults, nil)
	evaluator := Evaluator{Store: usageStore{buckets: buckets}}
	found, err := evaluator.Anomalies(context.Background(), e.manager.Capture(), at)
	if err != nil {
		t.Fatal(err)
	}
	var out []Payload
	for _, o := range found {
		p, _ := DecodePayload(o.Payload)
		out = append(out, p)
	}
	return out
}

func TestAnomalyFiveTimesBaseline(t *testing.T) {
	got := anomalies(t, history("bot", 100_000, 600_000), evaluation)
	if len(got) != 1 || got[0].App != "bot" || *got[0].BaselineMicrousd != 100_000 || *got[0].LastHourMicrousd != 600_000 || got[0].Multiplier != 5 {
		t.Fatalf("anomaly %+v", got)
	}
	if got := anomalies(t, history("bot", 100_000, 500_000), evaluation); len(got) != 0 {
		t.Fatal("exactly five times the baseline fired")
	}
	// Hours without spend count in the average: half of them empty halves it.
	sparse := history("bot", 0, 300_000)
	recent := evaluation.Add(-time.Hour)
	for h := 1; h <= 84; h++ {
		sparse = append(sparse, storage.UsageBucket{Start: recent.Add(-time.Duration(2*h) * time.Hour), App: "bot", Cost: 200_000})
	}
	if got := anomalies(t, sparse, evaluation); len(got) != 0 {
		t.Fatal("below the minimum spend fired")
	}
}

func TestAnomalyMinimumSpend(t *testing.T) {
	if got := anomalies(t, history("bot", 10_000, 499_999), evaluation); len(got) != 0 {
		t.Fatal("spend under US$0.50 fired")
	}
	if got := anomalies(t, history("bot", 10_000, 500_000), evaluation); len(got) != 1 {
		t.Fatal("spend of exactly US$0.50 and 50x the baseline did not fire")
	}
}

func TestAnomalyZeroBaseline(t *testing.T) {
	if got := anomalies(t, history("bot", 0, 500_000), evaluation); len(got) != 1 || *got[0].BaselineMicrousd != 0 {
		t.Fatal("a new app reaching the minimum did not fire")
	}
	if got := anomalies(t, history("strict", 0, 0), evaluation); len(got) != 0 {
		t.Fatal("an app without spend fired")
	}
	if got := anomalies(t, history("removed", 0, 900_000), evaluation); len(got) != 0 {
		t.Fatal("an app absent from the configuration fired")
	}
}

func TestAnomalyQuarterDedup(t *testing.T) {
	e := newEnv(t, defaults, nil)
	evaluator := Evaluator{Store: usageStore{buckets: history("bot", 0, 900_000)}}
	keys := map[string]bool{}
	for _, at := range []time.Time{evaluation, evaluation.Add(5 * time.Minute), evaluation.Add(10 * time.Minute), evaluation.Add(15 * time.Minute)} {
		found, err := evaluator.Anomalies(context.Background(), e.manager.Capture(), at.Add(2*time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range found {
			keys[o.DedupeKey] = true
		}
	}
	if len(keys) != 2 {
		t.Fatalf("anomaly keys %v, want one per quarter hour", keys)
	}
}
