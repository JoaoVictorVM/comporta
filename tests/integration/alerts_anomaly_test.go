package integration

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/alerts"
	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

// usageTotal is the five minute aggregate of the last hours.
func usageTotal(t *testing.T, g *gateway) int64 {
	t.Helper()
	buckets, err := g.usage.UsageBuckets(context.Background(), time.Now().Add(-2*time.Hour), time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, b := range buckets {
		total += b.Cost
	}
	return total
}

func recordsCost(t *testing.T, g *gateway) int64 {
	t.Helper()
	page, err := g.usage.Records(context.Background(), storage.RecordFilter{AppName: "bot", Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, r := range page.Records {
		total += int64(r.Cost)
	}
	return total
}

// nextStep is the first instant aligned to five minutes after now, when the
// bucket of the traffic just sent is closed.
func nextStep() time.Time {
	return time.Now().UTC().Truncate(alerts.AnomalyStep).Add(alerts.AnomalyStep)
}

func TestAnomalyFromMeteredCosts(t *testing.T) {
	h := newHookReceiver(t)
	g := startAlertGateway(t, h, "[50, 80, 100]", gatewayOptions{}, fakeprovider.Options{})
	key := g.createKey("bot").Key.Reveal()
	for range 3 {
		if response, _ := g.call("/v1/messages", key, smallBody); response.StatusCode != http.StatusOK {
			t.Fatal("request refused")
		}
	}
	eventually(t, 10*time.Second, "metered cost", func() bool { return usageTotal(t, g) > 0 })
	if usageTotal(t, g) != recordsCost(t, g) {
		t.Fatalf("aggregate %d differs from the records %d", usageTotal(t, g), recordsCost(t, g))
	}
	// A new app with no history: its first hour above the minimum is an anomaly.
	if err := g.alerts.EvaluateAnomalies(context.Background(), nextStep()); err != nil {
		t.Fatal(err)
	}
	eventually(t, 60*time.Second, "anomaly alert", func() bool { return len(h.payloads(alerts.TriggerAnomaly)) == 1 })
	p := h.payloads(alerts.TriggerAnomaly)[0]
	if p.App != "bot" || *p.LastHourMicrousd != recordsCost(t, g) || *p.BaselineMicrousd != 0 || p.Multiplier != 5 {
		t.Fatalf("anomaly payload %+v", p)
	}
	// The same quarter hour never alerts twice.
	if err := g.alerts.EvaluateAnomalies(context.Background(), nextStep()); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if n := len(h.payloads(alerts.TriggerAnomaly)); n != 1 {
		t.Fatalf("anomaly alerts %d", n)
	}
}

func TestFallbackAttemptsContributeToAnomaly(t *testing.T) {
	h := newHookReceiver(t)
	g := fallbackAlertGateway(t, h, gatewayOptions{})
	key := g.createKey("bot").Key.Reveal()
	for range 2 {
		if response, body := g.call("/v1/messages", key, testkit.AnthropicBody()); response.StatusCode != http.StatusOK {
			t.Fatalf("fallback request: %d %s", response.StatusCode, body)
		}
	}
	eventually(t, 10*time.Second, "both attempts materialized", func() bool {
		page, err := g.usage.Records(context.Background(), storage.RecordFilter{AppName: "bot", Limit: 100})
		return err == nil && len(page.Records) == 4
	})
	if usageTotal(t, g) != recordsCost(t, g) || recordsCost(t, g) == 0 {
		t.Fatalf("aggregate %d, attempts %d: every attempt counts once", usageTotal(t, g), recordsCost(t, g))
	}
	if err := g.alerts.EvaluateAnomalies(context.Background(), nextStep()); err != nil {
		t.Fatal(err)
	}
	eventually(t, 60*time.Second, "anomaly alert", func() bool { return len(h.payloads(alerts.TriggerAnomaly)) == 1 })
	if p := h.payloads(alerts.TriggerAnomaly)[0]; *p.LastHourMicrousd != recordsCost(t, g) {
		t.Fatalf("anomaly saw %d, the attempts cost %d", *p.LastHourMicrousd, recordsCost(t, g))
	}
}
