package pricing

import (
	"github.com/JoaoVictorVM/comporta/internal/config"
	"testing"
	"time"
)

func TestCatalogDateBoundary(t *testing.T) {
	c, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		delta   time.Duration
		old     bool
		invalid bool
	}{{-1, false, true}, {60 * 24 * time.Hour, false, false}, {60*24*time.Hour + time.Second, true, false}} {
		old, err := c.Check(c.VerifiedAt().Add(tc.delta))
		if old != tc.old || (err != nil) != tc.invalid {
			t.Fatalf("%+v: %v %v", tc, old, err)
		}
	}
}

func TestExactModelAliasesOnly(t *testing.T) {
	c, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	if !c.HasPrice("claude-haiku-5-5") || c.HasPrice("claude-haiku-5-5-invented") {
		t.Fatal("model matching")
	}
}

func TestLongContextTierBoundaries(t *testing.T) {
	q := Quote{price: config.Price{Input: 1}, high: config.Price{Input: 5}, threshold: 100000}
	for _, tc := range []struct {
		input int64
		rate  config.Microusd
	}{{99999, 1}, {100000, 1}, {100001, 5}} {
		if q.Price(tc.input).Input != tc.rate {
			t.Fatal(tc)
		}
	}
}

func TestConservativeQuoteIncludesCacheWriteAndHighTier(t *testing.T) {
	q := Quote{price: config.Price{Input: 1, Output: 2, CacheWrite: 3}, write1h: 4, threshold: 100, high: config.Price{Input: 5, Output: 6}, highWrite1h: 10}
	if got := q.ConservativePrice(); got.Input != 10 || got.Output != 6 {
		t.Fatal(got)
	}
}
