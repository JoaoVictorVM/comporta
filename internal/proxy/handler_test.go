package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	anthropicBody = `{"model":"fake-claude","max_tokens":32,"messages":[{"role":"user","content":"Ola"}],"stream":false,"future_option":{"enabled":true}}`
	openAIBody    = `{"model":"fake-gpt","messages":[{"role":"user","content":"Ola"}]}`
	countBody     = `{"model":"fake-claude","messages":[{"role":"user","content":"Ola"}],"system":"s"}`
)

func TestBodyLimitWithAndWithoutContentLength(t *testing.T) {
	upstream, server := startUpstream(t, nil)
	fx := newFixture(t, server.URL, fixtureOptions{})

	exact := func(size int) string {
		prefix := `{"model":"fake-gpt","messages":[],"p":"`
		suffix := `"}`
		return prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix
	}

	t.Run("at the limit", func(t *testing.T) {
		response, _ := fx.post("/v1/chat/completions", "openai", exact(10_000_000))
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status %d", response.StatusCode)
		}
	})
	before := upstream.count()

	t.Run("one byte over with a length", func(t *testing.T) {
		response, body := fx.post("/v1/chat/completions", "openai", exact(10_000_001))
		if response.StatusCode != http.StatusRequestEntityTooLarge || comportaCode(t, body) != "request_too_large" {
			t.Fatalf("status %d, body %s", response.StatusCode, body)
		}
		if !strings.Contains(body, "request body exceeds 10 MB limit") {
			t.Fatalf("message: %s", body)
		}
	})
	t.Run("one byte over, chunked", func(t *testing.T) {
		request := fx.request(http.MethodPost, "/v1/chat/completions", "openai", "x")
		request.Body = io.NopCloser(strings.NewReader(exact(10_000_001)))
		request.ContentLength = -1
		response, body := fx.do(request)
		if response.StatusCode != http.StatusRequestEntityTooLarge || comportaCode(t, body) != "request_too_large" {
			t.Fatalf("status %d, body %s", response.StatusCode, body)
		}
	})
	if upstream.count() != before {
		t.Fatal("an oversized body reached the provider")
	}
}

func TestUnknownModelAndFormatMismatch(t *testing.T) {
	upstream, server := startUpstream(t, nil)
	fx := newFixture(t, server.URL, fixtureOptions{})
	cases := []struct {
		name, path, format, body, code string
	}{
		{"unknown", "/v1/chat/completions", "openai", `{"model":"secret-model-name","messages":[]}`, "model_unknown"},
		{"anthropic model on openai route", "/v1/chat/completions", "openai", `{"model":"fake-claude","messages":[]}`, "provider_format_mismatch"},
		{"openai model on anthropic route", "/v1/messages", "anthropic", `{"model":"fake-gpt","max_tokens":1,"messages":[]}`, "provider_format_mismatch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response, body := fx.post(tc.path, tc.format, tc.body)
			if response.StatusCode != http.StatusBadRequest || comportaCode(t, body) != tc.code {
				t.Fatalf("status %d, body %s", response.StatusCode, body)
			}
			if strings.Contains(body, "secret-model-name") {
				t.Fatalf("value echoed: %s", body)
			}
		})
	}
	if upstream.count() != 0 {
		t.Fatal("a rejected request reached the provider")
	}
}

func TestLocalRejections(t *testing.T) {
	upstream, server := startUpstream(t, nil)
	fx := newFixture(t, server.URL, fixtureOptions{})
	t.Run("invalid JSON reports the offset without the body", func(t *testing.T) {
		text := `{"model":"canary-body-text",`
		response, body := fx.post("/v1/chat/completions", "openai", text)
		if response.StatusCode != http.StatusBadRequest || comportaCode(t, body) != "invalid_request" {
			t.Fatalf("status %d, body %s", response.StatusCode, body)
		}
		if !strings.Contains(body, "invalid JSON at byte") || strings.Contains(body, "canary-body-text") {
			t.Fatalf("message: %s", body)
		}
	})
	t.Run("content type", func(t *testing.T) {
		request := fx.request(http.MethodPost, "/v1/chat/completions", "openai", openAIBody)
		request.Header.Set("Content-Type", "text/plain")
		response, body := fx.do(request)
		if response.StatusCode != http.StatusUnsupportedMediaType || comportaCode(t, body) != "unsupported_media_type" {
			t.Fatalf("status %d, body %s", response.StatusCode, body)
		}
	})
	t.Run("content encoding", func(t *testing.T) {
		request := fx.request(http.MethodPost, "/v1/chat/completions", "openai", openAIBody)
		request.Header.Set("Content-Encoding", "gzip")
		response, body := fx.do(request)
		if response.StatusCode != http.StatusUnsupportedMediaType || comportaCode(t, body) != "unsupported_media_type" {
			t.Fatalf("status %d, body %s", response.StatusCode, body)
		}
	})
	t.Run("query", func(t *testing.T) {
		response, body := fx.post("/v1/chat/completions?x=1", "openai", openAIBody)
		if response.StatusCode != http.StatusBadRequest || comportaCode(t, body) != "invalid_request" {
			t.Fatalf("status %d, body %s", response.StatusCode, body)
		}
	})
	t.Run("authentication comes before reading the body", func(t *testing.T) {
		request := fx.request(http.MethodPost, "/v1/chat/completions", "openai", "not json")
		request.Header.Set("Authorization", "Bearer cmp_wrong")
		response, body := fx.do(request)
		if response.StatusCode != http.StatusUnauthorized || comportaCode(t, body) != "invalid_api_key" {
			t.Fatalf("status %d, body %s", response.StatusCode, body)
		}
	})
	t.Run("errors use the format of the route", func(t *testing.T) {
		_, body := fx.post("/v1/messages", "anthropic", `{`)
		var parsed struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(body), &parsed); err != nil || parsed.Type != "error" {
			t.Fatalf("body %s", body)
		}
	})
	if upstream.count() != 0 {
		t.Fatal("a rejected request reached the provider")
	}
}

func TestRequestDeadlineIncludesBody(t *testing.T) {
	upstream, server := startUpstream(t, nil)
	fx := newFixture(t, server.URL, fixtureOptions{listeners: "listeners:\n  non_streaming_timeout: 200ms\n"})
	target, _ := url.Parse(fx.server.URL)
	conn, err := net.Dial("tcp", target.Host)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	head := fmt.Sprintf("POST /v1/chat/completions HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: 100\r\n\r\n{", fx.key)
	if _, err := conn.Write([]byte(head)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	reply := make([]byte, 4096)
	n, err := conn.Read(reply)
	if err != nil {
		t.Fatalf("no answer to a stalled body: %v", err)
	}
	if !strings.HasPrefix(string(reply[:n]), "HTTP/1.1 504") || !strings.Contains(string(reply[:n]), "provider_timeout") {
		t.Fatalf("reply: %s", reply[:n])
	}
	if upstream.count() != 0 {
		t.Fatal("the request went upstream")
	}
}

func TestNonStreamingDeadlineCoversTheProvider(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	_, server := startUpstream(t, func(http.ResponseWriter, *http.Request) { <-release })
	fx := newFixture(t, server.URL, fixtureOptions{listeners: "listeners:\n  non_streaming_timeout: 200ms\n"})
	response, body := fx.post("/v1/chat/completions", "openai", openAIBody)
	if response.StatusCode != http.StatusGatewayTimeout || comportaCode(t, body) != "provider_timeout" {
		t.Fatalf("status %d, body %s", response.StatusCode, body)
	}
	result := fx.sink.wait(t)
	if result.ErrorClass != ClassTimeout || result.UpstreamStatus.Known {
		t.Fatalf("result %+v", result)
	}
}

func TestUpstreamHeaderTimeout(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	_, server := startUpstream(t, func(http.ResponseWriter, *http.Request) { <-release })
	fx := newFixture(t, server.URL, fixtureOptions{listeners: "listeners:\n  upstream_header_timeout: 150ms\n"})
	response, body := fx.post("/v1/chat/completions", "openai", openAIBody)
	if response.StatusCode != http.StatusGatewayTimeout || comportaCode(t, body) != "provider_timeout" {
		t.Fatalf("status %d, body %s", response.StatusCode, body)
	}
}

func TestProviderUnreachable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	fx := newFixture(t, "http://"+address, fixtureOptions{})
	response, body := fx.post("/v1/chat/completions", "openai", openAIBody)
	if response.StatusCode != http.StatusBadGateway || comportaCode(t, body) != "provider_unreachable" {
		t.Fatalf("status %d, body %s", response.StatusCode, body)
	}
	if !strings.Contains(body, "provider openai unreachable") || strings.Contains(body, address) {
		t.Fatalf("message: %s", body)
	}
	result := fx.sink.wait(t)
	if result.ErrorClass != ClassConnection || result.DownstreamStatus.Value != http.StatusBadGateway {
		t.Fatalf("result %+v", result)
	}
}

func TestNoImplicitPostRetry(t *testing.T) {
	var calls atomic.Int64
	_, server := startUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			t.Error("no hijacker")
			return
		}
		conn, _, err := hijacker.Hijack()
		if err == nil {
			_ = conn.Close()
		}
	})
	fx := newFixture(t, server.URL, fixtureOptions{})
	response, body := fx.post("/v1/chat/completions", "openai", openAIBody)
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, body %s", response.StatusCode, body)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("provider called %d times", got)
	}
	if results := fx.sink.wait(t); results.AttemptIndex != 1 {
		t.Fatalf("attempt %d", results.AttemptIndex)
	}
}

func TestRedirectNeverFollowed(t *testing.T) {
	var followed atomic.Bool
	_, elsewhere := startUpstream(t, func(http.ResponseWriter, *http.Request) { followed.Store(true) })
	_, server := startUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", elsewhere.URL)
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	fx := newFixture(t, server.URL, fixtureOptions{})
	response, body := fx.post("/v1/chat/completions", "openai", openAIBody)
	if response.StatusCode != http.StatusBadGateway || comportaCode(t, body) != "provider_redirect" {
		t.Fatalf("status %d, body %s", response.StatusCode, body)
	}
	if response.Header.Get("Location") != "" || strings.Contains(body, elsewhere.URL) {
		t.Fatal("the destination leaked")
	}
	if followed.Load() {
		t.Fatal("the redirect was followed")
	}
}

func TestForwardingUsesProviderCredentialOnly(t *testing.T) {
	upstream, server := startUpstream(t, nil)
	fx := newFixture(t, server.URL+"/v1", fixtureOptions{})
	request := fx.request(http.MethodPost, "/v1/messages", "anthropic", anthropicBody)
	request.Header.Set("Anthropic-Version", "2023-06-01")
	request.Header.Set("X-Forwarded-For", "9.9.9.9")
	request.Header.Set("Cookie", "a=b")
	request.Header.Set("X-Comporta-User", rawUserCanary)
	if response, body := fx.do(request); response.StatusCode != http.StatusOK {
		t.Fatalf("status %d, body %s", response.StatusCode, body)
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if upstream.path != "/v1/messages" {
		t.Fatalf("path %s", upstream.path)
	}
	if upstream.header.Get("X-Api-Key") != providerCanary || upstream.header.Get("Anthropic-Version") != "2023-06-01" {
		t.Fatalf("headers %v", upstream.header)
	}
	flat := fmt.Sprint(upstream.header)
	if strings.Contains(flat, fx.key) || strings.Contains(flat, rawUserCanary) || strings.Contains(flat, "9.9.9.9") || strings.Contains(flat, "a=b") {
		t.Fatalf("client data leaked upstream: %s", flat)
	}
	if string(upstream.body) != anthropicBody {
		t.Fatalf("body changed: %s", upstream.body)
	}
}

func TestStreamingBodyGetsOnlyTheDocumentedPatch(t *testing.T) {
	upstream, server := startUpstream(t, sseReply("data: [DONE]\n\n"))
	fx := newFixture(t, server.URL, fixtureOptions{})
	text := `{"model":"fake-gpt", "messages":[{"role":"user","content":"é"}],"n":1.50,"stream":true,"stream_options":{"future_option":1},"z":{"a":null}}`
	want := strings.Replace(text, `{"future_option":1}`, `{"future_option":1,"include_usage":true}`, 1)
	if response, body := fx.post("/v1/chat/completions", "openai", text); response.StatusCode != http.StatusOK {
		t.Fatalf("status %d, body %s", response.StatusCode, body)
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if string(upstream.body) != want {
		t.Fatalf("got  %s\nwant %s", upstream.body, want)
	}
}

func TestPanicBeforeAndAfterCommit(t *testing.T) {
	t.Run("before the commit", func(t *testing.T) {
		upstream, server := startUpstream(t, nil)
		var logs strings.Builder
		fx := newFixture(t, server.URL, fixtureOptions{logs: &logs, hooks: Hooks{
			BeforeSend: func(context.Context, *Envelope, RequestContent) *Rejection { panic("secret-panic-value") },
		}})
		response, body := fx.post("/v1/chat/completions", "openai", openAIBody)
		if response.StatusCode != http.StatusInternalServerError || comportaCode(t, body) != "internal_error" {
			t.Fatalf("status %d, body %s", response.StatusCode, body)
		}
		if strings.Contains(body, "secret-panic-value") || strings.Contains(logs.String(), "secret-panic-value") {
			t.Fatalf("panic value leaked: %s %s", body, logs.String())
		}
		if !strings.Contains(logs.String(), "request handler panicked") {
			t.Fatalf("logs: %s", logs.String())
		}
		if upstream.count() != 0 {
			t.Fatal("request forwarded")
		}
		if again, _ := fx.do(fx.request(http.MethodGet, "/v1/models", "openai", "")); again.StatusCode != http.StatusOK {
			t.Fatal("the panic affected later requests")
		}
	})
	t.Run("after a stream was committed", func(t *testing.T) {
		_, server := startUpstream(t, sseReply("data: {\"choices\":[{}]}\n\ndata: {\"choices\":[{}]}\n\n"))
		var seen atomic.Int64
		fx := newFixture(t, server.URL, fixtureOptions{hooks: Hooks{
			StreamEvent: func(context.Context, *Envelope, []byte) *Rejection {
				if seen.Add(1) == 2 {
					panic("secret-panic-value")
				}
				return nil
			},
		}})
		response, body := fx.post("/v1/chat/completions", "openai", `{"model":"fake-gpt","messages":[],"stream":true}`)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("status %d", response.StatusCode)
		}
		if !strings.Contains(body, "internal_error") || strings.Contains(body, "secret-panic-value") {
			t.Fatalf("body %s", body)
		}
		result := fx.sink.wait(t)
		if result.ErrorCode != CodeInternalError || !result.Committed || result.DownstreamStatus.Value != http.StatusOK {
			t.Fatalf("result %+v", result)
		}
	})
	t.Run("after a normal response started", func(t *testing.T) {
		_, server := startUpstream(t, nil)
		fx := newFixture(t, server.URL, fixtureOptions{})
		operation := &operation{
			handler: fx.handler, w: newTrackedWriter(discardWriter{}), raw: discardWriter{}, snapshot: fx.config.Capture(),
		}
		operation.w.WriteHeader(http.StatusOK)
		defer func() {
			if value := recover(); value != http.ErrAbortHandler {
				t.Fatalf("recovered %v", value)
			}
		}()
		func() {
			defer operation.finish()
			panic("secret-panic-value")
		}()
		t.Fatal("the connection must be aborted")
	})
}

type discardWriter struct{}

func (discardWriter) Header() http.Header         { return http.Header{} }
func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
func (discardWriter) WriteHeader(int)             {}

func TestHooksRunInOrderAndCanStop(t *testing.T) {
	var order []string
	upstream, server := startUpstream(t, nil)
	fx := newFixture(t, server.URL, fixtureOptions{hooks: Hooks{
		Prepare: func(_ context.Context, envelope *Envelope, content RequestContent) *Rejection {
			order = append(order, "prepare:"+envelope.ModelRequested+":"+fmt.Sprint(len(content.Body) > 0))
			return nil
		},
		BeforeSend: func(context.Context, *Envelope, RequestContent) *Rejection {
			order = append(order, "before")
			return nil
		},
		ResponseBody: func(_ context.Context, _ *Envelope, body []byte) *Rejection {
			order = append(order, "response:"+fmt.Sprint(len(body) > 0))
			return nil
		},
	}})
	if response, _ := fx.post("/v1/chat/completions", "openai", openAIBody); response.StatusCode != http.StatusOK {
		t.Fatalf("status %d", response.StatusCode)
	}
	if got := strings.Join(order, ","); got != "prepare:fake-gpt:true,before,response:true" {
		t.Fatalf("order %s", got)
	}

	blocked := newFixture(t, server.URL, fixtureOptions{hooks: Hooks{
		Prepare: func(context.Context, *Envelope, RequestContent) *Rejection {
			return &Rejection{Status: http.StatusTooManyRequests, Code: "policy", Class: ClassRateLimit, Message: "stopped"}
		},
	}})
	before := upstream.count()
	if response, _ := blocked.post("/v1/chat/completions", "openai", openAIBody); response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status %d", response.StatusCode)
	}
	if upstream.count() != before {
		t.Fatal("a stage could not stop the request")
	}
}

func TestPrivacyCanaries(t *testing.T) {
	const canary = "canary-prompt-text"
	_, server := startUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"you sent ` + canary + `"}}`))
	})
	var logs strings.Builder
	fx := newFixture(t, server.URL, fixtureOptions{logs: &logs})
	response, body := fx.post("/v1/chat/completions", "openai",
		`{"model":"fake-gpt","messages":[{"role":"user","content":"`+canary+`"}],"user":"`+rawUserCanary+`"}`)
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d", response.StatusCode)
	}
	result := fx.sink.wait(t)
	serialized, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{
		"response": body, "logs": logs.String(), "result": string(serialized), "formatted": fmt.Sprintf("%+v", result),
	} {
		for _, secret := range []string{canary, rawUserCanary, providerCanary, fx.key} {
			if strings.Contains(text, secret) {
				t.Errorf("%s exposes %q: %s", name, secret, text)
			}
		}
	}
	entries, err := os.ReadDir(fx.config.Capture().DataDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		content, readErr := os.ReadFile(fx.config.Capture().DataDir() + "/" + entry.Name())
		if readErr == nil && strings.Contains(string(content), canary) {
			t.Errorf("%s holds request content", entry.Name())
		}
	}
}
