package integration

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/limits"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/storage/sqlite"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

// brokenCheckpoints fails only the limit snapshot. Admissions, journal and
// usage records keep working, which isolates the failure to limits.
type brokenCheckpoints struct {
	*sqlite.Store
	broken atomic.Bool
}

func (b *brokenCheckpoints) SaveCheckpoint(ctx context.Context, cp storage.LimitCheckpoint) error {
	if b.broken.Load() {
		return storage.ErrUnavailable
	}
	return b.Store.SaveCheckpoint(ctx, cp)
}

func degradedGateway(t *testing.T, mode string, calls *atomic.Int64) (*gateway, *brokenCheckpoints) {
	t.Helper()
	var broken *brokenCheckpoints
	gw := startConfiguredGateway(t, fakeprovider.Options{OnRequest: func(fakeprovider.Request) { calls.Add(1) }}, gatewayOptions{
		metered: true,
		edit:    withBot("    fail_mode: " + mode + "\n    budget: {usd_per_day: 1.00}\n"),
		wrap: func(s *sqlite.Store) storage.LimitStore {
			broken = &brokenCheckpoints{Store: s}
			return broken
		},
	})
	broken.broken.Store(true)
	if err := gw.limits.Tick(context.Background()); err == nil {
		t.Fatal("injected checkpoint failure not reported")
	}
	return gw, broken
}

func (g *gateway) eventTypes(app string) []events.Type {
	g.t.Helper()
	page, err := g.usage.ReadEvents(context.Background(), events.Filter{App: app, Types: []events.Type{events.LimitStateUnavailable, events.LimitStateRecovered}, Limit: 100})
	if err != nil {
		g.t.Fatal(err)
	}
	var out []events.Type
	for _, e := range page.Events {
		if e.Payload.LimitName != "" || strings.Contains(e.Payload.Reason, "1") {
			g.t.Fatal("unexpected payload")
		}
		out = append(out, e.Type)
	}
	return out
}

func TestUnavailableLimitStateFailClosed503(t *testing.T) {
	var calls atomic.Int64
	gw, _ := degradedGateway(t, "fail_closed", &calls)
	key := gw.createKey("bot").Key.Reveal()
	for _, path := range []string{"/v1/chat/completions", "/v1/messages"} {
		body := testkit.OpenAIBody()
		if path == "/v1/messages" {
			body = testkit.AnthropicBody()
		}
		response, answer := gw.call(path, key, body)
		decoded := decodeLimitError(t, answer)
		if response.StatusCode != 503 || decoded.Error.Code != "limits_unavailable" || decoded.Error.Message != "limits cannot be verified; request rejected (fail_closed)" {
			t.Fatalf("%s: %d %s", path, response.StatusCode, answer)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("fail_closed reached the provider")
	}
}

func TestIsolatedLimitsFailureFailOpenWithAlertEvent(t *testing.T) {
	var calls atomic.Int64
	gw, _ := degradedGateway(t, "fail_open", &calls)
	key := gw.createKey("bot").Key.Reveal()
	response, body := gw.call("/v1/chat/completions", key, testkit.OpenAIBody())
	if response.StatusCode != 200 || calls.Load() != 1 {
		t.Fatalf("fail_open refused: %d %s", response.StatusCode, body)
	}
	if response.Header.Get("X-Comporta-Budget-Remaining") != "" {
		t.Fatal("budget headers sent while limit state is unverifiable")
	}
	// The next tick delivers the queued events through the healthy event store.
	_ = gw.limits.Tick(context.Background())
	got := gw.eventTypes("bot")
	if len(got) != 1 || got[0] != events.LimitStateUnavailable {
		t.Fatalf("alert event for the app: %v", got)
	}
}

func TestFailOpenDoesNotBypassMeteringQueue(t *testing.T) {
	var calls atomic.Int64
	gw, _ := degradedGateway(t, "fail_open", &calls)
	key := gw.createKey("bot").Key.Reveal()
	gw.usage.Close()
	response, body := gw.call("/v1/chat/completions", key, testkit.OpenAIBody())
	if response.StatusCode != 503 || !strings.Contains(body, "usage_storage_unavailable") || calls.Load() != 0 {
		t.Fatalf("fail_open bypassed the usage journal: %d %s", response.StatusCode, body)
	}
}

func TestRecoveryReconcilesDegradedRequests(t *testing.T) {
	var calls atomic.Int64
	gw, broken := degradedGateway(t, "fail_open", &calls)
	key := gw.createKey("bot").Key.Reveal()
	for range 3 {
		if response, _ := gw.call("/v1/chat/completions", key, testkit.OpenAIBody()); response.StatusCode != 200 {
			t.Fatal("fail_open refused")
		}
	}
	gw.results.waitCount(t, 3)
	broken.broken.Store(false)
	if err := gw.limits.Tick(context.Background()); err != nil || gw.limits.Degraded() {
		t.Fatalf("recovery: %v", err)
	}
	if got := gw.eventTypes("bot"); len(got) != 2 || got[1] != events.LimitStateRecovered {
		t.Fatalf("recovery event: %v", got)
	}
	cost := gw.dailyCost(storage.Filter{})
	if v := gw.limitView(nil, "apps.bot.budget.usd_per_day"); v.Used != cost || v.Reserved != 0 {
		t.Fatalf("degraded consumption missing from limits: %+v vs %d", v, cost)
	}
	page, err := gw.usage.Records(context.Background(), storage.RecordFilter{AppName: "bot", Limit: 10})
	if err != nil || len(page.Records) != 3 {
		t.Fatal(err)
	}
	for _, r := range page.Records {
		if !r.LimitsDegraded {
			t.Fatal("degraded admission not marked on its record")
		}
	}
	// A restart rebuilds the same state from the checkpoint and ledger.
	restarted := limits.New(limits.Options{Store: gw.usage})
	if err := restarted.Recover(context.Background(), gw.config.Capture()); err != nil {
		t.Fatal(err)
	}
	policy, _ := limits.PolicyFor(gw.config.Capture(), "bot")
	for _, v := range restarted.State(policy, nil).Limits {
		if v.Dimension == limits.USDPerDay && v.Used != cost {
			t.Fatalf("restart lost degraded consumption: %+v", v)
		}
	}
}

func TestUserConcurrencyAboveLimitReturns429(t *testing.T) {
	entered, release := make(chan struct{}, 8), make(chan struct{})
	gw := startConfiguredGateway(t, fakeprovider.Options{OnRequest: func(fakeprovider.Request) {
		entered <- struct{}{}
		<-release
	}}, gatewayOptions{metered: true})
	key := gw.createKey("bot").Key.Reveal()
	user := testkit.UserMarker()
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() { gw.callUser(key, user) })
	}
	for range 4 {
		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			close(release)
			t.Fatal("the calls never reached the provider")
		}
	}
	response, body := gw.callAs("/v1/chat/completions", key, user, testkit.OpenAIBody())
	decoded := decodeLimitError(t, body)
	if response.StatusCode != 429 || decoded.Error.Code != "concurrency_exceeded" || decoded.Error.Message != "too many concurrent requests for this user (limit 4)" || response.Header.Get("Retry-After") != "" {
		t.Fatalf("fifth concurrent call: %d %s", response.StatusCode, body)
	}
	close(release)
	wg.Wait()
	gw.results.waitCount(t, 4)
	if response, _ := gw.callAs("/v1/chat/completions", key, user, testkit.OpenAIBody()); response.StatusCode != 200 {
		t.Fatal("finished calls did not free their slots")
	}
}

// callUser is safe to call from several goroutines: unlike callAs, it shares no
// state on the gateway.
func (g *gateway) callUser(key, user string) int {
	request, _ := http.NewRequest(http.MethodPost, g.proxyURL("/v1/chat/completions"), strings.NewReader(testkit.OpenAIBody()))
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Comporta-User", user)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return 0
	}
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	return response.StatusCode
}
