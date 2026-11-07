package proxy

import (
	"net/http"
	"strings"
	"testing"
)

func TestNonStreamingBodyPreserved(t *testing.T) {
	original := `{ "id":"x", "unknown" : {"a":1.50}, "text":"é",  "usage":{"input_tokens":10,"output_tokens":2,"cache_read_input_tokens":3} }` + "\n"
	_, server := startUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Request-Id", "req_1")
		w.Header().Set("Anthropic-Ratelimit-Requests-Remaining", "5")
		w.Header().Set("Set-Cookie", "tracking=1")
		w.Header().Set("X-Internal", "value")
		_, _ = w.Write([]byte(original))
	})
	fx := newFixture(t, server.URL, fixtureOptions{})
	response, body := fx.post("/v1/messages", "anthropic", anthropicBody)
	if response.StatusCode != http.StatusOK || body != original {
		t.Fatalf("status %d, body %q", response.StatusCode, body)
	}
	if response.Header.Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("content type %q", response.Header.Get("Content-Type"))
	}
	if response.Header.Get("Request-Id") != "req_1" || response.Header.Get("Anthropic-Ratelimit-Requests-Remaining") != "5" {
		t.Fatalf("allowed headers lost: %v", response.Header)
	}
	if response.Header.Get("Set-Cookie") != "" || response.Header.Get("X-Internal") != "" {
		t.Fatalf("unknown headers forwarded: %v", response.Header)
	}
	result := fx.sink.wait(t)
	if !result.UsageComplete || !result.TerminalSeen || !result.Committed {
		t.Fatalf("result %+v", result)
	}
	if result.Usage.Input.Value != 10 || result.Usage.Output.Value != 2 || result.Usage.CacheRead.Value != 3 || result.Usage.CacheWrite.Known {
		t.Fatalf("usage %+v", result.Usage)
	}
	if result.Provider != "anthropic" || result.ModelUsed != "fake-claude" || result.UpstreamStatus.Value != 200 || result.DownstreamStatus.Value != 200 {
		t.Fatalf("result %+v", result)
	}
	if !result.UpstreamTTFB.Known || !result.ClientTTFB.Known || !result.UpstreamHeaders.Known {
		t.Fatalf("timings %+v", result)
	}
}

func TestResponseWithoutUsageIsDelivered(t *testing.T) {
	_, server := startUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","usage":null}`))
	})
	fx := newFixture(t, server.URL, fixtureOptions{})
	response, body := fx.post("/v1/chat/completions", "openai", openAIBody)
	if response.StatusCode != http.StatusOK || body != `{"id":"x","usage":null}` {
		t.Fatalf("status %d, body %s", response.StatusCode, body)
	}
	result := fx.sink.wait(t)
	if result.UsageComplete || result.Usage.Input.Known || result.Usage.Output.Known {
		t.Fatalf("missing usage must stay unknown: %+v", result)
	}
}

func TestResponseLimitBeforeCommit(t *testing.T) {
	_, server := startUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		chunk := strings.Repeat(" ", 1<<20)
		for range maxResponseBytes/(1<<20) + 2 {
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
		}
	})
	fx := newFixture(t, server.URL, fixtureOptions{})
	response, body := fx.post("/v1/chat/completions", "openai", openAIBody)
	if response.StatusCode != http.StatusBadGateway || comportaCode(t, body) != "invalid_upstream_response" {
		t.Fatalf("status %d, body %.200s", response.StatusCode, body)
	}
}

func TestInvalidUpstreamResponses(t *testing.T) {
	cases := map[string]http.HandlerFunc{
		"not JSON": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`<html>`))
		},
		"wrong content type": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(`{}`))
		},
		"negative usage": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":-1,"completion_tokens":1}}`))
		},
		"usage overflow": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":9223372036854775808}}`))
		},
		"cached over total": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"prompt_tokens_details":{"cached_tokens":2}}}`))
		},
		"truncated body": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Length", "100")
			_, _ = w.Write([]byte(`{}`))
		},
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			_, server := startUpstream(t, reply)
			fx := newFixture(t, server.URL, fixtureOptions{})
			response, body := fx.post("/v1/chat/completions", "openai", openAIBody)
			if response.StatusCode != http.StatusBadGateway || comportaCode(t, body) != "invalid_upstream_response" {
				t.Fatalf("status %d, body %s", response.StatusCode, body)
			}
			if result := fx.sink.wait(t); result.ErrorClass != ClassProtocol {
				t.Fatalf("result %+v", result)
			}
		})
	}
}

func TestProviderErrorNeverEchoesCanary(t *testing.T) {
	const canary = "canary-echoed-prompt"
	cases := []struct {
		status int
		code   string
		class  ErrorClass
	}{
		{400, "provider_error", ClassInvalidRequest},
		{401, "provider_error", ClassAuthentication},
		{403, "provider_error", ClassAuthentication},
		{404, "provider_error", ClassInvalidRequest},
		{429, "provider_rate_limited", ClassRateLimit},
		{500, "provider_error", ClassUpstream5xx},
		{503, "provider_error", ClassUpstream5xx},
		{529, "provider_overloaded", ClassOverload},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			_, server := startUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "7")
				w.Header().Set("X-Request-Id", "req_2")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"type":"error","error":{"type":"x","message":"` + canary + `"}}`))
			})
			fx := newFixture(t, server.URL, fixtureOptions{})
			response, body := fx.post("/v1/messages", "anthropic", anthropicBody)
			if response.StatusCode != tc.status || comportaCode(t, body) != tc.code {
				t.Fatalf("status %d, body %s", response.StatusCode, body)
			}
			if strings.Contains(body, canary) {
				t.Fatalf("provider text echoed: %s", body)
			}
			if !strings.HasPrefix(body, `{"type":"error","error":`) {
				t.Fatalf("format: %s", body)
			}
			if response.Header.Get("X-Request-Id") != "req_2" {
				t.Fatalf("request id lost: %v", response.Header)
			}
			keepsRetry := tc.status == 429 || tc.status == 529
			if (response.Header.Get("Retry-After") == "7") != keepsRetry {
				t.Fatalf("retry-after %q", response.Header.Get("Retry-After"))
			}
			result := fx.sink.wait(t)
			if result.ErrorClass != tc.class || result.UpstreamStatus.Value != tc.status || result.UsageComplete {
				t.Fatalf("result %+v", result)
			}
		})
	}
}

func TestInvalidRetryAfterIsDropped(t *testing.T) {
	_, server := startUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "soon")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	fx := newFixture(t, server.URL, fixtureOptions{})
	response, _ := fx.post("/v1/chat/completions", "openai", openAIBody)
	if response.StatusCode != 429 || response.Header.Get("Retry-After") != "" {
		t.Fatalf("status %d, header %q", response.StatusCode, response.Header.Get("Retry-After"))
	}
}

func TestCountTokensPassesThrough(t *testing.T) {
	upstream, server := startUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":10}`))
	})
	fx := newFixture(t, server.URL, fixtureOptions{})
	response, body := fx.post("/v1/messages/count_tokens", "anthropic", countBody)
	if response.StatusCode != http.StatusOK || body != `{"input_tokens":10}` {
		t.Fatalf("status %d, body %s", response.StatusCode, body)
	}
	upstream.mu.Lock()
	path := upstream.path
	upstream.mu.Unlock()
	if path != "/v1/messages/count_tokens" {
		t.Fatalf("path %s", path)
	}
}
