package proxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

func streamEvents(events ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Request-Id", "req-stream")
		for _, e := range events {
			_, _ = io.WriteString(w, e)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}
}

const (
	anthropicOverloadedEvent = "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"prompt echo\"}}\n\n"
	anthropicStartEvent      = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"
	anthropicStopEvents      = "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":1}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
)

func TestFallbackBeforeFirstChunk(t *testing.T) {
	fx, upstream := fallbackFixture(t, "model-b", map[string]http.HandlerFunc{
		"model-a": streamEvents(anthropicOverloadedEvent),
		"model-b": streamEvents(anthropicStartEvent, anthropicStopEvents),
	}, "")
	response, body := fx.post("/v1/messages", "anthropic", messages("model-a", true))
	if response.StatusCode != 200 || len(upstream.sent()) != 2 || strings.Contains(body, "overloaded") || !strings.Contains(body, "message_stop") {
		t.Fatalf("%d %s %v", response.StatusCode, body, upstream.sent())
	}
	if got := response.Header.Get(fallbackHeader); got != "model-a->model-b (529)" {
		t.Fatalf("header %q", got)
	}
	// An error class the parser does not recognize is never inferred as
	// transient from its text.
	fx, upstream = fallbackFixture(t, "model-b", map[string]http.HandlerFunc{
		"model-a": streamEvents("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded maybe\"}}\n\n"),
	}, "")
	response, _ = fx.post("/v1/messages", "anthropic", messages("model-a", true))
	if response.StatusCode != 502 || len(upstream.sent()) != 1 {
		t.Fatalf("unrecognized class fell back: %d %v", response.StatusCode, upstream.sent())
	}
}

func TestFallbackNeverAfterFirstChunk(t *testing.T) {
	for name, first := range map[string]string{
		"start without text": anthropicStartEvent,
		"comment":            ": keep-alive\n\n",
		"text":               "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"Ol\"}}\n\n",
	} {
		fx, upstream := fallbackFixture(t, "model-b", map[string]http.HandlerFunc{"model-a": streamEvents(first, anthropicOverloadedEvent)}, "")
		response, body := fx.post("/v1/messages", "anthropic", messages("model-a", true))
		if response.StatusCode != 200 || len(upstream.sent()) != 1 || !strings.Contains(body, `"comporta_code":"provider_overloaded"`) || strings.Contains(body, "prompt echo") {
			t.Errorf("%s: %d %s %v", name, response.StatusCode, body, upstream.sent())
		}
		if response.Header.Get(fallbackHeader) != "" {
			t.Errorf("%s: fallback header on a committed stream", name)
		}
	}
	// A stream that ends early without an error event is a protocol failure,
	// not a new trigger.
	fx, upstream := fallbackFixture(t, "model-b", map[string]http.HandlerFunc{"model-a": streamEvents()}, "")
	response, _ := fx.post("/v1/messages", "anthropic", messages("model-a", true))
	if response.StatusCode != 502 || len(upstream.sent()) != 1 {
		t.Fatalf("early end fell back: %d %v", response.StatusCode, upstream.sent())
	}
}

func TestFallbackFilteredUsageIsNotCommit(t *testing.T) {
	usageOnly := "data: {\"id\":\"c\",\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":0}}\n\n"
	rateLimited := "data: {\"error\":{\"type\":\"rate_limit_exceeded\",\"message\":\"slow down\"}}\n\n"
	done := "data: {\"id\":\"c\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"
	fx, upstream := fallbackFixture(t, "gpt-b", map[string]http.HandlerFunc{
		"gpt-a": streamEvents(usageOnly, rateLimited),
		"gpt-b": streamEvents(done),
	}, "")
	// The client did not ask for usage, so the usage chunk is dropped by the
	// gateway and nothing was delivered yet.
	response, body := fx.post("/v1/chat/completions", "openai", bodyFor(testkit.OpenAIStreamBody(), "gpt-a"))
	if response.StatusCode != 200 || len(upstream.sent()) != 2 || !strings.Contains(body, "[DONE]") || strings.Contains(body, "slow down") {
		t.Fatalf("%d %s %v", response.StatusCode, body, upstream.sent())
	}
	if got := response.Header.Get(fallbackHeader); got != "gpt-a->gpt-b (429)" {
		t.Fatalf("header %q", got)
	}
}

func TestFallbackClientCancel(t *testing.T) {
	var cancelled atomic.Int64
	started := make(chan struct{})
	hang := func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		cancelled.Store(time.Now().UnixNano())
	}
	fx, upstream := fallbackFixture(t, "model-b", map[string]http.HandlerFunc{"model-a": hang}, "")
	ctx, cancel := context.WithCancel(context.Background())
	request := fx.request(http.MethodPost, "/v1/messages", "anthropic", messages("model-a", true)).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if response, err := http.DefaultClient.Do(request); err == nil {
			_ = response.Body.Close()
		}
	}()
	<-started
	left := time.Now()
	cancel()
	<-done
	deadline := time.Now().Add(2 * time.Second)
	for cancelled.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if cancelled.Load() == 0 || time.Unix(0, cancelled.Load()).Sub(left) > time.Second {
		t.Fatal("the provider was not released within 1 s")
	}
	time.Sleep(100 * time.Millisecond)
	if len(upstream.sent()) != 1 {
		t.Fatalf("a cancelled request reached another model: %v", upstream.sent())
	}
	result := fx.sink.wait(t)
	if !result.ClientDisconnected || result.AttemptIndex != 1 {
		t.Fatalf("result %+v", result)
	}
}
