package integration

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/limits"
	"github.com/JoaoVictorVM/comporta/internal/proxy"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/storage/sqlite"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

const botBlock = "  bot:\n    allowed_models: [fake-claude, fake-gpt]\n"

// withBot appends settings to the bot app of the base configuration.
func withBot(settings string) func(string) string {
	return func(text string) string { return strings.Replace(text, botBlock, botBlock+settings, 1) }
}

func startLimitedGateway(t *testing.T, settings string, options gatewayOptions) *gateway {
	t.Helper()
	options.metered = true
	edit := options.edit
	options.edit = func(text string) string {
		text = withBot(settings)(text)
		if edit != nil {
			text = edit(text)
		}
		return text
	}
	return startConfiguredGateway(t, fakeprovider.Options{}, options)
}

type limitError struct {
	Type  string `json:"type"`
	Error struct {
		Type      string `json:"type"`
		Message   string `json:"message"`
		Code      string `json:"comporta_code"`
		LimitName string `json:"limit_name"`
		Scope     string `json:"scope"`
		Reason    string `json:"reason"`
		RenewsAt  string `json:"renews_at"`
	} `json:"error"`
}

func decodeLimitError(t *testing.T, body string) limitError {
	t.Helper()
	var decoded limitError
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		t.Fatalf("not an error payload: %v", err)
	}
	return decoded
}

func (g *gateway) limitView(user *limits.Subject, name string) limits.LimitView {
	g.t.Helper()
	policy, ok := limits.PolicyFor(g.config.Capture(), "bot")
	if !ok {
		g.t.Fatal("no bot app")
	}
	for _, v := range g.limits.State(policy, user).Limits {
		if v.LimitName == name {
			return v
		}
	}
	g.t.Fatalf("limit %s not in state", name)
	return limits.LimitView{}
}

func (g *gateway) userSubject(user string) *limits.Subject {
	g.t.Helper()
	hash, err := g.identity.LookupHash("bot", user)
	if err != nil {
		g.t.Fatal(err)
	}
	return &limits.Subject{App: "bot", Hash: hash.Hex()}
}

func (g *gateway) dailyCost(filter storage.Filter) int64 {
	g.t.Helper()
	if err := g.accounting.Flush(context.Background()); err != nil {
		g.t.Fatal(err)
	}
	filter.AppName = "bot"
	rows, err := g.usage.Daily(context.Background(), filter)
	if err != nil {
		g.t.Fatal(err)
	}
	total := int64(0)
	for _, row := range rows {
		total += int64(row.Cost)
	}
	return total
}

func TestConfiguredLimitsUseAuthenticatedUserAndFrozenQuote(t *testing.T) {
	gw := startLimitedGateway(t, "    budget: {usd_per_day: 1.00}\n    per_user:\n      limits: {requests_per_minute: 1}\n", gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	first, second := testkit.UserMarker(), testkit.UserMarker()+"b"
	if response, body := gw.callAs("/v1/chat/completions", key, first, testkit.OpenAIBody()); response.StatusCode != 200 {
		t.Fatalf("first user: %d %s", response.StatusCode, body)
	}
	response, body := gw.callAs("/v1/chat/completions", key, first, testkit.OpenAIBody())
	if response.StatusCode != 429 || decodeLimitError(t, body).Error.Scope != "user" {
		t.Fatalf("same user must hit its own limit: %d %s", response.StatusCode, body)
	}
	if response, body := gw.callAs("/v1/chat/completions", key, second, testkit.OpenAIBody()); response.StatusCode != 200 {
		t.Fatalf("another user shares nothing: %d %s", response.StatusCode, body)
	}
	if response, _ := gw.call("/v1/chat/completions", key, testkit.OpenAIBody()); response.StatusCode != 200 {
		t.Fatal("anonymous bucket refused its first request")
	}
	gw.results.waitCount(t, 3)
	// The budget holds exactly what the frozen quote priced for the attempts.
	if used := gw.limitView(nil, "apps.bot.budget.usd_per_day"); used.Reserved != 0 || used.Used != gw.dailyCost(storage.Filter{}) || used.Used == 0 {
		t.Fatalf("budget %+v", used)
	}
	if v := gw.limitView(gw.userSubject(first), "apps.bot.per_user.limits.requests_per_minute"); v.Used != 1 {
		t.Fatalf("authenticated user window: %+v", v)
	}
}

func Test429BothFormatsAndRetryAfter(t *testing.T) {
	for _, format := range []string{"anthropic", "openai"} {
		t.Run(format, func(t *testing.T) {
			gw := startLimitedGateway(t, "    per_user:\n      limits: {requests_per_minute: 1}\n", gatewayOptions{})
			key := gw.createKey("bot").Key.Reveal()
			path, body := "/v1/chat/completions", testkit.OpenAIBody()
			if format == "anthropic" {
				path, body = "/v1/messages", testkit.AnthropicBody()
			}
			gw.callAs(path, key, testkit.UserMarker(), body)
			response, answer := gw.callAs(path, key, testkit.UserMarker(), body)
			decoded := decodeLimitError(t, answer)
			retry, err := strconv.Atoi(response.Header.Get("Retry-After"))
			if response.StatusCode != 429 || err != nil || retry < 1 || retry > 60 || response.Header.Get("Cache-Control") != "no-store" {
				t.Fatalf("status %d retry-after %q", response.StatusCode, response.Header.Get("Retry-After"))
			}
			if decoded.Error.Type != "rate_limit_error" || decoded.Error.Code != "rate_limited" || decoded.Error.LimitName != "apps.bot.per_user.limits.requests_per_minute" || decoded.Error.Scope != "user" || decoded.Error.Reason != "spent" {
				t.Fatalf("payload: %+v", decoded)
			}
			if (format == "anthropic") != (decoded.Type == "error") {
				t.Fatalf("envelope of %s: %q", format, decoded.Type)
			}
			if strings.Contains(answer, testkit.UserMarker()) {
				t.Fatal("the refusal echoed the user identifier")
			}
		})
	}
	gw := startLimitedGateway(t, "    budget: {usd_per_day: 0.01}\n", gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	response, answer := gw.call("/v1/chat/completions", key, testkit.OpenAIBody())
	decoded := decodeLimitError(t, answer)
	renews, err := time.Parse(time.RFC3339, decoded.Error.RenewsAt)
	if response.StatusCode != 429 || decoded.Error.Code != "budget_exceeded" || err != nil || response.Header.Get("Retry-After") != "" {
		t.Fatalf("budget refusal: %d %s", response.StatusCode, answer)
	}
	if want := time.Now().UTC().Truncate(24 * time.Hour).Add(24 * time.Hour); !renews.Equal(want) || !strings.Contains(decoded.Error.Message, "daily budget of $0.01 for app bot would be exceeded (spent $0.00, reserved $0.00); the request alone exceeds this limit; renews at 00:00 UTC (in ") {
		t.Fatalf("renewal: %v %q", renews, decoded.Error.Message)
	}
}

func TestBudgetHeadersBeforeSSE(t *testing.T) {
	gw := startLimitedGateway(t, "    budget: {usd_per_day: 1.00, tokens_per_month: 1000000}\n", gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	for _, body := range []string{testkit.OpenAIStreamBody(), testkit.OpenAIBody()} {
		request, _ := http.NewRequest(http.MethodPost, gw.proxyURL("/v1/chat/completions"), strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+key)
		request.Header.Set("Content-Type", "application/json")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		// Headers arrive with the status line, before the first event is read.
		remaining, unit, limit := response.Header.Get("X-Comporta-Budget-Remaining"), response.Header.Get("X-Comporta-Budget-Unit"), response.Header.Get("X-Comporta-Budget-Limit")
		reset, resetErr := strconv.ParseInt(response.Header.Get("X-Comporta-Budget-Reset"), 10, 64)
		_, _ = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		if response.StatusCode != 200 || unit != "usd" || limit != "apps.bot.budget.usd_per_day" || resetErr != nil || reset != time.Now().UTC().Truncate(24*time.Hour).Add(24*time.Hour).Unix() {
			t.Fatalf("budget headers: %d %q %q %q %d", response.StatusCode, remaining, unit, limit, reset)
		}
		if !strings.HasPrefix(remaining, "0.9") || len(remaining) != len("0.000000") {
			t.Fatalf("remaining after the reservation: %q", remaining)
		}
	}
}

func TestUpstreamCannotOverwriteComportaHeaders(t *testing.T) {
	hostile := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("X-Comporta-Budget-Remaining", "999999.000000")
			w.Header().Set("X-Comporta-Budget-Limit", "apps.bot.budget.forged")
			w.Header().Set("X-Comporta-Debug", "upstream")
			next.ServeHTTP(w, r)
		})
	}
	gw := startLimitedGateway(t, "    budget: {usd_per_day: 1.00}\n", gatewayOptions{upstream: hostile})
	key := gw.createKey("bot").Key.Reveal()
	for _, body := range []string{testkit.OpenAIBody(), testkit.OpenAIStreamBody()} {
		response, _ := gw.call("/v1/chat/completions", key, body)
		if response.StatusCode != 200 || strings.HasPrefix(response.Header.Get("X-Comporta-Budget-Remaining"), "999999") || response.Header.Get("X-Comporta-Budget-Limit") != "apps.bot.budget.usd_per_day" || response.Header.Get("X-Comporta-Debug") != "" {
			t.Fatalf("upstream header reached the client: %v", response.Header)
		}
	}
}

func TestCancelAndProviderErrorsSettleConsumedUsage(t *testing.T) {
	for _, scenario := range []fakeprovider.Scenario{fakeprovider.ScenarioOverload, fakeprovider.ScenarioDisconnectMidstream} {
		t.Run(string(scenario), func(t *testing.T) {
			gw := startConfiguredGateway(t, fakeprovider.Options{Scenario: scenario}, gatewayOptions{metered: true, edit: withBot("    budget: {usd_per_day: 1.00}\n")})
			key := gw.createKey("bot").Key.Reveal()
			response, _ := gw.call("/v1/chat/completions", key, testkit.OpenAIStreamBody())
			if response.StatusCode == 0 {
				t.Fatal("no response")
			}
			gw.results.waitCount(t, 1)
			v := gw.limitView(nil, "apps.bot.budget.usd_per_day")
			// Without complete usage the attempt is charged its reservation, the same
			// value its usage record holds: never zero, never twice.
			if v.Reserved != 0 || v.Used == 0 || v.Used != gw.dailyCost(storage.Filter{}) {
				t.Fatalf("settled budget %+v", v)
			}
			if c := gw.limitView(nil, "apps.bot.limits.max_concurrent"); c.Used != 0 {
				t.Fatalf("slot kept after the attempt ended: %+v", c)
			}
		})
	}
}

type failingAdmission struct {
	*sqlite.Store
}

func (f failingAdmission) AdmitReserved(context.Context, storage.Record, storage.Reservation) (int64, error) {
	return 0, storage.ErrUnavailable
}

func TestNoProviderWhenAdmissionCommitFails(t *testing.T) {
	var calls atomic.Int64
	gw := startConfiguredGateway(t, fakeprovider.Options{OnRequest: func(fakeprovider.Request) { calls.Add(1) }}, gatewayOptions{
		metered: true,
		wrap:    func(s *sqlite.Store) storage.LimitStore { return failingAdmission{s} },
	})
	key := gw.createKey("bot").Key.Reveal()
	response, body := gw.call("/v1/chat/completions", key, testkit.OpenAIBody())
	if response.StatusCode != 503 || !strings.Contains(body, "usage_storage_unavailable") || calls.Load() != 0 {
		t.Fatalf("admission failure reached the provider: %d %s", response.StatusCode, body)
	}
	if v := gw.limitView(nil, "apps.bot.limits.max_concurrent"); v.Used != 0 {
		t.Fatal("a failed admission kept a slot")
	}
}

func TestConcurrentReloadCannotAdmitOldCeiling(t *testing.T) {
	held, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	hooks := proxy.Hooks{BeforeSend: func(context.Context, *proxy.Envelope, proxy.RequestContent) *proxy.Rejection {
		if first.CompareAndSwap(false, true) {
			close(held)
			<-release
		}
		return nil
	}}
	gw := startLimitedGateway(t, "    budget: {usd_per_day: 1.00}\n", gatewayOptions{hooks: hooks})
	key := gw.createKey("bot").Key.Reveal()
	old := make(chan string, 1)
	go func() {
		response, body := gw.call("/v1/chat/completions", key, testkit.OpenAIBody())
		old <- strconv.Itoa(response.StatusCode) + " " + body
	}()
	<-held
	gw.reload(func(text string) string {
		return strings.Replace(text, "budget: {usd_per_day: 1.00}", "budget: {usd_per_day: 0.02}", 1)
	})
	// A request of the new generation reaches the coordinator first; the new
	// ceiling is below a single reservation, so it is refused.
	if response, body := gw.call("/v1/chat/completions", key, testkit.OpenAIBody()); response.StatusCode != 429 {
		t.Fatalf("new generation: %d %s", response.StatusCode, body)
	}
	close(release)
	result := <-old
	// The request that captured the old generation is admitted again under the
	// new ceiling instead of the one it was quoted with.
	if !strings.HasPrefix(result, "429 ") || !strings.Contains(result, "budget_exceeded") {
		t.Fatalf("old ceiling applied after the reload: %s", result)
	}
}

func TestOverestimateAndUnderestimateAreRecorded(t *testing.T) {
	var calls atomic.Int64
	expensive := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) == 1 {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","created":0,"model":"fake-gpt","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":1000000,"total_tokens":1000010}}`)
		})
	}
	gw := startLimitedGateway(t, "    budget: {usd_per_day: 100.00}\n", gatewayOptions{upstream: expensive})
	key := gw.createKey("bot").Key.Reveal()
	if response, _ := gw.call("/v1/chat/completions", key, testkit.OpenAIBody()); response.StatusCode != 200 {
		t.Fatal("ordinary call failed")
	}
	gw.results.waitCount(t, 1)
	normal := gw.limitView(nil, "apps.bot.budget.usd_per_day").Used
	if response, _ := gw.call("/v1/chat/completions", key, testkit.OpenAIBody()); response.StatusCode != 200 {
		t.Fatal("underestimated call failed")
	}
	gw.results.waitCount(t, 2)
	v := gw.limitView(nil, "apps.bot.budget.usd_per_day")
	// The real cost is charged in full even beyond the reservation.
	if v.Used-normal != 6_000_020 || v.Used != gw.dailyCost(storage.Filter{}) {
		t.Fatalf("underestimate not charged as consumed: %+v", v)
	}
	page, err := gw.usage.Records(context.Background(), storage.RecordFilter{AppName: "bot", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	flags := map[bool]int{}
	for _, r := range page.Records {
		flags[r.EstimateExceeded]++
	}
	if flags[true] != 1 || flags[false] != 1 {
		t.Fatalf("estimate flags: %v", flags)
	}
}
