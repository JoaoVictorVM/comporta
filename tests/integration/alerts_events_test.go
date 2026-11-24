package integration

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/alerts"
	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/storage/sqlite"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

// firstArrival is when the first webhook of a trigger reached the receiver.
func (h *hookReceiver) firstArrival(trigger alerts.Trigger) time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.calls {
		if c.path == "/hook" && containsType(c.body, trigger) {
			return c.at
		}
	}
	return time.Time{}
}

func containsType(body []byte, trigger alerts.Trigger) bool {
	return strings.Contains(string(body), `"type":"`+string(trigger)+`"`)
}

func withinMinute(t *testing.T, h *hookReceiver, trigger alerts.Trigger, since time.Time) {
	t.Helper()
	eventually(t, 60*time.Second, string(trigger)+" alert", func() bool { return !h.firstArrival(trigger).IsZero() })
	if took := h.firstArrival(trigger).Sub(since); took > 60*time.Second {
		t.Fatalf("%s alert took %s", trigger, took)
	}
}

func fallbackAlertGateway(t *testing.T, h *hookReceiver, options gatewayOptions) *gateway {
	t.Helper()
	options.edit = func(text string) string {
		return withFallbackModels(withBot("    fallback: [fake-haiku]\n")(text))
	}
	return startAlertGateway(t, h, "[50, 80, 100]", options, fakeprovider.Options{ByModel: map[string]fakeprovider.Scenario{"fake-claude": fakeprovider.ScenarioOverload}})
}

func TestLoopFallbackReloadWithinSixtySeconds(t *testing.T) {
	h := newHookReceiver(t)
	g := fallbackAlertGateway(t, h, gatewayOptions{loop: true})
	key := g.createKey("bot").Key.Reveal()
	g.userHeader = "agent-7"
	// Five fallbacks in five minutes for the app.
	start := time.Now()
	for range 5 {
		if response, body := g.call("/v1/messages", key, testkit.AnthropicBody()); response.StatusCode != http.StatusOK {
			t.Fatalf("fallback request: %d %s", response.StatusCode, body)
		}
	}
	withinMinute(t, h, alerts.TriggerFallback, start)
	// The same request keeps coming until the loop guard cuts it.
	start = time.Now()
	for range 12 {
		g.call("/v1/messages", key, testkit.AnthropicBody())
	}
	withinMinute(t, h, alerts.TriggerLoopCut, start)
	start = time.Now()
	rejectReload(t, g)
	withinMinute(t, h, alerts.TriggerReloadFailed, start)
	loop := h.payloads(alerts.TriggerLoopCut)
	burst := h.payloads(alerts.TriggerFallback)
	hash, err := g.identity.LookupHash("bot", "agent-7")
	if err != nil {
		t.Fatal(err)
	}
	if len(loop) != 1 || loop[0].UserHash != hash.Hex() || loop[0].Count < 1 || len(burst) != 1 || burst[0].Count < 5 {
		t.Fatalf("loop %+v burst %+v", loop, burst)
	}
}

func TestFailOpenProducesAlert(t *testing.T) {
	h := newHookReceiver(t)
	var broken *brokenCheckpoints
	g := startAlertGateway(t, h, "[50, 80, 100]", gatewayOptions{
		edit: withBot("    fail_mode: fail_open\n    budget: {usd_per_day: 1.00}\n"),
		wrap: func(s *sqlite.Store) storage.LimitStore {
			broken = &brokenCheckpoints{Store: s}
			return broken
		},
	}, fakeprovider.Options{})
	broken.broken.Store(true)
	if err := g.limits.Tick(context.Background()); err == nil {
		t.Fatal("injected checkpoint failure not reported")
	}
	key := g.createKey("bot").Key.Reveal()
	if response, body := g.call("/v1/chat/completions", key, testkit.OpenAIBody()); response.StatusCode != http.StatusOK {
		t.Fatalf("fail_open refused: %d %s", response.StatusCode, body)
	}
	start := time.Now()
	_ = g.limits.Tick(context.Background())
	withinMinute(t, h, alerts.TriggerFailOpen, start)
	if p := h.payloads(alerts.TriggerFailOpen); len(p) != 1 || p[0].App != "bot" || p[0].WindowStart == "" {
		t.Fatalf("fail open alerts %+v", p)
	}
}

func TestLostBusNotificationResyncsFromInbox(t *testing.T) {
	h := newHookReceiver(t)
	// No bus at all: every notification is lost, and the inbox alone carries
	// the alert to the receiver on the next poll.
	g := startAlertGateway(t, h, "[50, 80, 100]", gatewayOptions{alertOptions: func(o *alerts.Options) { o.Bus, o.Poll = nil, 500*time.Millisecond }}, fakeprovider.Options{})
	start := time.Now()
	rejectReload(t, g)
	withinMinute(t, h, alerts.TriggerReloadFailed, start)
}
