package metering

import (
	"context"
	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/limits"
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
	admission, reject := s.Admit(ctx, snapshot, e, "test", provider)
	complete := admission.Complete
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
	admission, reject := s.Admit(context.Background(), snapshot, e, "test", p)
	complete := admission.Complete
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
	admission, reject := s.Admit(context.Background(), snapshot, e, "test", p)
	complete := admission.Complete
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

// policySetup connects a limit coordinator to an app whose first request
// already exceeds its budget: main reserves 8500 microusd, the budget is 1000.
func policySetup(t *testing.T, policy string) (*Service, *sqlite.Store, *config.Snapshot, *proxy.Envelope) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "comporta.yaml")
	data := `timezone: UTC
providers:
  test:
    api_format: anthropic
    base_url: http://localhost:12345
    models: [main, cheap]
apps:
  bot:
    allowed_models: [main, cheap]
` + policy + `
pricing:
  main: {input: 20, output: 60, cache_write: 25, cache_read: 2}
  cheap: {input: 2, output: 6, cache_write: 2.5, cache_read: 0.2}
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
	coordinator := limits.New(limits.Options{Store: store})
	if err = coordinator.Recover(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	if err = service.UseLimits(coordinator, store, manager); err != nil {
		t.Fatal(err)
	}
	envelope := &proxy.Envelope{RequestID: "0123456789abcdef0123456789abcdef", AppName: "bot", Operation: proxy.OperationGeneration, ModelRequested: "main", ModelEffective: "main", Identity: identity, BodySizeOriginal: 300, BodySizeEffective: 300, MaxTokens: known(100)}
	return service, store, snapshot, envelope
}

const exceededBudget = "    budget: {usd_per_day: 0.001}\n"

func candidates(snapshot *config.Snapshot, e *proxy.Envelope, withTarget bool) []proxy.Candidate {
	p, _ := snapshot.Provider("test")
	out := []proxy.Candidate{{ID: proxy.CandidateOriginal, Model: e.ModelEffective, ProviderName: "test", Provider: p, BodySizeEffective: e.BodySizeEffective}}
	if withTarget {
		out = append(out, proxy.Candidate{ID: proxy.CandidateDowngrade, Model: "cheap", ProviderName: "test", Provider: p, BodySizeEffective: e.BodySizeEffective - 3})
	}
	return out
}

func eventsOf(t *testing.T, store *sqlite.Store, kind events.Type) []events.Event {
	t.Helper()
	page, err := store.ReadEvents(context.Background(), events.Filter{Types: []events.Type{kind}, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	return page.Events
}

func onlyRecord(t *testing.T, s *Service, store *sqlite.Store) storage.Record {
	t.Helper()
	if err := s.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	page, err := store.Records(context.Background(), storage.RecordFilter{AppName: "bot", Limit: 10})
	if err != nil || len(page.Records) != 1 {
		t.Fatalf("records: %+v %v", page.Records, err)
	}
	return page.Records[0]
}

func TestWarnRetainsUsageAndErrors(t *testing.T) {
	s, store, snapshot, e := policySetup(t, exceededBudget+"    on_limit: warn")
	admission, reject := s.AdmitCandidates(context.Background(), snapshot, e, candidates(snapshot, e, false))
	if reject != nil || admission.Policy != proxy.PolicyWarned || admission.Selected != proxy.CandidateOriginal {
		t.Fatalf("warn did not admit: %+v %+v", admission, reject)
	}
	// The event is durable at admission, before anything is sent.
	warned := eventsOf(t, store, events.Warned)
	if len(warned) != 1 || warned[0].Payload.LimitName != "apps.bot.budget.usd_per_day" || warned[0].Payload.ModelUsed != "main" || warned[0].RequestID != e.RequestID {
		t.Fatalf("warned event: %+v", warned)
	}
	admission.Complete(proxy.AttemptResult{Operation: proxy.OperationGeneration, UpstreamStatus: proxy.Optional[int]{Value: 500, Known: true},
		ErrorClass: proxy.ClassUpstream5xx, ErrorCode: proxy.CodeProviderError, UsageComplete: true, Usage: proxy.Usage{Input: known(10), Output: known(0), CacheWrite: known(0), CacheRead: known(0)}})
	r := onlyRecord(t, s, store)
	if r.PolicyOutcome != "warned" || r.Outcome != "ok" || r.ErrorCode != "provider_error" || r.Input == nil || *r.Input != 10 || r.PolicyLimitName != "apps.bot.budget.usd_per_day" || r.PolicyRenewsAt == nil {
		t.Fatalf("warned record lost usage or error: %+v", r)
	}
}

func TestDowngradeSettlesSelectedPrice(t *testing.T) {
	s, store, snapshot, e := policySetup(t, exceededBudget+"    on_limit: downgrade\n    downgrade_to: cheap")
	admission, reject := s.AdmitCandidates(context.Background(), snapshot, e, candidates(snapshot, e, true))
	if reject != nil || admission.Policy != proxy.PolicyDowngraded || admission.Selected != proxy.CandidateDowngrade {
		t.Fatalf("downgrade did not admit the target: %+v %+v", admission, reject)
	}
	pending, err := store.Pending(context.Background())
	if err != nil || len(pending) != 1 || pending[0].Admission.ModelUsed != "cheap" || pending[0].Admission.ModelRequested != "main" || pending[0].Admission.ReservedCost >= 8500 {
		t.Fatalf("journal froze the wrong candidate: %+v %v", pending, err)
	}
	if got := eventsOf(t, store, events.Downgraded); len(got) != 1 || got[0].Payload.ModelRequested != "main" || got[0].Payload.ModelUsed != "cheap" {
		t.Fatalf("downgraded event: %+v", got)
	}
	admission.Complete(proxy.AttemptResult{Operation: proxy.OperationGeneration, UsageComplete: true, Usage: proxy.Usage{Input: known(1_000_000), Output: known(0), CacheWrite: known(0), CacheRead: known(0)}})
	r := onlyRecord(t, s, store)
	// One million input tokens at the target's price of 2 USD per million.
	if r.Outcome != "downgraded" || r.PolicyOutcome != "downgraded" || r.ModelUsed != "cheap" || r.Cost != 2_000_000 {
		t.Fatalf("settled at the wrong price: %+v", r)
	}
}

func TestRejectedCandidateLeavesNoJournal(t *testing.T) {
	s, store, snapshot, e := policySetup(t, exceededBudget+"    on_limit: downgrade\n    downgrade_to: cheap")
	options := candidates(snapshot, e, true)
	options[1].Unavailable = &proxy.Rejection{Status: 400, Code: proxy.CodeProviderFormatMismatch, Class: proxy.ClassInvalidRequest, Message: "target in another API format"}
	_, reject := s.AdmitCandidates(context.Background(), snapshot, e, options)
	if reject == nil || reject.Code != proxy.CodeProviderFormatMismatch {
		t.Fatalf("unavailable target admitted: %+v", reject)
	}
	pending, err := store.Pending(context.Background())
	if err != nil || len(pending) != 0 {
		t.Fatalf("a refused request left a journal entry: %+v %v", pending, err)
	}
	r := onlyRecord(t, s, store)
	if r.AttemptIndex != 0 || r.Outcome != "blocked" || r.PolicyOutcome != "blocked" || r.ErrorCode != "provider_format_mismatch" || r.Cost != 0 {
		t.Fatalf("block record: %+v", r)
	}
	if len(eventsOf(t, store, events.Downgraded)) != 0 {
		t.Fatal("a refused downgrade produced a downgraded event")
	}

	// Under budget, the target never reaches the journal.
	s, store, snapshot, e = policySetup(t, "    budget: {usd_per_day: 1.00}\n    on_limit: downgrade\n    downgrade_to: cheap")
	admission, reject := s.AdmitCandidates(context.Background(), snapshot, e, candidates(snapshot, e, true))
	if reject != nil || admission.Policy != proxy.PolicyNone || admission.Selected != proxy.CandidateOriginal {
		t.Fatalf("under budget: %+v %+v", admission, reject)
	}
	if pending, _ := store.Pending(context.Background()); len(pending) != 1 || pending[0].Admission.ModelUsed != "main" || pending[0].Admission.PolicyOutcome != "" {
		t.Fatalf("journal: %+v", pending)
	}
}
