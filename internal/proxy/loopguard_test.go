package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/loopguard"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

type loopRecorder struct {
	mu    sync.Mutex
	cuts  []loopguard.Decision
	err   error
	order *[]string
}

func (r *loopRecorder) RecordLoopCut(_ context.Context, _ *config.Snapshot, _ *Envelope, d loopguard.Decision) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.order != nil {
		*r.order = append(*r.order, "loop-record")
	}
	r.cuts = append(r.cuts, d)
	return r.err
}

type orderAccounting struct {
	order *[]string
}

func (a orderAccounting) Admit(context.Context, *config.Snapshot, *Envelope, string, config.Provider) (Admission, *Rejection) {
	*a.order = append(*a.order, "accounting")
	return Admission{Complete: func(AttemptResult) {}}, nil
}

const loopSettings = `    loop_guard:
      similar_requests: 2
      requests_per_minute: 100
      cooldown: 90s
`

func loopFixture(t *testing.T, settings string, recorder *loopRecorder, accounting Accounting) (*fixture, *recordingUpstream, *loopguard.FakeClock) {
	t.Helper()
	upstream, server := startUpstream(t, nil)
	clock := loopguard.NewFakeClock(time.Date(2026, 11, 11, 15, 0, 0, 0, time.UTC))
	stage := &LoopStage{Guard: loopguard.New(loopguard.Options{Clock: clock}), Recorder: recorder}
	fx := newFixture(t, server.URL, fixtureOptions{listeners: settings, loop: stage, accounting: accounting})
	stage.Source = fx.config
	return fx, upstream, clock
}

var (
	anthropicLoopBody = `{"model":"fake-claude","max_tokens":10,"metadata":{"user_id":"u-1"},"messages":[{"role":"user","content":"` + testkit.Prompt("continue") + `"}]}`
	openaiLoopBody    = `{"model":"fake-gpt","user":"u-1","messages":[{"role":"user","content":"` + testkit.Prompt("continue") + `"}]}`
)

func TestGuardRunsBeforeAccounting(t *testing.T) {
	var order []string
	recorder := &loopRecorder{order: &order}
	fx, upstream, _ := loopFixture(t, loopSettings, recorder, orderAccounting{order: &order})
	for range 2 {
		if response, body := fx.post("/v1/messages", "anthropic", anthropicLoopBody); response.StatusCode != 200 {
			t.Fatalf("allowed request: %d %s", response.StatusCode, body)
		}
	}
	response, body := fx.post("/v1/messages", "anthropic", anthropicLoopBody)
	if response.StatusCode != 429 || comportaCode(t, body) != "loop_detected" {
		t.Fatalf("third similar: %d %s", response.StatusCode, body)
	}
	if upstream.count() != 2 || strings.Join(order, ",") != "accounting,accounting,loop-record" {
		t.Fatalf("upstream=%d order=%v", upstream.count(), order)
	}
	for _, name := range []string{"X-Comporta-Budget-Remaining", "X-Comporta-Budget-Limit"} {
		if response.Header.Get(name) != "" {
			t.Fatalf("budget header on a loop cut: %s", name)
		}
	}
}

func TestLoopRejectionFormats(t *testing.T) {
	fx, _, _ := loopFixture(t, loopSettings, &loopRecorder{}, nil)
	for range 2 {
		fx.post("/v1/chat/completions", "openai", openaiLoopBody)
	}
	response, body := fx.post("/v1/chat/completions", "openai", openaiLoopBody)
	var openai struct {
		Type  *string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Code    string `json:"comporta_code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &openai); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 429 || openai.Type != nil || openai.Error.Type != "rate_limit_error" || openai.Error.Code != "loop_detected" ||
		openai.Error.Message != "loop detected: 2 similar requests in 30s; requests from this user are paused for 90s" {
		t.Fatalf("openai: %d %s", response.StatusCode, body)
	}

	fx, _, _ = loopFixture(t, "    loop_guard:\n      requests_per_minute: 1\n", &loopRecorder{}, nil)
	fx.post("/v1/messages", "anthropic", anthropicLoopBody)
	response, body = fx.post("/v1/messages", "anthropic", strings.Replace(anthropicLoopBody, "continue", "other", 1))
	var anthropic struct {
		Type  string `json:"type"`
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &anthropic); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != 429 || anthropic.Type != "error" || anthropic.Error.Type != "rate_limit_error" ||
		anthropic.Error.Message != "loop detected: more than 1 requests in 60s; requests from this user are paused for 120s" {
		t.Fatalf("anthropic: %d %s", response.StatusCode, body)
	}
}

func TestLoopRejectionRetryAfter(t *testing.T) {
	fx, upstream, clock := loopFixture(t, loopSettings, &loopRecorder{}, nil)
	stream := strings.Replace(anthropicLoopBody, `"max_tokens":10,`, `"max_tokens":10,"stream":true,`, 1)
	for range 2 {
		fx.post("/v1/messages", "anthropic", stream)
	}
	response, body := fx.post("/v1/messages", "anthropic", stream)
	if response.StatusCode != 429 || response.Header.Get("Retry-After") != "90" || response.Header.Get("Content-Type") != "application/json" ||
		response.Header.Get("Cache-Control") != "no-store" || strings.Contains(body, "event:") {
		t.Fatalf("stream cut: %d %v %s", response.StatusCode, response.Header, body)
	}
	clock.Advance(30*time.Second + 500*time.Millisecond)
	response, body = fx.post("/v1/messages", "anthropic", stream)
	if response.Header.Get("Retry-After") != "60" || !strings.HasSuffix(comportaMessage(t, body), "paused for 60s") {
		t.Fatalf("retry: %v %s", response.Header, body)
	}
	if upstream.count() != 2 {
		t.Fatalf("a cut reached the provider: %d", upstream.count())
	}
}

func comportaMessage(t *testing.T, body string) string {
	t.Helper()
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		t.Fatal(err)
	}
	return parsed.Error.Message
}

func TestCountTokensBypassesLoop(t *testing.T) {
	recorder := &loopRecorder{}
	fx, _, _ := loopFixture(t, "    loop_guard:\n      similar_requests: 1\n      requests_per_minute: 1\n", recorder, nil)
	count := `{"model":"fake-claude","metadata":{"user_id":"u-1"},"messages":[{"role":"user","content":"` + testkit.Prompt("continue") + `"}]}`
	for range 5 {
		if response, body := fx.post("/v1/messages/count_tokens", "anthropic", count); response.StatusCode != 200 {
			t.Fatalf("count_tokens: %d %s", response.StatusCode, body)
		}
	}
	if response, _ := fx.post("/v1/messages", "anthropic", anthropicLoopBody); response.StatusCode != 200 {
		t.Fatal("count_tokens consumed the generation allowance")
	}
	if len(recorder.cuts) != 0 {
		t.Fatal("count_tokens recorded a cut")
	}
}

func TestLoopStorageFailureAnswers503(t *testing.T) {
	recorder := &loopRecorder{err: errors.New("disk")}
	fx, upstream, _ := loopFixture(t, loopSettings, recorder, nil)
	for range 2 {
		fx.post("/v1/messages", "anthropic", anthropicLoopBody)
	}
	response, body := fx.post("/v1/messages", "anthropic", anthropicLoopBody)
	if response.StatusCode != http.StatusServiceUnavailable || comportaCode(t, body) != "usage_storage_unavailable" || upstream.count() != 2 {
		t.Fatalf("storage failure: %d %s", response.StatusCode, body)
	}
	// The cooldown stays in memory; the retry is still refused, never forwarded.
	recorder.mu.Lock()
	recorder.err = nil
	recorder.mu.Unlock()
	if response, body = fx.post("/v1/messages", "anthropic", anthropicLoopBody); response.StatusCode != 429 || upstream.count() != 2 {
		t.Fatalf("after failure: %d %s", response.StatusCode, body)
	}
}

func TestLoopRejectionMessageFractions(t *testing.T) {
	d := loopguard.Decision{First: true, Remaining: 1500 * time.Millisecond, Episode: loopguard.Episode{
		Reason: loopguard.ReasonSimilar, Limit: 3, Window: 2500 * time.Millisecond, Cooldown: 1500 * time.Millisecond}}
	r := loopRejection(d)
	if r.RetryAfter != 2 || r.Message != "loop detected: 3 similar requests in 2.5s; requests from this user are paused for 1.5s" {
		t.Fatalf("fractions: %+v", r)
	}
}
