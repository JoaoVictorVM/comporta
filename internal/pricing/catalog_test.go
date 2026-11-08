package pricing

import (
	"github.com/JoaoVictorVM/comporta/internal/config"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOverrideReplacesAllTiersAndLocalProviderRules(t *testing.T) {
	c, err := Embedded()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "comporta.yaml")
	text := `timezone: UTC
providers:
  test:
    api_format: anthropic
    base_url: http://localhost:12345
    models: [claude-haiku-5-5]
apps:
  bot: {allowed_models: [claude-haiku-5-5]}
pricing:
  claude-haiku-5-5: {input: 0.000001, output: 0.000002, cache_write: 0.000003, cache_read: 0.000004}
`
	if err = os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	manager, err := config.NewManager(config.ManagerOptions{Path: path, Options: config.Options{Catalog: c}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := manager.Capture()
	p, _ := snapshot.Provider("test")
	q, err := c.Resolve(snapshot, p, "claude-haiku-5-5")
	if err != nil {
		t.Fatal(err)
	}
	if q.Price(1000000) != (config.Price{Input: 1, Output: 2, CacheWrite: 3, CacheRead: 4}) || q.threshold != 0 || q.write1h != 3 {
		t.Fatal("override retained embedded dimensions or tiers")
	}
	if _, err = c.Resolve(snapshot, p, "unlisted"); err != ErrUnknownPrice {
		t.Fatal("Anthropic local provider made free")
	}
	p.APIFormat = config.FormatOpenAI
	q, err = c.Resolve(snapshot, p, "unlisted")
	if err != nil || q.ConservativePrice() != (config.Price{}) {
		t.Fatal("OpenAI-compatible local provider not free")
	}
}

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

func TestCacheWriteTTLAndOverrideRates(t *testing.T) {
	q := Quote{price: config.Price{Input: 2000000, Output: 6000000, CacheWrite: 2500000, CacheRead: 200000}, write1h: 4000000}
	five, hour := int64(2), int64(2)
	tokens := Tokens{10, 3, 4, 5}
	got, err := q.Cost(tokens, &five, &hour)
	if err != nil || got != 52 {
		t.Fatalf("mixed TTL cost %d %v", got, err)
	}
	got, err = q.Cost(tokens, nil, nil)
	if err != nil || got != 55 {
		t.Fatalf("unknown TTL cost %d %v", got, err)
	}
	q.write1h = q.price.CacheWrite
	got, err = q.Cost(tokens, &five, &hour)
	if err != nil || got != 49 {
		t.Fatalf("flat override cost %d %v", got, err)
	}
}
