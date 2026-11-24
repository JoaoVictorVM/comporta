package integration

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/alerts"
	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
)

// spendUntil sends small requests until an alert of the trigger arrives or the
// budget refuses one, and returns how many were admitted.
func spendUntil(t *testing.T, g *gateway, key string, h *hookReceiver, done func() bool) int {
	t.Helper()
	admitted := 0
	for range 60 {
		response, body := g.call("/v1/messages", key, smallBody)
		if response.StatusCode == http.StatusTooManyRequests {
			break
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status %d %s", response.StatusCode, body)
		}
		admitted++
		if done() {
			break
		}
	}
	return admitted
}

func eighty(h *hookReceiver) func() bool {
	return func() bool { return len(h.payloads(alerts.TriggerBudget)) > 0 }
}

func TestDailyBudgetEightyPercentOncePerChannel(t *testing.T) {
	h := newHookReceiver(t)
	g := startAlertGateway(t, h, "[80]", gatewayOptions{edit: withBot("    budget: {usd_per_day: 0.001}\n")}, fakeprovider.Options{})
	key := g.createKey("bot").Key.Reveal()
	// Below 80% nothing is sent.
	g.call("/v1/messages", key, smallBody)
	time.Sleep(300 * time.Millisecond)
	if h.count("/hook") != 0 {
		t.Fatal("alert below the threshold")
	}
	spendUntil(t, g, key, h, eighty(h))
	eventually(t, 60*time.Second, "80% alert on both channels", func() bool {
		return len(h.payloads(alerts.TriggerBudget)) == 1 && h.texts("reached 80% of its daily budget") == 1
	})
	// More traffic in the same day, admitted or refused, adds nothing.
	for range 5 {
		g.call("/v1/messages", key, smallBody)
	}
	time.Sleep(500 * time.Millisecond)
	p := h.payloads(alerts.TriggerBudget)
	if len(p) != 1 || h.texts("reached 80%") != 1 || p[0].ThresholdPercent != 80 || p[0].Scope != "app" || p[0].Dimension != "usd_per_day" || *p[0].Limit != 1000 || *p[0].Used < 800 {
		t.Fatalf("budget alerts %+v, slack %d", p, h.texts("reached 80%"))
	}
}

func TestPerUserBudgetUsesHash(t *testing.T) {
	h := newHookReceiver(t)
	g := startAlertGateway(t, h, "[80]", gatewayOptions{edit: withBot("    per_user:\n      budget: {usd_per_day: 0.001}\n")}, fakeprovider.Options{})
	key := g.createKey("bot").Key.Reveal()
	const raw = "customer-4242"
	g.userHeader = raw
	spendUntil(t, g, key, h, eighty(h))
	eventually(t, 60*time.Second, "per-user 80% alert", func() bool { return len(h.payloads(alerts.TriggerBudget)) == 1 })
	hash, err := g.identity.LookupHash("bot", raw)
	if err != nil {
		t.Fatal(err)
	}
	p := h.payloads(alerts.TriggerBudget)[0]
	if p.Scope != "user" || p.UserHash != hash.Hex() {
		t.Fatalf("payload %+v, want the pseudonym %s", p, hash.Hex())
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.calls {
		if strings.Contains(string(c.body), raw) || (c.path == "/slack" && strings.Contains(string(c.body), hash.Hex())) {
			t.Fatalf("%s body carries the raw ID or the full pseudonym: %s", c.path, c.body)
		}
	}
}

func TestBudgetRestartDoesNotRepeat(t *testing.T) {
	h := newHookReceiver(t)
	g := startAlertGateway(t, h, "[50, 80]", gatewayOptions{edit: withBot("    budget: {usd_per_day: 0.001}\n")}, fakeprovider.Options{})
	key := g.createKey("bot").Key.Reveal()
	spendUntil(t, g, key, h, func() bool { return len(h.payloads(alerts.TriggerBudget)) >= 2 })
	eventually(t, 60*time.Second, "50% and 80% alerts", func() bool { return len(h.payloads(alerts.TriggerBudget)) == 2 })
	// Another life of the worker over the same store: inputs, the budget sweep
	// and deliveries find nothing new.
	ctx := context.Background()
	again := alerts.New(alerts.Options{Store: g.usage, Source: g.config, Logger: slog.New(slog.DiscardHandler)})
	if _, err := again.ProcessInputs(ctx); err != nil {
		t.Fatal(err)
	}
	if err := again.EvaluateBudgets(ctx); err != nil {
		t.Fatal(err)
	}
	again.DeliverOnce(ctx)
	time.Sleep(300 * time.Millisecond)
	if n := len(h.payloads(alerts.TriggerBudget)); n != 2 || h.texts("of its daily budget") != 2 {
		t.Fatalf("budget alerts %d after restart", n)
	}
}
