package metering

import (
	"context"
	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/pricing"
	"github.com/JoaoVictorVM/comporta/internal/proxy"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/storage/sqlite"
	"github.com/JoaoVictorVM/comporta/internal/useridentity"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func setup(t *testing.T) (*Service, *sqlite.Store, *config.Snapshot, *proxy.Envelope) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "comporta.yaml")
	data := `timezone: UTC
providers:
  test:
    api_format: anthropic
    base_url: http://localhost:12345
    models: [fixture, unknown]
apps:
  bot: {allowed_models: [fixture, unknown]}
pricing:
  fixture: {input: 2, output: 6, cache_write: 2.5, cache_read: 0.2}
`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	manager, err := config.NewManager(config.ManagerOptions{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	snapshot := manager.Capture()
	id, err := useridentity.Open(context.Background(), useridentity.Options{Dir: snapshot.DataDir()})
	if err != nil {
		t.Fatal(err)
	}
	identity, err := id.Resolve("bot", "opaque-user", true, false)
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlite.Open(context.Background(), snapshot.DataDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	catalog, err := pricing.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	service := New(store, catalog, slog.New(slog.DiscardHandler), func() int64 { return 30 })
	envelope := &proxy.Envelope{RequestID: "0123456789abcdef0123456789abcdef", AppName: "bot", Operation: proxy.OperationGeneration, ModelRequested: "fixture", ModelEffective: "fixture", Identity: identity, BodySizeOriginal: 300, BodySizeEffective: 300, MaxTokens: proxy.Optional[int64]{Value: 100, Known: true}}
	return service, store, snapshot, envelope
}

func known(n int64) proxy.Optional[int64] { return proxy.Optional[int64]{Value: n, Known: true} }

func TestOverrideProducesExactPricedRecord(t *testing.T) {
	s, store, snapshot, e := setup(t)
	ctx := context.Background()
	provider, _ := snapshot.Provider("test")
	complete, reject := s.Admit(ctx, snapshot, e, "test", provider)
	if reject != nil {
		t.Fatal(reject)
	}
	pending, err := store.Pending(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("admission not durable: %v", err)
	}
	result := proxy.AttemptResult{Operation: proxy.OperationGeneration, UsageComplete: true, Usage: proxy.Usage{Input: known(10), Output: known(3), CacheWrite: known(4), CacheRead: known(5)}}
	complete(result)
	complete(result)
	if err = s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := store.Daily(ctx, storage.Filter{AppName: "bot", UserHash: e.Identity.Hash().Hex()})
	if err != nil || len(rows) != 1 || rows[0].Cost != 49 || rows[0].Requests != 1 {
		t.Fatalf("%+v %v", rows, err)
	}
	if len(s.slots) != 0 {
		t.Fatal("slot not released after commit")
	}
}

func TestMissingUsageUsesFrozenConservativeQuote(t *testing.T) {
	s, store, snapshot, e := setup(t)
	p, _ := snapshot.Provider("test")
	complete, reject := s.Admit(context.Background(), snapshot, e, "test", p)
	if reject != nil {
		t.Fatal(reject)
	}
	complete(proxy.AttemptResult{Operation: proxy.OperationGeneration})
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := store.Daily(context.Background(), storage.Filter{AppName: "bot"})
	if err != nil || len(rows) != 1 || rows[0].Cost != 850 || rows[0].UsageMissing != 1 {
		t.Fatalf("%+v %v", rows, err)
	}
}

func TestUnknownPriceAndCapacityRejectBeforeAdmission(t *testing.T) {
	s, store, snapshot, e := setup(t)
	p, _ := snapshot.Provider("test")
	e.ModelEffective = "unknown"
	_, reject := s.Admit(context.Background(), snapshot, e, "test", p)
	if reject == nil || reject.Status != 400 {
		t.Fatal(reject)
	}
	e.ModelEffective = "fixture"
	for range cap(s.slots) {
		s.slots <- struct{}{}
	}
	_, reject = s.Admit(context.Background(), snapshot, e, "test", p)
	if reject == nil || reject.Status != 503 {
		t.Fatal(reject)
	}
	pending, err := store.Pending(context.Background())
	if err != nil || len(pending) != 0 {
		t.Fatal("rejected request admitted")
	}
}

func TestCountTokensNeverCreatesGenerationSpend(t *testing.T) {
	s, store, snapshot, e := setup(t)
	p, _ := snapshot.Provider("test")
	e.Operation = proxy.OperationCountTokens
	e.ModelEffective = "unknown"
	complete, reject := s.Admit(context.Background(), snapshot, e, "test", p)
	if reject != nil {
		t.Fatal(reject)
	}
	complete(proxy.AttemptResult{Operation: proxy.OperationCountTokens, CountedInputTokens: known(100)})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := store.Daily(ctx, storage.Filter{AppName: "bot"})
	if err != nil || len(rows) != 1 || rows[0].Cost != 0 || rows[0].Input != 0 {
		t.Fatalf("%+v %v", rows, err)
	}
}
