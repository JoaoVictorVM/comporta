package integration

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/alerts"
	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

const alertSecret = "integration-alert-signing-secret"

// hookReceiver is a local generic webhook at /hook and a Slack incoming
// webhook at /slack. It records every call and answers what the test sets.
type hookReceiver struct {
	*httptest.Server
	mu      sync.Mutex
	calls   []hookCall
	respond func(path string) int
}

type hookCall struct {
	path   string
	header http.Header
	body   []byte
	at     time.Time
}

func newHookReceiver(t *testing.T) *hookReceiver {
	t.Helper()
	h := &hookReceiver{respond: func(string) int { return http.StatusNoContent }}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.calls = append(h.calls, hookCall{path: r.URL.Path, header: r.Header.Clone(), body: body, at: time.Now()})
		status := h.respond(r.URL.Path)
		h.mu.Unlock()
		if r.URL.Path == "/slack" && status == http.StatusNoContent {
			_, _ = io.WriteString(w, "ok")
			return
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *hookReceiver) set(respond func(path string) int) {
	h.mu.Lock()
	h.respond = respond
	h.mu.Unlock()
}

// payloads are the generic webhook bodies of one trigger.
func (h *hookReceiver) payloads(trigger alerts.Trigger) []alerts.Payload {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []alerts.Payload
	for _, c := range h.calls {
		if c.path != "/hook" {
			continue
		}
		var p alerts.Payload
		if json.Unmarshal(c.body, &p) == nil && p.Type == trigger {
			out = append(out, p)
		}
	}
	return out
}

// texts are the Slack messages that contain a fragment.
func (h *hookReceiver) texts(fragment string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, c := range h.calls {
		if c.path == "/slack" && strings.Contains(string(c.body), fragment) {
			n++
		}
	}
	return n
}

func (h *hookReceiver) count(path string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, c := range h.calls {
		if c.path == path {
			n++
		}
	}
	return n
}

func eventually(t *testing.T, within time.Duration, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("%s not observed within %s", what, within)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// alertBlock configures both receivers through the environment, as secrets.
func alertBlock(t *testing.T, h *hookReceiver, thresholds string) string {
	t.Helper()
	t.Setenv("COMPORTA_ALERT_HOOK", h.URL+"/hook")
	t.Setenv("COMPORTA_ALERT_SLACK", h.URL+"/slack")
	t.Setenv("COMPORTA_ALERT_SECRET", alertSecret)
	return `alerts:
  thresholds: ` + thresholds + `
  anomaly_min_usd: 0.000001
  channels:
    hook:
      type: webhook
      url: ${COMPORTA_ALERT_HOOK}
      signing_secret: ${COMPORTA_ALERT_SECRET}
    team:
      type: slack
      url: ${COMPORTA_ALERT_SLACK}
`
}

// startAlertGateway runs a metered gateway with the alert worker. The poll is
// short so tests wait on deliveries, not on the clock.
func startAlertGateway(t *testing.T, h *hookReceiver, thresholds string, options gatewayOptions, fake fakeprovider.Options) *gateway {
	t.Helper()
	block := alertBlock(t, h, thresholds)
	edit := options.edit
	options.edit = func(text string) string {
		if edit != nil {
			text = edit(text)
		}
		return text + block
	}
	options.metered, options.alerts = true, true
	poll := options.alertOptions
	options.alertOptions = func(o *alerts.Options) {
		o.Poll = 50 * time.Millisecond
		if poll != nil {
			poll(o)
		}
	}
	return startConfiguredGateway(t, fake, options)
}

// smallBody is a marked prompt with a two token answer, so a budget is
// crossed in a few steps while each reservation stays small.
var smallBody = `{"model":"fake-claude","max_tokens":2,"messages":[{"role":"user","content":"` + testkit.Prompt("hi") + `"}]}`

// rejectReload writes an invalid file and waits for the manager to refuse it.
func rejectReload(t *testing.T, g *gateway) {
	t.Helper()
	if err := os.WriteFile(g.path, []byte(g.baseYAML+"telemetry:\n  otel_enabled: maybe\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if status := g.config.Reload(context.Background()); status.Result != config.ResultRejected {
		t.Fatalf("reload: %+v", status)
	}
}

func TestWebhookValidHMACAndTimestamp(t *testing.T) {
	h := newHookReceiver(t)
	g := startAlertGateway(t, h, "[50, 80, 100]", gatewayOptions{}, fakeprovider.Options{})
	rejectReload(t, g)
	eventually(t, 60*time.Second, "failed reload alert", func() bool { return len(h.payloads(alerts.TriggerReloadFailed)) == 1 })
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.calls {
		if c.path != "/hook" {
			continue
		}
		timestamp, signature := c.header.Get("X-Comporta-Timestamp"), c.header.Get("X-Comporta-Signature")
		if !alerts.Verify([]byte(alertSecret), timestamp, signature, c.body, time.Now()) {
			t.Fatalf("signature %q over timestamp %q does not verify", signature, timestamp)
		}
		if len(c.header.Get("X-Comporta-Delivery-ID")) != 32 {
			t.Fatal("delivery id missing")
		}
	}
}

func TestRetriesResumeAfterRestart(t *testing.T) {
	h := newHookReceiver(t)
	h.set(func(path string) int {
		if path == "/hook" {
			return http.StatusServiceUnavailable
		}
		return http.StatusNoContent
	})
	// The gateway's own worker stays parked: this test plays two worker lives
	// over the same store.
	g := startAlertGateway(t, h, "[50, 80, 100]", gatewayOptions{alertOptions: func(o *alerts.Options) { o.Poll, o.Bus = time.Hour, nil }}, fakeprovider.Options{})
	rejectReload(t, g)
	ctx := context.Background()
	first := alerts.New(alerts.Options{Store: g.usage, Source: g.config, Logger: slog.New(slog.DiscardHandler)})
	eventually(t, 10*time.Second, "failed reload input", func() bool {
		_, _ = first.ProcessInputs(ctx)
		return first.DeliverOnce(ctx) > 0 && h.count("/hook") == 1
	})
	// The first life ends; the retry is due 2 s later and is found by a new one.
	h.set(func(string) int { return http.StatusNoContent })
	second := alerts.New(alerts.Options{Store: g.usage, Source: g.config, Logger: slog.New(slog.DiscardHandler)})
	eventually(t, 15*time.Second, "retried delivery", func() bool {
		second.DeliverOnce(ctx)
		return h.count("/hook") == 2
	})
	time.Sleep(alerts.RetryDelays[1] + time.Second)
	second.DeliverOnce(ctx)
	if h.count("/hook") != 2 || len(h.payloads(alerts.TriggerReloadFailed)) != 2 {
		t.Fatalf("hook calls %d, want the failed call and one confirmed retry", h.count("/hook"))
	}
	backlog, err := g.usage.AlertBacklog(ctx, time.Now())
	if err != nil || backlog.PendingDeliveries != 0 {
		t.Fatalf("backlog %+v %v", backlog, err)
	}
}
