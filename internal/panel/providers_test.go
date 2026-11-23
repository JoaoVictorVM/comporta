package panel

import (
	"context"
	"testing"
	"time"
)

func TestProviderHealthThresholds(t *testing.T) {
	for _, c := range []struct {
		attempts, successes, failures int64
		want                          Health
	}{
		{0, 0, 0, HealthUnknown},
		{100, 96, 4, HealthHealthy},
		{100, 95, 5, HealthDegraded},
		{100, 80, 20, HealthDegraded},
		{100, 79, 21, HealthUnhealthy},
		{3, 0, 0, HealthUnhealthy},
		{2, 0, 0, HealthHealthy},
		{10, 0, 1, HealthUnhealthy},
	} {
		if got := classify(c.attempts, c.successes, c.failures); got != c.want {
			t.Fatalf("%+v: got %s", c, got)
		}
	}
}

func (e *fixtureEnv) attempt(t *testing.T, n int, app string, latencyMS int64, class string) {
	t.Helper()
	r := e.record(t, n, app, "alice", e.now.Add(-time.Duration(n)*time.Minute/4), 10)
	r.LatencyUS, r.ErrorClass = latencyMS*1000, class
	e.commit(t, r)
}

func TestProviderPercentiles(t *testing.T) {
	env := newEnv(t)
	for n := 1; n <= 10; n++ {
		env.attempt(t, n, "bot", int64(n*100), "")
	}
	p := env.service.Providers(context.Background())
	card := p.Cards[0]
	if card.Name != "anthropic" || card.Attempts != 10 || card.P50MS != "500.0" || card.P95MS != "1000.0" || card.Health != HealthHealthy {
		t.Fatalf("card: %+v", card)
	}
	present := 0
	for _, b := range card.Buckets {
		if b.Present {
			present++
		}
	}
	if len(card.Buckets) != 12 || present == 0 || present == 12 {
		t.Fatalf("buckets: %d present of %d", present, len(card.Buckets))
	}
}

func TestCacheReadRatio(t *testing.T) {
	env := newEnv(t)
	r := env.record(t, 1, "bot", "alice", env.now.Add(-time.Minute), 10)
	read := int64(300)
	r.CacheRead = &read
	env.commit(t, r)
	card := env.service.Providers(context.Background()).Cards[0]
	// 300 read from cache out of 100 input plus 300 read.
	if card.CacheReadRatio != "75.0%" {
		t.Fatalf("ratio: %s", card.CacheReadRatio)
	}
}

func TestNoTrafficIsUnknown(t *testing.T) {
	env := newEnv(t)
	env.attempt(t, 1, "bot", 100, "overload")
	p := env.service.Providers(context.Background())
	if len(p.Cards) != 2 || p.Cards[1].Name != "openai" || p.Cards[1].Health != HealthUnknown || p.Cards[1].ErrorRate != "n/a" {
		t.Fatalf("cards: %+v", p.Cards)
	}
	if p.Cards[0].Health != HealthUnhealthy || p.Cards[0].ErrorRate != "100.0%" {
		t.Fatalf("failing provider: %+v", p.Cards[0])
	}
}
