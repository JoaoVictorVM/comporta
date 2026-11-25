package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/auth"
	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

const fallbackConfig = `timezone: UTC
providers:
  anthropic:
    api_format: anthropic
    base_url: %[1]s
    api_key: ${COMPORTA_PROXY_TEST_KEY}
    models: [model-a, model-b, model-c, model-d, model-e]
  openai:
    api_format: openai
    base_url: %[1]s
    api_key: ${COMPORTA_PROXY_TEST_KEY}
    models: [gpt-a, gpt-b]
apps:
  bot:
    allowed_models: [model-a, model-b, model-c, model-d, model-e, gpt-a, gpt-b]
`

// modelUpstream answers by the model named in the request body and records the
// order in which models were called.
type modelUpstream struct {
	mu      sync.Mutex
	calls   []string
	bodies  [][]byte
	replies map[string]http.HandlerFunc
}

func (u *modelUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var parsed struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &parsed)
	u.mu.Lock()
	u.calls = append(u.calls, parsed.Model)
	u.bodies = append(u.bodies, body)
	reply := u.replies[parsed.Model]
	u.mu.Unlock()
	r.Body = io.NopCloser(bytes.NewReader(body))
	if reply == nil {
		reply = answerJSON(parsed.Model)
	}
	reply(w, r)
}

func (u *modelUpstream) sent() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return slices.Clone(u.calls)
}

func answerJSON(model string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Request-Id", "req-"+model)
		_, _ = fmt.Fprintf(w, `{"id":"msg","type":"message","model":%q,"content":[],"usage":{"input_tokens":1,"output_tokens":1}}`, model)
	}
}

func answerStatus(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Retry-After", "7")
		w.Header().Set("Request-Id", "req-failed")
		w.Header().Set("X-Comporta-Fallback", "forged")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"secret upstream detail"}}`))
	}
}

func dropConnection(w http.ResponseWriter, _ *http.Request) {
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		panic(http.ErrAbortHandler)
	}
	conn, _, err := hijacker.Hijack()
	if err == nil {
		_ = conn.Close()
	}
}

func fallbackFixture(t *testing.T, chain string, replies map[string]http.HandlerFunc, extra string) (*fixture, *modelUpstream) {
	t.Helper()
	upstream := &modelUpstream{replies: replies}
	server := startServer(t, upstream)
	text := strings.Replace(fallbackConfig, "    allowed_models:", "    fallback: ["+chain+"]\n    allowed_models:", 1) + extra
	return newFixture(t, server, fixtureOptions{config: text}), upstream
}

func startServer(t *testing.T, handler http.Handler) string {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return server.URL
}

func messages(model string, stream bool) string {
	if stream {
		return bodyFor(testkit.AnthropicStreamBody(), model)
	}
	return bodyFor(testkit.AnthropicBody(), model)
}

// bodyFor names another model in a marked body built by the testkit.
func bodyFor(body, model string) string {
	for _, from := range []string{`"model":"fake-claude"`, `"model":"fake-gpt"`} {
		body = strings.Replace(body, from, `"model":"`+model+`"`, 1)
	}
	return body
}

func writeConfig(t *testing.T, text string) string {
	t.Helper()
	t.Setenv("COMPORTA_PROXY_TEST_KEY", providerCanary)
	path := filepath.Join(t.TempDir(), "comporta.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func chainOf(t *testing.T, chain []string, initial string, format auth.Format) []string {
	t.Helper()
	snapshot, err := config.NewManager(config.ManagerOptions{Path: writeConfig(t, strings.Replace(fallbackConfig, "%[1]s", "http://127.0.0.1:1", -1))})
	if err != nil {
		t.Fatal(err)
	}
	o := &operation{snapshot: snapshot.Capture(), format: format}
	app, _ := o.snapshot.App("bot")
	app.Fallback = chain
	return o.fallbackChain(app, &Envelope{Operation: OperationGeneration}, initial)
}

func TestFallbackChainSuffix(t *testing.T) {
	got := chainOf(t, []string{"model-a", "model-b", "model-c"}, "model-b", auth.FormatAnthropic)
	if !slices.Equal(got, []string{"model-c"}) {
		t.Fatalf("chain after the initial model: %v", got)
	}
	fx, upstream := fallbackFixture(t, "model-a, model-b, model-c", map[string]http.HandlerFunc{"model-b": answerStatus(529)}, "")
	response, body := fx.post("/v1/messages", "anthropic", messages("model-b", false))
	if response.StatusCode != 200 || !slices.Equal(upstream.sent(), []string{"model-b", "model-c"}) {
		t.Fatalf("%d %s %v", response.StatusCode, body, upstream.sent())
	}
	if got := response.Header.Get(fallbackHeader); got != "model-b->model-c (529)" {
		t.Fatalf("header %q", got)
	}
}

func TestFallbackInitialOutsideChain(t *testing.T) {
	got := chainOf(t, []string{"model-a", "model-b", "model-c"}, "model-d", auth.FormatAnthropic)
	if !slices.Equal(got, []string{"model-a", "model-b", "model-c"}) {
		t.Fatalf("whole chain after an outside model: %v", got)
	}
}

func TestFallbackSkipsDuplicatesAndKeepsFormats(t *testing.T) {
	got := chainOf(t, []string{"gpt-a", "model-a", "model-b", "model-b"}, "model-a", auth.FormatAnthropic)
	if !slices.Equal(got, []string{"model-b"}) {
		t.Fatalf("anthropic chain %v", got)
	}
	// Entries of another format stay in the chain: they are translated.
	if got := chainOf(t, []string{"model-a", "gpt-b"}, "gpt-a", auth.FormatOpenAI); !slices.Equal(got, []string{"model-a", "gpt-b"}) {
		t.Fatalf("openai chain %v", got)
	}
	// A model before the initial one in the chain is never sent.
	fx, upstream := fallbackFixture(t, "gpt-a, model-a, model-b", map[string]http.HandlerFunc{"model-a": answerStatus(529), "model-b": answerStatus(503)}, "")
	response, body := fx.post("/v1/messages", "anthropic", messages("model-a", false))
	if response.StatusCode != 502 || !slices.Equal(upstream.sent(), []string{"model-a", "model-b"}) || comportaCode(t, body) != string(CodeFallbackExhausted) {
		t.Fatalf("%d %s %v", response.StatusCode, body, upstream.sent())
	}
}

func TestFallbackMaximumAttempts(t *testing.T) {
	failing := map[string]http.HandlerFunc{}
	for _, m := range []string{"model-a", "model-b", "model-c", "model-d", "model-e"} {
		failing[m] = answerStatus(500)
	}
	fx, upstream := fallbackFixture(t, "model-a, model-b, model-c, model-d", failing, "")
	response, body := fx.post("/v1/messages", "anthropic", messages("model-e", false))
	if response.StatusCode != 502 || len(upstream.sent()) != maxAttempts {
		t.Fatalf("%d %s %v", response.StatusCode, body, upstream.sent())
	}
	results := fx.sink.all()
	if len(results) != maxAttempts {
		t.Fatalf("results %d", len(results))
	}
	for i, r := range results {
		if r.AttemptIndex != i+1 || r.Committed != (i == maxAttempts-1) {
			t.Fatalf("attempt %d: %+v", i, r)
		}
	}
}

func TestFallbackTriggerMatrix(t *testing.T) {
	for _, status := range []int{429, 500, 502, 503, 529} {
		fx, upstream := fallbackFixture(t, "model-b", map[string]http.HandlerFunc{"model-a": answerStatus(status)}, "")
		response, _ := fx.post("/v1/messages", "anthropic", messages("model-a", false))
		if response.StatusCode != 200 || len(upstream.sent()) != 2 {
			t.Errorf("status %d did not fall back: %d %v", status, response.StatusCode, upstream.sent())
		}
	}
	for _, status := range []int{400, 401, 403, 404, 413, 422} {
		fx, upstream := fallbackFixture(t, "model-b", map[string]http.HandlerFunc{"model-a": answerStatus(status)}, "")
		response, _ := fx.post("/v1/messages", "anthropic", messages("model-a", false))
		if response.StatusCode != status || len(upstream.sent()) != 1 {
			t.Errorf("status %d fell back: %d %v", status, response.StatusCode, upstream.sent())
		}
	}
	// A refusal inside a valid answer and a broken answer are terminal too.
	for name, reply := range map[string]http.HandlerFunc{
		"refusal": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"type":"message","stop_reason":"refusal","content":[],"usage":{"input_tokens":1,"output_tokens":0}}`))
		},
		"invalid json": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"broken`))
		},
	} {
		fx, upstream := fallbackFixture(t, "model-b", map[string]http.HandlerFunc{"model-a": reply}, "")
		fx.post("/v1/messages", "anthropic", messages("model-a", false))
		if len(upstream.sent()) != 1 {
			t.Errorf("%s fell back: %v", name, upstream.sent())
		}
	}
	// A dropped connection is a transient failure.
	fx, upstream := fallbackFixture(t, "model-b", map[string]http.HandlerFunc{"model-a": dropConnection}, "")
	response, body := fx.post("/v1/messages", "anthropic", messages("model-a", false))
	if response.StatusCode != 200 || len(upstream.sent()) != 2 || response.Header.Get(fallbackHeader) != "model-a->model-b (connection)" {
		t.Fatalf("connection: %d %s %v %q", response.StatusCode, body, upstream.sent(), response.Header.Get(fallbackHeader))
	}
	// A refusal decided locally never reaches another model.
	fx, upstream = fallbackFixture(t, "model-b", nil, "")
	response, _ = fx.post("/v1/messages", "anthropic", `{"model":"model-a","max_tokens":"x","messages":[]}`)
	if response.StatusCode != 400 || len(upstream.sent()) != 0 {
		t.Fatalf("local refusal: %d %v", response.StatusCode, upstream.sent())
	}
}

func TestFallbackCountTokensUnchanged(t *testing.T) {
	fx, upstream := fallbackFixture(t, "model-b", map[string]http.HandlerFunc{"model-a": answerStatus(529)}, "")
	response, _ := fx.post("/v1/messages/count_tokens", "anthropic", bodyFor(testkit.CountBody(), "model-a"))
	if response.StatusCode != 529 || len(upstream.sent()) != 1 || response.Header.Get(fallbackHeader) != "" {
		t.Fatalf("count_tokens fell back: %d %v", response.StatusCode, upstream.sent())
	}
}

func TestFallbackNoChainKeepsTimeout(t *testing.T) {
	// Without a chain the answer is the provider's, Retry-After and request ID
	// included, exactly as before.
	upstream := &modelUpstream{replies: map[string]http.HandlerFunc{"model-a": answerStatus(529)}}
	fx := newFixture(t, startServer(t, upstream), fixtureOptions{config: fallbackConfig + "listeners:\n  non_streaming_timeout: 200ms\n"})
	response, body := fx.post("/v1/messages", "anthropic", messages("model-a", false))
	if response.StatusCode != 529 || response.Header.Get("Retry-After") != "7" || response.Header.Get("Request-Id") != "req-failed" || comportaCode(t, body) != string(CodeProviderOverloaded) {
		t.Fatalf("%d %v %s", response.StatusCode, response.Header, body)
	}
	// The listener's own limit still applies to the single attempt.
	upstream.replies["model-b"] = func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }
	started := time.Now()
	response, _ = fx.post("/v1/messages", "anthropic", messages("model-b", false))
	if response.StatusCode != 504 || time.Since(started) > 2*time.Second {
		t.Fatalf("single attempt timeout: %d after %s", response.StatusCode, time.Since(started))
	}
}

func TestFallbackExhaustedListsCauses(t *testing.T) {
	fx, _ := fallbackFixture(t, "model-b", map[string]http.HandlerFunc{"model-a": answerStatus(529), "model-b": answerStatus(500)}, "")
	response, body := fx.post("/v1/messages", "anthropic", messages("model-a", false))
	var parsed struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Code    string `json:"comporta_code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 502 || parsed.Error.Code != "fallback_exhausted" || parsed.Error.Type != "api_error" ||
		parsed.Error.Message != "all fallback models failed (model-a: 529, model-b: 500)" {
		t.Fatalf("%d %s", response.StatusCode, body)
	}
	// Nothing the providers said reaches the client.
	if strings.Contains(body, "secret") || response.Header.Get("Retry-After") != "" || response.Header.Get("Request-Id") != "" || response.Header.Get(fallbackHeader) != "model-a->model-b (529)" {
		t.Fatalf("leaked %v %s", response.Header, body)
	}
	// The OpenAI envelope is the one WriteFailure already produces.
	fx, _ = fallbackFixture(t, "gpt-b", map[string]http.HandlerFunc{"gpt-a": answerStatus(429), "gpt-b": answerStatus(503)}, "")
	response, body = fx.post("/v1/chat/completions", "openai", bodyFor(testkit.OpenAIBody(), "gpt-a"))
	if response.StatusCode != 502 || !strings.Contains(body, `"type":"server_error"`) || !strings.Contains(body, "gpt-a: 429, gpt-b: 503") {
		t.Fatalf("openai %d %s", response.StatusCode, body)
	}
}

func TestFallbackHeadersAreIsolated(t *testing.T) {
	fx, upstream := fallbackFixture(t, "model-b, model-c", map[string]http.HandlerFunc{"model-a": answerStatus(429), "model-b": answerStatus(503)}, "")
	response, _ := fx.post("/v1/messages", "anthropic", messages("model-a", false))
	if response.StatusCode != 200 || len(upstream.sent()) != 3 {
		t.Fatalf("%d %v", response.StatusCode, upstream.sent())
	}
	// Only the answering attempt's headers come through, and the gateway's own
	// header cannot be forged by a provider.
	if response.Header.Get("Request-Id") != "req-model-c" || response.Header.Get("Retry-After") != "" ||
		response.Header.Get(fallbackHeader) != "model-a->model-b (429); model-b->model-c (503)" {
		t.Fatalf("headers %v", response.Header)
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	// Every model got the same body except for the model name.
	for i, body := range upstream.bodies {
		normalized := strings.Replace(string(body), upstream.calls[i], "MODEL", 1)
		if normalized != strings.Replace(string(upstream.bodies[0]), upstream.calls[0], "MODEL", 1) {
			t.Fatalf("body of attempt %d differs: %s", i+1, body)
		}
	}
	if got := fallbackHeaderValueOf("model a", "b->c"); got != "model%20a->b-%3Ec (529)" {
		t.Fatalf("escaping %q", got)
	}
}

func fallbackHeaderValueOf(from, to string) string {
	value, _ := fallbackHeaderValue([]transition{{from: from, to: to, cause: "529"}})
	return value
}

func TestFallbackTotalDeadline(t *testing.T) {
	if got := fallbackDeadlineAt(time.Unix(0, 0), 300*time.Second).Sub(time.Unix(0, 0)); got != fallbackTotal {
		t.Fatalf("total %s", got)
	}
	if got := fallbackDeadlineAt(time.Unix(0, 0), 30*time.Second).Sub(time.Unix(0, 0)); got != 30*time.Second {
		t.Fatalf("listener limit %s", got)
	}
	hang := func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }
	fx, upstream := fallbackFixture(t, "model-b", map[string]http.HandlerFunc{"model-a": hang}, "listeners:\n  non_streaming_timeout: 300ms\n")
	started := time.Now()
	response, body := fx.post("/v1/messages", "anthropic", messages("model-a", false))
	// The deadline is shared: once the first model used it up, nothing else is sent.
	if response.StatusCode != 504 || comportaCode(t, body) != string(CodeFallbackDeadlineExceeded) || len(upstream.sent()) != 1 ||
		!strings.Contains(body, "model-a: timeout") || time.Since(started) > 2*time.Second {
		t.Fatalf("%d %s %v after %s", response.StatusCode, body, upstream.sent(), time.Since(started))
	}
}

// fakeSessions admits everything and can fail the conclusion of an attempt.
type fakeSessions struct {
	mu        sync.Mutex
	conclude  error
	refuse    *Rejection
	continued []string
	closed    []*Failure
}

func (f *fakeSessions) Admit(context.Context, *config.Snapshot, *Envelope, string, config.Provider) (Admission, *Rejection) {
	return Admission{Complete: func(AttemptResult) {}}, nil
}

func (f *fakeSessions) AdmitSession(context.Context, *config.Snapshot, *Envelope, []Candidate) (AccountingSession, Admission, *Rejection) {
	return f, Admission{}, nil
}

func (f *fakeSessions) Conclude(AttemptResult) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.conclude
}

func (f *fakeSessions) Continue(_ context.Context, e *Envelope, _ []Candidate, from, cause string) (Admission, *Rejection) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.continued = append(f.continued, from+">"+e.ModelEffective+":"+cause)
	if f.refuse != nil {
		return Admission{}, f.refuse
	}
	return Admission{}, nil
}

func (f *fakeSessions) Close(terminal *Failure) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, terminal)
}

func TestFallbackStopsWhenAccountingCannotContinue(t *testing.T) {
	sessions := &fakeSessions{conclude: errors.New("settlement not durable")}
	upstream := &modelUpstream{replies: map[string]http.HandlerFunc{"model-a": answerStatus(529)}}
	text := strings.Replace(fallbackConfig, "    allowed_models:", "    fallback: [model-b]\n    allowed_models:", 1)
	fx := newFixture(t, startServer(t, upstream), fixtureOptions{config: text, accounting: sessions})
	response, body := fx.post("/v1/messages", "anthropic", messages("model-a", false))
	if response.StatusCode != 503 || comportaCode(t, body) != string(CodeUsageStorageUnavailable) || len(upstream.sent()) != 1 {
		t.Fatalf("%d %s %v", response.StatusCode, body, upstream.sent())
	}
	// A local refusal of the next attempt wins over exhaustion, and the session
	// is closed once with that answer.
	sessions = &fakeSessions{refuse: &Rejection{Status: 429, Code: CodeBudgetExceeded, Class: ClassRateLimit, Message: "budget"}}
	fx = newFixture(t, startServer(t, upstream), fixtureOptions{config: text, accounting: sessions})
	response, body = fx.post("/v1/messages", "anthropic", messages("model-a", false))
	if response.StatusCode != 429 || comportaCode(t, body) != string(CodeBudgetExceeded) {
		t.Fatalf("%d %s", response.StatusCode, body)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		sessions.mu.Lock()
		closed := slices.Clone(sessions.closed)
		sessions.mu.Unlock()
		if len(closed) == 1 && closed[0] != nil && closed[0].Code == CodeBudgetExceeded {
			if !slices.Equal(sessions.continued, []string{"model-a>model-b:529"}) {
				t.Fatalf("continuations %v", sessions.continued)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("session not closed once with the refusal: %+v", sessions.closed)
}
