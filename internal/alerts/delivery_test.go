package alerts

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/storage"
)

// receiver is a local destination that records every call and answers with
// whatever the test sets.
type receiver struct {
	*httptest.Server
	mu      sync.Mutex
	calls   []captured
	respond func(w http.ResponseWriter, r *http.Request)
}

type captured struct {
	header http.Header
	query  string
	body   []byte
}

func newReceiver(t *testing.T, respond func(w http.ResponseWriter, r *http.Request)) *receiver {
	t.Helper()
	rc := &receiver{respond: respond}
	rc.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rc.mu.Lock()
		rc.calls = append(rc.calls, captured{header: r.Header.Clone(), query: r.URL.RawQuery, body: body})
		respond := rc.respond
		rc.mu.Unlock()
		respond(w, r)
	}))
	t.Cleanup(rc.Close)
	return rc
}

func (rc *receiver) set(respond func(w http.ResponseWriter, r *http.Request)) {
	rc.mu.Lock()
	rc.respond = respond
	rc.mu.Unlock()
}

func (rc *receiver) count() int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return len(rc.calls)
}

func (rc *receiver) last() captured {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.calls[len(rc.calls)-1]
}

func status(code int, body string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
		_, _ = io.WriteString(w, body)
	}
}

const webhookChannel = defaults + `  channels:
    ops:
      type: webhook
      url: ${HOOK_URL}
      signing_secret: ${HOOK_SECRET}
`

const hookSecret = "s3cr3t-signing-key"

// occur stores one loop cut occurrence for every configured channel.
func occur(t *testing.T, e *env, n int) {
	t.Helper()
	publish(t, e, loopEvent(e.clock.Now(), n))
	e.process()
}

func (e *env) pending() int64 {
	e.t.Helper()
	b, err := e.store.AlertBacklog(context.Background(), e.clock.Now())
	if err != nil {
		e.t.Fatal(err)
	}
	return b.PendingDeliveries
}

func (e *env) advance(d time.Duration) { e.at(e.clock.Now().Add(d)) }

func TestWebhookSignatureAndTimestamp(t *testing.T) {
	rc := newReceiver(t, status(204, ""))
	e := newEnv(t, webhookChannel, map[string]string{"HOOK_URL": rc.URL + "/hooks/comporta?team=ops", "HOOK_SECRET": hookSecret}, func(o *Options) { o.Version = "1.2.3" })
	occur(t, e, 1)
	if n := e.service.DeliverOnce(context.Background()); n != 1 || rc.count() != 1 {
		t.Fatalf("deliveries %d calls %d", n, rc.count())
	}
	call := rc.last()
	occurrence := e.occurrences()[0]
	if !bytes.Equal(call.body, occurrence.Payload) {
		t.Fatal("body differs from the canonical payload")
	}
	timestamp, signature := call.header.Get("X-Comporta-Timestamp"), call.header.Get("X-Comporta-Signature")
	if timestamp != "1795435200" || !strings.HasPrefix(signature, "sha256=") {
		t.Fatalf("headers %q %q", timestamp, signature)
	}
	if !Verify([]byte(hookSecret), timestamp, signature, call.body, e.clock.Now()) {
		t.Fatal("signature does not verify")
	}
	if Verify([]byte(hookSecret), timestamp, signature, call.body, e.clock.Now().Add(6*time.Minute)) {
		t.Fatal("a timestamp older than five minutes verified")
	}
	if Verify([]byte("other"), timestamp, signature, call.body, e.clock.Now()) || Verify([]byte(hookSecret), timestamp, signature, append(call.body, ' '), e.clock.Now()) {
		t.Fatal("a wrong secret or body verified")
	}
	if call.header.Get("X-Comporta-Delivery-ID") != storage.DeliveryID(occurrence.ID, "ops") || call.header.Get("User-Agent") != "comporta-alerts/1.2.3" ||
		call.header.Get("Content-Type") != "application/json" || call.query != "team=ops" {
		t.Fatalf("headers %v query %q", call.header, call.query)
	}
	if e.pending() != 0 {
		t.Fatal("confirmed delivery still pending")
	}
}

func TestRedirectAndProxyDisabled(t *testing.T) {
	elsewhere := newReceiver(t, status(204, ""))
	proxy := newReceiver(t, status(204, ""))
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	rc := newReceiver(t, func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, elsewhere.URL, http.StatusFound) })
	e := newEnv(t, webhookChannel, map[string]string{"HOOK_URL": rc.URL, "HOOK_SECRET": hookSecret})
	occur(t, e, 1)
	e.service.DeliverOnce(context.Background())
	if rc.count() != 1 || elsewhere.count() != 0 || proxy.count() != 0 {
		t.Fatalf("calls: origin %d redirect target %d proxy %d", rc.count(), elsewhere.count(), proxy.count())
	}
	if e.pending() != 0 {
		t.Fatal("a redirect was retried")
	}
	if transport := NewClient().Transport.(*http.Transport); transport.Proxy != nil {
		t.Fatal("client reads proxy settings")
	}
}

func TestResponseBodyBounded(t *testing.T) {
	huge := strings.Repeat("x", 1<<20)
	rc := newReceiver(t, status(200, huge))
	e := newEnv(t, defaults, nil)
	r, err := e.service.call(context.Background(), outbound{url: rc.URL, body: []byte(`{}`)})
	if err != nil || len(r.body) != MaxResponseBytes || r.status != 200 {
		t.Fatalf("read %d bytes, status %d, %v", len(r.body), r.status, err)
	}
}

func TestRetryScheduleAndTerminalFailure(t *testing.T) {
	rc := newReceiver(t, status(503, "busy"))
	e := newEnv(t, webhookChannel, map[string]string{"HOOK_URL": rc.URL, "HOOK_SECRET": hookSecret})
	occur(t, e, 1)
	ctx := context.Background()
	e.service.DeliverOnce(ctx)
	for i, delay := range RetryDelays {
		e.advance(delay - 100*time.Millisecond)
		if e.service.DeliverOnce(ctx) != 0 {
			t.Fatalf("retry %d before its minimum delay", i+1)
		}
		e.advance(100 * time.Millisecond)
		if e.service.DeliverOnce(ctx) != 1 {
			t.Fatalf("retry %d not attempted after %s", i+1, delay)
		}
	}
	e.advance(time.Hour)
	e.service.DeliverOnce(ctx)
	if rc.count() != 4 || e.pending() != 0 {
		t.Fatalf("calls %d pending %d, want four calls and a terminal failure", rc.count(), e.pending())
	}
}

func TestRetryAfterBound(t *testing.T) {
	rc := newReceiver(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "20")
		w.WriteHeader(429)
	})
	e := newEnv(t, webhookChannel, map[string]string{"HOOK_URL": rc.URL, "HOOK_SECRET": hookSecret})
	occur(t, e, 1)
	ctx := context.Background()
	e.service.DeliverOnce(ctx)
	e.advance(19 * time.Second)
	if e.service.DeliverOnce(ctx) != 0 {
		t.Fatal("Retry-After ignored")
	}
	e.advance(time.Second)
	rc.set(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "100000")
		w.WriteHeader(429)
	})
	if e.service.DeliverOnce(ctx) != 1 {
		t.Fatal("retry not attempted after Retry-After")
	}
	e.advance(MaxRetryAfter - time.Second)
	if e.service.DeliverOnce(ctx) != 0 {
		t.Fatal("retry before the bounded Retry-After")
	}
	e.advance(time.Second)
	if e.service.DeliverOnce(ctx) != 1 {
		t.Fatal("Retry-After not bounded to five minutes")
	}
	if got := retryAfter(time.Date(2026, 11, 23, 12, 0, 30, 0, time.UTC).Format(http.TimeFormat), time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)); got != 30*time.Second {
		t.Fatalf("HTTP date Retry-After %s", got)
	}
}

func TestPermanentClientError(t *testing.T) {
	rc := newReceiver(t, status(404, "unknown webhook"))
	e := newEnv(t, webhookChannel, map[string]string{"HOOK_URL": rc.URL, "HOOK_SECRET": hookSecret})
	occur(t, e, 1)
	ctx := context.Background()
	e.service.DeliverOnce(ctx)
	e.advance(time.Hour)
	e.service.DeliverOnce(ctx)
	if rc.count() != 1 || e.pending() != 0 {
		t.Fatalf("calls %d pending %d after a 404", rc.count(), e.pending())
	}
}

func TestChannelReloadBehavior(t *testing.T) {
	first, second := newReceiver(t, status(503, "")), newReceiver(t, status(204, ""))
	vars := map[string]string{"HOOK_URL": first.URL, "HOOK_SECRET": hookSecret, "NEW_URL": second.URL}
	e := newEnv(t, webhookChannel, vars)
	occur(t, e, 1)
	ctx := context.Background()
	e.service.DeliverOnce(ctx)
	// The URL rotates: the retry uses the current secret, not the old one.
	e.reload(strings.Replace(webhookChannel, "${HOOK_URL}", "${NEW_URL}", 1))
	e.advance(RetryDelays[0])
	e.service.DeliverOnce(ctx)
	if first.count() != 1 || second.count() != 1 || e.pending() != 0 {
		t.Fatalf("rotation: first %d second %d pending %d", first.count(), second.count(), e.pending())
	}
	// A confirmed delivery is never sent again by a reload.
	e.reload(webhookChannel)
	e.advance(time.Hour)
	e.service.DeliverOnce(ctx)
	if first.count() != 1 || second.count() != 1 {
		t.Fatal("confirmed delivery repeated after reload")
	}
	// A removed channel cancels what is pending instead of sending elsewhere.
	occur(t, e, 2)
	e.reload(defaults)
	e.service.DeliverOnce(ctx)
	if e.pending() != 0 || first.count() != 1 || second.count() != 1 {
		t.Fatal("delivery of a removed channel was not canceled")
	}
	// Another type under the same name cancels as well.
	e.reload(webhookChannel)
	occur(t, e, 3)
	e.reload(defaults + "  channels:\n    ops:\n      type: slack\n      url: ${NEW_URL}\n")
	e.service.DeliverOnce(ctx)
	if e.pending() != 0 || second.count() != 1 {
		t.Fatal("delivery sent to a channel of another type")
	}
}

func TestDiscordWaitConfirmation(t *testing.T) {
	rc := newReceiver(t, status(204, ""))
	e := newEnv(t, defaults+"  channels:\n    chat:\n      type: discord\n      url: ${DISCORD}\n", map[string]string{"DISCORD": rc.URL + "/api/webhooks/1/tok?thread_id=5"})
	occur(t, e, 1)
	ctx := context.Background()
	e.service.DeliverOnce(ctx)
	if call := rc.last(); call.query != "thread_id=5&wait=true" || !strings.Contains(string(call.body), `"content":"⚠️ loop cut in bot`) {
		t.Fatalf("discord call %q %s", call.query, call.body)
	}
	if e.pending() != 0 {
		t.Fatal("204 without confirmation was retried")
	}
	rc.set(status(200, `{"id":"1180000000000000000","content":"x"}`))
	e.advance(time.Hour)
	occur(t, e, 2)
	e.service.DeliverOnce(ctx)
	if rc.count() != 2 || e.pending() != 0 {
		t.Fatal("confirmed discord message not delivered")
	}
}

func TestSlackRequiresOK(t *testing.T) {
	rc := newReceiver(t, status(200, "ok\n"))
	e := newEnv(t, defaults+"  channels:\n    team:\n      type: slack\n      url: ${SLACK}\n", map[string]string{"SLACK": rc.URL + "/services/T/B/x"})
	occur(t, e, 1)
	ctx := context.Background()
	if e.service.DeliverOnce(ctx); e.pending() != 0 || !strings.HasPrefix(string(rc.last().body), `{"text":"⚠️ loop cut in bot`) {
		t.Fatalf("slack body %s", rc.last().body)
	}
	r := reply{status: 200, body: []byte("invalid_payload")}
	if result := e.service.classify(storage.AlertDelivery{ChannelType: "slack"}, r, nil); result.Outcome != storage.AlertFailed || result.Error != CategoryInvalid {
		t.Fatalf("slack answer without ok: %+v", result)
	}
}

func TestNtfyJSONAndBearer(t *testing.T) {
	const token = "tk_canary_ntfy_token"
	rc := newReceiver(t, status(500, "upstream "+token))
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	channel := defaults + "  channels:\n    phone:\n      type: ntfy\n      server_url: " + rc.URL + "\n      topic: comporta-alerts\n      token: ${NTFY_TOKEN}\n"
	e := newEnv(t, channel, map[string]string{"NTFY_TOKEN": token}, func(o *Options) { o.Logger = logger })
	occur(t, e, 1)
	ctx := context.Background()
	for range storage.MaxAlertAttempts {
		e.service.DeliverOnce(ctx)
		e.advance(time.Minute)
	}
	call := rc.last()
	if call.header.Get("Authorization") != "Bearer "+token {
		t.Fatal("ntfy token not sent as a bearer token")
	}
	var body map[string]any
	if err := json.Unmarshal(call.body, &body); err != nil || body["topic"] != "comporta-alerts" || body["title"] != "Comporta alert" {
		t.Fatalf("ntfy body %s", call.body)
	}
	for _, forbidden := range []string{"email", "call", "attach", "click", "actions", "delay"} {
		if _, ok := body[forbidden]; ok {
			t.Fatalf("ntfy body requests %s", forbidden)
		}
	}
	if !strings.Contains(logs.String(), "alert delivery failed") {
		t.Fatalf("terminal failure not logged: %s", logs.String())
	}
	if strings.Contains(logs.String(), token) || strings.Contains(logs.String(), rc.URL) || strings.Contains(logs.String(), "upstream") {
		t.Fatalf("log carries the token, URL or response: %s", logs.String())
	}
	rc.set(status(200, `{"id":"sPs71M8A2T","time":1700000000,"event":"message","topic":"comporta-alerts"}`))
	occur(t, e, 2)
	e.service.DeliverOnce(ctx)
	if e.pending() != 0 {
		t.Fatal("ntfy publish not confirmed")
	}
}
