package fakeprovider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func startFake(t *testing.T, options Options) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(New(options))
	t.Cleanup(server.Close)
	return server
}

func post(t *testing.T, url, body string) (*http.Response, string) {
	t.Helper()
	response, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
	return response, string(data)
}

func TestBothFormatsAndCountTokens(t *testing.T) {
	server := startFake(t, Options{})
	cases := map[string]struct {
		path, body, want string
	}{
		"anthropic": {"/v1/messages", `{"model":"fake-claude"}`, `"input_tokens":10,"output_tokens":2`},
		"openai":    {"/v1/chat/completions", `{"model":"fake-gpt"}`, `"prompt_tokens":10,"completion_tokens":2,"total_tokens":12`},
		"count":     {"/v1/messages/count_tokens", `{"model":"fake-claude"}`, `{"input_tokens":10}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			response, body := post(t, server.URL+tc.path, tc.body)
			if response.StatusCode != 200 || !strings.Contains(body, tc.want) || !json.Valid([]byte(body)) {
				t.Fatalf("status %d, body %s", response.StatusCode, body)
			}
		})
	}
	t.Run("models", func(t *testing.T) {
		response, err := http.Get(server.URL + "/v1/models")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		data, _ := io.ReadAll(response.Body)
		if !strings.Contains(string(data), "fake-gpt") {
			t.Fatalf("body %s", data)
		}
	})
	t.Run("streams", func(t *testing.T) {
		_, anthropic := post(t, server.URL+"/v1/messages", `{"model":"m","stream":true}`)
		for _, name := range []string{"message_start", "content_block_delta", "message_delta", "message_stop"} {
			if !strings.Contains(anthropic, "event: "+name+"\n") {
				t.Fatalf("missing %s: %s", name, anthropic)
			}
		}
		_, plain := post(t, server.URL+"/v1/chat/completions", `{"model":"m","stream":true}`)
		if strings.Contains(plain, `"usage"`) || !strings.HasSuffix(plain, "data: [DONE]\n\n") {
			t.Fatalf("plain stream: %s", plain)
		}
		_, withUsage := post(t, server.URL+"/v1/chat/completions", `{"model":"m","stream":true,"stream_options":{"include_usage":true}}`)
		if !strings.Contains(withUsage, `"choices":[],"usage":{"prompt_tokens":10`) {
			t.Fatalf("usage stream: %s", withUsage)
		}
	})
	t.Run("deterministic", func(t *testing.T) {
		_, first := post(t, server.URL+"/v1/messages", `{"model":"m","stream":true}`)
		_, second := post(t, server.URL+"/v1/messages", `{"model":"m","stream":true}`)
		if first != second {
			t.Fatal("two runs differ")
		}
	})
	t.Run("unknown route", func(t *testing.T) {
		if response, _ := post(t, server.URL+"/v1/files", `{}`); response.StatusCode != 404 {
			t.Fatalf("status %d", response.StatusCode)
		}
	})
}

func TestFailureScenariosDeterministic(t *testing.T) {
	t.Run("rate limit", func(t *testing.T) {
		server := startFake(t, Options{Scenario: ScenarioRateLimit})
		response, body := post(t, server.URL+"/v1/messages", `{"model":"m"}`)
		if response.StatusCode != 429 || response.Header.Get("Retry-After") != "1" || !strings.HasPrefix(body, `{"type":"error"`) {
			t.Fatalf("status %d, body %s", response.StatusCode, body)
		}
		_, openai := post(t, server.URL+"/v1/chat/completions", `{"model":"m"}`)
		if !strings.HasPrefix(openai, `{"error":`) {
			t.Fatalf("body %s", openai)
		}
	})
	t.Run("overload", func(t *testing.T) {
		server := startFake(t, Options{Scenario: ScenarioOverload})
		if response, _ := post(t, server.URL+"/v1/messages", `{"model":"m"}`); response.StatusCode != 529 {
			t.Fatalf("status %d", response.StatusCode)
		}
	})
	t.Run("disconnect mid-stream", func(t *testing.T) {
		server := startFake(t, Options{Scenario: ScenarioDisconnectMidstream})
		_, body := post(t, server.URL+"/v1/messages", `{"model":"m","stream":true}`)
		if !strings.Contains(body, "message_start") || strings.Contains(body, "message_stop") {
			t.Fatalf("body %s", body)
		}
	})
	t.Run("slow stream uses the injected wait", func(t *testing.T) {
		var waits int
		server := startFake(t, Options{Scenario: ScenarioSlowStream, Sleep: func(context.Context, time.Duration) error {
			waits++
			return nil
		}})
		_, body := post(t, server.URL+"/v1/chat/completions", `{"model":"m","stream":true}`)
		if waits != 4 || !strings.HasSuffix(body, "data: [DONE]\n\n") {
			t.Fatalf("waits %d, body %s", waits, body)
		}
	})
	t.Run("a request cannot select a scenario", func(t *testing.T) {
		server := startFake(t, Options{})
		request, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/messages?scenario=overload", strings.NewReader(`{"model":"m","scenario":"overload"}`))
		request.Header.Set("X-Scenario", "overload")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatalf("status %d", response.StatusCode)
		}
	})
	t.Run("invalid scenario name", func(t *testing.T) {
		if _, err := ParseScenario("nope"); err == nil {
			t.Fatal("accepted")
		}
		for _, scenario := range Scenarios() {
			if _, err := ParseScenario(string(scenario)); err != nil {
				t.Fatal(err)
			}
		}
	})
}

func TestFakeObservesCancellationWithinOneSecond(t *testing.T) {
	for _, scenario := range []Scenario{ScenarioHeaderTimeout, ScenarioSlowStream} {
		t.Run(string(scenario), func(t *testing.T) {
			canceled := make(chan time.Time, 1)
			started := make(chan struct{}, 1)
			server := startFake(t, Options{
				Scenario:  scenario,
				SlowDelay: time.Minute,
				OnRequest: func(Request) { started <- struct{}{} },
				OnCancel:  func() { canceled <- time.Now() },
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			request, _ := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/messages", strings.NewReader(`{"model":"m","stream":true}`))
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				if response, err := http.DefaultClient.Do(request); err == nil {
					_, _ = io.Copy(io.Discard, response.Body)
					_ = response.Body.Close()
				}
			}()
			<-started
			time.Sleep(50 * time.Millisecond)
			canceledAt := time.Now()
			cancel()
			select {
			case at := <-canceled:
				if at.Sub(canceledAt) > time.Second {
					t.Fatalf("observed after %s", at.Sub(canceledAt))
				}
			case <-time.After(3 * time.Second):
				t.Fatal("cancellation not observed")
			}
			<-finished
		})
	}
}

func TestObserverSeesExactBytes(t *testing.T) {
	const body = `{ "model":"m",  "unknown": 1.50 }`
	got := make(chan Request, 1)
	server := startFake(t, Options{OnRequest: func(request Request) { got <- request }})
	post(t, server.URL+"/v1/messages", body)
	request := <-got
	if string(request.Body) != body || request.Path != "/v1/messages" || request.Method != http.MethodPost {
		t.Fatalf("request %+v", request)
	}
}

func TestAgentLoopScenario(t *testing.T) {
	loop := startFake(t, Options{Scenario: ScenarioAgentLoop})
	plain := startFake(t, Options{})
	for _, anthropic := range []bool{true, false} {
		path, model := "/v1/chat/completions", "fake-gpt"
		if anthropic {
			path, model = "/v1/messages", "fake-claude"
		}
		agent := NewAgentLoop(anthropic, model, "u-1", "check the status")
		var sizes []int
		for turn := range 3 {
			body := agent.Body(false)
			sizes = append(sizes, len(body))
			response, data := post(t, loop.URL+path, string(body))
			if response.StatusCode != 200 || !json.Valid([]byte(data)) {
				t.Fatalf("turn %d: %d %s", turn, response.StatusCode, data)
			}
			if err := agent.Advance([]byte(data), LoopToolResult); err != nil {
				t.Fatalf("turn %d: %v", turn, err)
			}
		}
		if agent.Turns() != 3 || !(sizes[0] < sizes[1] && sizes[1] < sizes[2]) {
			t.Fatalf("context does not grow: %v", sizes)
		}
		for _, id := range []string{LoopToolUseID, LoopToolCallID} {
			if (id == LoopToolUseID) == anthropic && !strings.Contains(string(agent.Body(false)), id) {
				t.Fatalf("stable id %s missing", id)
			}
		}
		// The scenario is chosen at setup: the same request gets plain text elsewhere.
		_, data := post(t, plain.URL+path, string(agent.Body(false)))
		if err := agent.Advance([]byte(data), LoopToolResult); err == nil {
			t.Fatal("success scenario answered with a tool call")
		}
		_, stream := post(t, loop.URL+path, string(agent.Body(true)))
		for _, line := range strings.Split(stream, "\n") {
			if payload, ok := strings.CutPrefix(line, "data: "); ok && payload != "[DONE]" && !json.Valid([]byte(payload)) {
				t.Fatalf("invalid SSE payload %q", payload)
			}
		}
		if !strings.Contains(stream, LoopToolName) {
			t.Fatalf("stream without the tool call: %s", stream)
		}
	}
	if _, err := ParseScenario("agent-loop"); err != nil {
		t.Fatal(err)
	}
}
