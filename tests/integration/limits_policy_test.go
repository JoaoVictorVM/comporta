package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/proxy"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

const downgradedFrom = "X-Comporta-Downgraded-From"

// withPolicyModels declares a cheaper model in each API format, priced at one
// dollar per million tokens, and allows both for the bot app.
func withPolicyModels(text string) string {
	text = strings.Replace(text, "models: [fake-claude]", "models: [fake-claude, fake-haiku]", 1)
	text = strings.Replace(text, "models: [fake-gpt]", "models: [fake-gpt, fake-gpt-mini]", 1)
	text = strings.Replace(text, "  bot:\n    allowed_models: [fake-claude, fake-gpt]\n", "  bot:\n    allowed_models: [fake-claude, fake-gpt, fake-haiku, fake-gpt-mini]\n", 1)
	return strings.Replace(text, "pricing:\n", "pricing:\n  fake-haiku: {input: 1, output: 1, cache_write: 1, cache_read: 0.1}\n  fake-gpt-mini: {input: 1, output: 1, cache_write: 1, cache_read: 0.1}\n", 1)
}

func startPolicyGateway(t *testing.T, settings string, options gatewayOptions) *gateway {
	t.Helper()
	edit := options.edit
	options.edit = func(text string) string {
		text = withPolicyModels(text)
		if edit != nil {
			text = edit(text)
		}
		return text
	}
	return startLimitedGateway(t, settings, options)
}

// A one-microdollar budget: every generation exceeds it with its own reservation.
const tinyBudget = "    budget: {usd_per_day: 0.000001}\n"

func requestModel(t *testing.T, r fakeprovider.Request) (model string, maxTokens json.RawMessage) {
	t.Helper()
	var body struct {
		Model     string          `json:"model"`
		MaxTokens json.RawMessage `json:"max_tokens"`
	}
	if err := json.Unmarshal(r.Body, &body); err != nil {
		t.Fatal(err)
	}
	return body.Model, body.MaxTokens
}

func (g *gateway) records(t *testing.T) []storage.Record {
	t.Helper()
	if err := g.accounting.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	page, err := g.usage.Records(context.Background(), storage.RecordFilter{AppName: "bot", Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return page.Records
}

func (g *gateway) policyEvents(t *testing.T, kind events.Type) []events.Event {
	t.Helper()
	page, err := g.usage.ReadEvents(context.Background(), events.Filter{App: "bot", Types: []events.Type{kind}, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	return page.Events
}

func (g *gateway) noProviderCall(t *testing.T) {
	t.Helper()
	select {
	case r := <-g.requests:
		t.Fatalf("the provider was called: %s", r.Body)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestPolicyConsumesLimitDecisionAndHardCap(t *testing.T) {
	// The per-user budget triggers the policy; the app hard cap still bounds it.
	gw := startPolicyGateway(t, "    on_limit: downgrade\n    downgrade_to: fake-haiku\n    hard_cap: {usd_per_day: 1.20}\n    budget: {usd_per_day: 1.00}\n    per_user:\n      budget: {usd_per_day: 0.000001}\n      hard_cap: {usd_per_day: 0.01}\n", gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	user := testkit.UserMarker()
	response, body := gw.callAs("/v1/messages", key, user, testkit.AnthropicBody())
	if response.StatusCode != 200 || response.Header.Get(downgradedFrom) != "fake-claude" {
		t.Fatalf("status %d header %q: %s", response.StatusCode, response.Header.Get(downgradedFrom), body)
	}
	if model, _ := requestModel(t, gw.lastRequest()); model != "fake-haiku" {
		t.Fatalf("provider received %s", model)
	}
	gw.results.waitCount(t, 1)
	records := gw.records(t)
	if len(records) != 1 {
		t.Fatalf("records: %+v", records)
	}
	r := records[0]
	if r.PolicyOutcome != "downgraded" || r.Outcome != "downgraded" || r.PolicyLimitName != "apps.bot.per_user.budget.usd_per_day" || r.PolicyScope != "user" ||
		r.ModelRequested != "fake-claude" || r.ModelUsed != "fake-haiku" || r.UserHash != gw.userSubject(user).Hash || r.PolicyRenewsAt == nil {
		t.Fatalf("record: %+v", r)
	}
	if got := gw.policyEvents(t, events.Downgraded); len(got) != 1 || got[0].RequestID != r.RequestID || got[0].UserHash != "" {
		t.Fatalf("event: %+v", got)
	}
	if strings.Contains(body, user) {
		t.Fatal("the answer echoed the user identifier")
	}
}

func TestPolicyBlockBothAPIFormats(t *testing.T) {
	for _, format := range []string{"anthropic", "openai"} {
		t.Run(format, func(t *testing.T) {
			gw := startPolicyGateway(t, "    on_limit: block\n"+tinyBudget, gatewayOptions{})
			key := gw.createKey("bot").Key.Reveal()
			path, body := "/v1/chat/completions", testkit.OpenAIBody()
			if format == "anthropic" {
				path, body = "/v1/messages", testkit.AnthropicBody()
			}
			response, answer := gw.call(path, key, body)
			decoded := decodeLimitError(t, answer)
			renews, err := time.Parse(time.RFC3339, decoded.Error.RenewsAt)
			if response.StatusCode != 429 || decoded.Error.Code != "budget_exceeded" || decoded.Error.LimitName != "apps.bot.budget.usd_per_day" ||
				decoded.Error.Scope != "app" || decoded.Error.Reason != "spent" || err != nil || !renews.After(time.Now()) || renews.Hour() != 0 {
				t.Fatalf("status %d: %s", response.StatusCode, answer)
			}
			if (format == "anthropic") != (decoded.Type == "error") || response.Header.Get(downgradedFrom) != "" {
				t.Fatalf("envelope: %s", answer)
			}
			gw.noProviderCall(t)
			if records := gw.records(t); len(records) != 1 || records[0].PolicyOutcome != "blocked" || records[0].AttemptIndex != 0 || records[0].Cost != 0 {
				t.Fatalf("records: %+v", records)
			}
		})
	}
}

func TestWarnWithoutCapForwardsAndRecords(t *testing.T) {
	gw := startPolicyGateway(t, "    on_limit: warn\n"+tinyBudget, gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	for range 3 {
		response, body := gw.call("/v1/chat/completions", key, testkit.OpenAIBody())
		if response.StatusCode != 200 || response.Header.Get(downgradedFrom) != "" {
			t.Fatalf("status %d: %s", response.StatusCode, body)
		}
		if model, _ := requestModel(t, gw.lastRequest()); model != "fake-gpt" {
			t.Fatalf("warn changed the model: %s", model)
		}
	}
	gw.results.waitCount(t, 3)
	for _, r := range gw.records(t) {
		if r.PolicyOutcome != "warned" || r.Outcome != "ok" || r.Cost == 0 || r.ModelUsed != "fake-gpt" {
			t.Fatalf("record: %+v", r)
		}
	}
	if got := gw.policyEvents(t, events.Warned); len(got) != 3 {
		t.Fatalf("%d warned events", len(got))
	}
	// Spend went past the budget: without a hard cap there is no ceiling.
	if view := gw.limitView(nil, "apps.bot.budget.usd_per_day"); view.Used <= view.Limit {
		t.Fatalf("budget %+v", view)
	}

	// With a hard cap, warn blocks at the cap.
	capped := startPolicyGateway(t, "    on_limit: warn\n"+tinyBudget+"    hard_cap: {usd_per_day: 0.000002}\n", gatewayOptions{})
	key = capped.createKey("bot").Key.Reveal()
	response, answer := capped.call("/v1/chat/completions", key, testkit.OpenAIBody())
	if decoded := decodeLimitError(t, answer); response.StatusCode != 429 || decoded.Error.Code != "hard_cap_exceeded" || decoded.Error.LimitName != "apps.bot.hard_cap.usd_per_day" || decoded.Error.RenewsAt == "" {
		t.Fatalf("status %d: %s", response.StatusCode, answer)
	}
	if !strings.Contains(answer, "hard cap") {
		t.Fatalf("message does not say the cap is mandatory: %s", answer)
	}
	capped.noProviderCall(t)
	if len(capped.policyEvents(t, events.Warned)) != 0 {
		t.Fatal("a refused request was marked warned")
	}
}

func TestDowngradeStopsAtHardCap(t *testing.T) {
	gw := startPolicyGateway(t, "    on_limit: downgrade\n    downgrade_to: fake-haiku\n"+tinyBudget+"    hard_cap: {usd_per_day: 0.0005}\n", gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	admitted := 0
	for i := 0; ; i++ {
		if i > 300 {
			t.Fatal("the hard cap was never reached")
		}
		response, body := gw.call("/v1/messages", key, testkit.AnthropicBody())
		if response.StatusCode == 429 {
			if decoded := decodeLimitError(t, body); decoded.Error.Code != "hard_cap_exceeded" {
				t.Fatalf("stopped by %s", body)
			}
			break
		}
		if response.StatusCode != 200 || response.Header.Get(downgradedFrom) != "fake-claude" {
			t.Fatalf("status %d: %s", response.StatusCode, body)
		}
		if model, _ := requestModel(t, gw.lastRequest()); model != "fake-haiku" {
			t.Fatalf("provider received %s", model)
		}
		admitted++
		gw.results.waitCount(t, admitted)
	}
	if admitted == 0 {
		t.Fatal("no request was downgraded before the cap")
	}
	gw.noProviderCall(t)
	if response, _ := gw.call("/v1/messages", key, testkit.AnthropicBody()); response.StatusCode != 429 {
		t.Fatal("the cap admitted another request")
	}
	gw.noProviderCall(t)
	view := gw.limitView(nil, "apps.bot.hard_cap.usd_per_day")
	if view.Used > view.Limit || view.Reserved != 0 {
		t.Fatalf("cap %+v", view)
	}
	if got := len(gw.policyEvents(t, events.Downgraded)); got != admitted {
		t.Fatalf("%d downgraded events for %d admissions", got, admitted)
	}
}

func TestForbiddenModelCannotDowngrade(t *testing.T) {
	gw := startPolicyGateway(t, "    on_limit: downgrade\n    downgrade_to: fake-haiku\n"+tinyBudget+"    hard_cap: {usd_per_day: 1.00}\n", gatewayOptions{edit: func(text string) string {
		return strings.Replace(text, "allowed_models: [fake-claude, fake-gpt, fake-haiku, fake-gpt-mini]", "allowed_models: [fake-gpt, fake-haiku]", 1)
	}})
	key := gw.createKey("bot").Key.Reveal()
	response, body := gw.call("/v1/messages", key, testkit.AnthropicBody())
	want := `{"type":"error","error":{"type":"permission_error","message":"model \"fake-claude\" is not allowed for app bot","comporta_code":"model_not_allowed"}}`
	if response.StatusCode != http.StatusForbidden || body != want {
		t.Fatalf("status %d: %s", response.StatusCode, body)
	}
	gw.noProviderCall(t)
	if records := gw.records(t); len(records) != 0 {
		t.Fatalf("a forbidden model reached accounting: %+v", records)
	}
}

func TestUnavailableDowngradeReturns502(t *testing.T) {
	gw := startPolicyGateway(t, "    on_limit: downgrade\n    downgrade_to: fake-haiku\n"+tinyBudget+"    hard_cap: {usd_per_day: 1.00}\n", gatewayOptions{edit: func(text string) string {
		// The target lives on a provider nobody listens on.
		text = strings.Replace(text, "models: [fake-claude, fake-haiku]", "models: [fake-claude]", 1)
		return strings.Replace(text, "pricing:\n", "  dead:\n    api_format: anthropic\n    base_url: http://127.0.0.1:1\n    api_key: ${COMPORTA_INTEGRATION_KEY}\n    models: [fake-haiku]\npricing:\n", 1)
	}})
	key := gw.createKey("bot").Key.Reveal()
	response, body := gw.call("/v1/messages", key, testkit.AnthropicBody())
	if response.StatusCode != http.StatusBadGateway || !strings.Contains(body, `"comporta_code":"provider_unreachable"`) || response.Header.Get(downgradedFrom) != "fake-claude" {
		t.Fatalf("status %d: %s", response.StatusCode, body)
	}
	// The original is never sent instead of the unavailable target.
	gw.noProviderCall(t)
	gw.results.waitCount(t, 1)
	if records := gw.records(t); len(records) != 1 || records[0].ModelUsed != "fake-haiku" || records[0].Provider != "dead" || records[0].ErrorClass != "connection" {
		t.Fatalf("records: %+v", records)
	}
}

func TestAdmittedStreamFinishesAfterCapReload(t *testing.T) {
	entered, release := make(chan struct{}, 1), make(chan struct{})
	held := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(body), `"stream":true`) {
				r.Body = io.NopCloser(strings.NewReader(string(body)))
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"id\":\"c\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":null}\n\n")
			w.(http.Flusher).Flush()
			entered <- struct{}{}
			<-release
			_, _ = io.WriteString(w, "data: {\"id\":\"c\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12}}\n\ndata: [DONE]\n\n")
		})
	}
	gw := startPolicyGateway(t, "    on_limit: warn\n"+tinyBudget+"    hard_cap: {usd_per_day: 1.00}\n", gatewayOptions{upstream: held})
	key := gw.createKey("bot").Key.Reveal()
	type outcome struct {
		status int
		body   string
	}
	done := make(chan outcome, 1)
	go func() {
		request, _ := http.NewRequest(http.MethodPost, gw.proxyURL("/v1/chat/completions"), strings.NewReader(testkit.OpenAIStreamBody()))
		request.Header.Set("Authorization", "Bearer "+key)
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			done <- outcome{}
			return
		}
		defer func() { _ = response.Body.Close() }()
		data, _ := io.ReadAll(bufio.NewReader(response.Body))
		done <- outcome{response.StatusCode, string(data)}
	}()
	<-entered
	gw.reload(func(text string) string {
		return strings.Replace(text, "hard_cap: {usd_per_day: 1.00}", "hard_cap: {usd_per_day: 0.000001}", 1)
	})
	if response, body := gw.call("/v1/chat/completions", key, testkit.OpenAIBody()); response.StatusCode != 429 || !strings.Contains(body, "hard_cap_exceeded") {
		t.Fatalf("the new cap was not applied to the next request: %d %s", response.StatusCode, body)
	}
	close(release)
	got := <-done
	if got.status != 200 || !strings.Contains(got.body, "[DONE]") {
		t.Fatalf("the admitted stream was cut: %d %q", got.status, got.body)
	}
	gw.results.waitCount(t, 1)
	for _, r := range gw.records(t) {
		if r.AttemptIndex == 1 && (r.PolicyOutcome != "warned" || r.ErrorCode != "") {
			t.Fatalf("stream record: %+v", r)
		}
	}
}

func TestPolicyReloadRebuildsCandidateAndQuote(t *testing.T) {
	var gw *gateway
	var key string
	var fired atomic.Bool
	var nested sync.WaitGroup
	hooks := proxy.Hooks{BeforeSend: func(context.Context, *proxy.Envelope, proxy.RequestContent) *proxy.Rejection {
		if !fired.CompareAndSwap(false, true) {
			return nil
		}
		// A reload lands, and another request reserves on it first, while this
		// one is still being prepared on the old snapshot.
		gw.reload(func(text string) string {
			return strings.Replace(text, "    on_limit: downgrade\n", "    on_limit: downgrade\n    max_tokens: 50\n", 1)
		})
		nested.Add(1)
		defer nested.Done()
		if response, body := gw.call("/v1/messages", key, testkit.AnthropicBody()); response.StatusCode != 200 {
			t.Errorf("nested request: %d %s", response.StatusCode, body)
		}
		return nil
	}}
	gw = startPolicyGateway(t, "    on_limit: downgrade\n    downgrade_to: fake-haiku\n"+tinyBudget+"    hard_cap: {usd_per_day: 1.00}\n", gatewayOptions{hooks: hooks})
	key = gw.createKey("bot").Key.Reveal()
	body := strings.Replace(testkit.AnthropicBody(), `"max_tokens":32`, `"max_tokens":900`, 1)
	response, answer := gw.call("/v1/messages", key, body)
	nested.Wait()
	if response.StatusCode != 200 || response.Header.Get(downgradedFrom) != "fake-claude" {
		t.Fatalf("status %d: %s", response.StatusCode, answer)
	}
	// Both requests were sent once each, both on the new cap.
	for range 2 {
		if model, maxTokens := requestModel(t, gw.lastRequest()); model != "fake-haiku" || string(maxTokens) == "900" {
			t.Fatalf("sent %s with max_tokens %s", model, maxTokens)
		}
	}
	gw.noProviderCall(t)
	gw.results.waitCount(t, 2)
	records := gw.records(t)
	if len(records) != 2 {
		t.Fatalf("records: %+v", records)
	}
	for _, r := range records {
		if r.Quote.EstimatedOutput > 50 || r.PolicyOutcome != "downgraded" || r.ModelUsed != "fake-haiku" {
			t.Fatalf("record priced on the old snapshot: %+v", r)
		}
	}
	if got := len(gw.policyEvents(t, events.Downgraded)); got != 2 {
		t.Fatalf("%d downgraded events for two requests", got)
	}
}

func TestPoliciesRespectPauseLoopAndSensitiveBlock(t *testing.T) {
	settings := "    on_limit: warn\n" + tinyBudget
	edit := func(text string) string {
		return withBot(settings)(withDetection("secret: block")(text))
	}
	s := startServedWith(t, fakeprovider.Options{}, edit)
	if status, body := s.admin(http.MethodPost, "/admin/v1/pause", `{"app":"bot"}`); status != http.StatusOK {
		t.Fatalf("pause: %d %s", status, body)
	}
	if status, body := s.call("bot", "", gptBody); status != http.StatusServiceUnavailable || !strings.Contains(body, "app_paused") {
		t.Fatalf("a paused app was let through by warn: %d %s", status, body)
	}
	if status, _ := s.admin(http.MethodPost, "/admin/v1/resume", `{"app":"bot"}`); status != http.StatusOK {
		t.Fatal("resume")
	}
	// Assembled at run time so the repository holds no complete credential shape.
	secret := strings.Replace(gptBody, "Ola", "use "+"sk-"+"ant-"+"api03-"+strings.Repeat("Zq7_", 10), 1)
	if status, body := s.call("bot", "", secret); status != http.StatusBadRequest || !strings.Contains(body, "sensitive_data_blocked") {
		t.Fatalf("warn bypassed the sensitive block: %d %s", status, body)
	}
	if status, body := s.call("bot", "", gptBody); status != http.StatusOK {
		t.Fatalf("warned request: %d %s", status, body)
	}
}

func TestForgetRemovesPolicyCorrelations(t *testing.T) {
	s := startServedWith(t, fakeprovider.Options{}, withBot("    on_limit: warn\n"+tinyBudget))
	id := testkit.UserMarker()
	if status, body := s.call("bot", id, gptBody); status != http.StatusOK {
		t.Fatalf("%d %s", status, body)
	}
	hash := s.hash("bot", id)
	s.waitStored("bot", hash, false, 1)
	status, reply, text := s.forget(id)
	// The request, its completion and its warned event go together.
	if status != http.StatusOK || reply.Deleted.Requests != 1 || reply.Deleted.Events != 2 {
		t.Fatalf("forget: %d %s", status, text)
	}
	if requests, daily := s.stored("bot", hash, false); requests != 0 || daily != 0 {
		t.Fatalf("records left: %d %d", requests, daily)
	}
}
