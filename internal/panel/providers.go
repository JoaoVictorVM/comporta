package panel

import (
	"context"
	"slices"
	"strconv"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// classify applies the health thresholds over the window: no attempt is
// unknown; below 5% failures is healthy, up to 20% degraded and above that
// unhealthy. Three or more attempts without any success are unhealthy too.
func classify(attempts, successes, failures int64) Health {
	if attempts <= 0 {
		return HealthUnknown
	}
	if attempts >= 3 && successes == 0 {
		return HealthUnhealthy
	}
	switch {
	case failures*100 < attempts*5:
		return HealthHealthy
	case failures*100 <= attempts*20:
		return HealthDegraded
	}
	return HealthUnhealthy
}

func percent(part, whole int64) string {
	if whole <= 0 {
		return "n/a"
	}
	return strconv.FormatFloat(float64(part)*100/float64(whole), 'f', 1, 64) + "%"
}

// Providers reports the observed health of every configured provider over the
// last hour, plus removed providers that still have attempts in the window.
func (s *Service) Providers(ctx context.Context) Providers {
	snapshot := s.config.Capture()
	until := s.now().UTC()
	from := until.Add(-storage.PanelProviderWindow)
	out := Providers{From: from, Until: until}
	var stats []storage.PanelProviderStats
	var buckets []storage.PanelProviderBucket
	err := s.query(ctx, ScreenProviders, func(ctx context.Context) error {
		var err error
		stats, buckets, err = s.store.PanelProviders(ctx, from, until)
		return err
	})
	if err != nil {
		out.Stats = Unavailable
	}
	byName := map[string]storage.PanelProviderStats{}
	for _, st := range stats {
		byName[st.Provider] = st
	}
	names := snapshot.ProviderNames()
	configured := map[string]bool{}
	for _, name := range names {
		configured[name] = true
	}
	for _, st := range stats {
		if !configured[st.Provider] {
			names = append(names, st.Provider)
		}
	}
	slices.Sort(names)
	for _, name := range names {
		st := byName[name]
		card := ProviderCard{Name: name, Removed: !configured[name], Attempts: st.Attempts, Successes: st.Successes, Failures: st.Failures,
			Health: classify(st.Attempts, st.Successes, st.Failures), ErrorRate: percent(st.Failures, st.Attempts), P50MS: "n/a", P95MS: "n/a", CacheReadRatio: "n/a"}
		if out.Stats != Available {
			card.Health = HealthUnknown
		}
		if st.Attempts > 0 {
			card.P50MS, card.P95MS = millis(st.P50LatencyUS), millis(st.P95LatencyUS)
		}
		if denominator := st.InputTokens + st.CacheReadToks; denominator > 0 {
			card.CacheReadRatio = percent(st.CacheReadToks, denominator)
		}
		card.Buckets = make([]ProviderPoint, storage.PanelBuckets)
		for i := range card.Buckets {
			card.Buckets[i].Start = from.Add(storage.PanelBucket * timeIndex(i))
		}
		for _, b := range buckets {
			if b.Provider == name && b.Index >= 0 && b.Index < storage.PanelBuckets {
				card.Buckets[b.Index].Present, card.Buckets[b.Index].Attempts, card.Buckets[b.Index].Failures = true, b.Attempts, b.Failures
			}
		}
		out.Cards = append(out.Cards, card)
	}
	return out
}

func timeIndex(i int) time.Duration { return time.Duration(i) }
