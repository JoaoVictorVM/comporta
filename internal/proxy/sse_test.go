package proxy

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	streamOpenAIBody    = `{"model":"fake-gpt","messages":[{"role":"user","content":"x"}],"stream":true}`
	streamAnthropicBody = `{"model":"fake-claude","max_tokens":5,"messages":[{"role":"user","content":"x"}],"stream":true}`
	usageChunk          = "data: {\"id\":\"c\",\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12}}\n\n"
	deltaChunk          = "data: {\"id\":\"c\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":null}\n\n"
	doneChunk           = "data: [DONE]\n\n"
)

// sseReply writes the text as one stream response.
func sseReply(text string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(text))
	}
}

func parseAll(t *testing.T, chunks ...string) []sseEvent {
	t.Helper()
	parser := newSSEParser()
	var events []sseEvent
	for _, chunk := range chunks {
		parser.Feed([]byte(chunk))
		for {
			event, ok, err := parser.Next()
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				break
			}
			event.Raw = append([]byte(nil), event.Raw...)
			events = append(events, event)
		}
	}
	return events
}

func TestSSEFlushBeforeNextEvent(t *testing.T) {
	release := make(chan struct{})
	_, server := startUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(deltaChunk))
		w.(http.Flusher).Flush()
		<-release
		_, _ = w.Write([]byte(doneChunk))
	})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	fx := newFixture(t, server.URL, fixtureOptions{})
	response, err := http.DefaultClient.Do(fx.request(http.MethodPost, "/v1/chat/completions", "openai", streamOpenAIBody))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.Header.Get("Cache-Control") != "no-cache" || response.Header.Get("X-Accel-Buffering") != "no" {
		t.Fatalf("headers %v", response.Header)
	}
	got := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(response.Body).ReadString('\n')
		got <- line
	}()
	select {
	case line := <-got:
		if !strings.Contains(line, `"content":"hi"`) {
			t.Fatalf("line %q", line)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the event was held until the next one")
	}
	close(release)
}

func TestMultilineCRLFAndFragmentedEvents(t *testing.T) {
	text := "event: a\r\ndata: one\r\ndata: two\r\n\r\ndata: lf\n\n: comment\n\ndata: cr\r\rid: 9\ndata: end\n\n"
	whole := parseAll(t, text)
	var names []string
	for _, event := range whole {
		names = append(names, string(event.Data))
	}
	got := strings.Join(names, "|")
	if got != "one\ntwo|lf|||cr|end" {
		t.Fatalf("data %q", got)
	}
	if whole[0].Name != "a" {
		t.Fatalf("name %q", whole[0].Name)
	}
	if !whole[2].Comment {
		t.Fatalf("event 2 %+v", whole[2])
	}
	var rejoined strings.Builder
	for _, event := range whole {
		rejoined.Write(event.Raw)
	}
	if rejoined.String() != text {
		t.Fatalf("bytes changed: %q", rejoined.String())
	}

	for size := 1; size < 7; size++ {
		var chunks []string
		for i := 0; i < len(text); i += size {
			chunks = append(chunks, text[i:min(i+size, len(text))])
		}
		parts := parseAll(t, chunks...)
		if len(parts) != len(whole) {
			t.Fatalf("chunk size %d: %d events", size, len(parts))
		}
		for i := range parts {
			if string(parts[i].Raw) != string(whole[i].Raw) || string(parts[i].Data) != string(whole[i].Data) {
				t.Fatalf("chunk size %d, event %d: %q", size, i, parts[i].Raw)
			}
		}
	}
}

func TestByteOrderMarkDoesNotHideFirstField(t *testing.T) {
	events := parseAll(t, "\xef\xbb\xbfdata: x\n\n")
	if len(events) != 1 || string(events[0].Data) != "x" {
		t.Fatalf("events %+v", events)
	}
}

func TestHiddenOpenAIUsageOnlyChunk(t *testing.T) {
	for name, body := range map[string]string{
		"omitted": streamOpenAIBody,
		"false":   `{"model":"fake-gpt","messages":[],"stream":true,"stream_options":{"include_usage":false}}`,
		"null":    `{"model":"fake-gpt","messages":[],"stream":true,"stream_options":null}`,
		"other":   `{"model":"fake-gpt","messages":[],"stream":true,"stream_options":{"future":1}}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, server := startUpstream(t, sseReply(deltaChunk+usageChunk+doneChunk))
			fx := newFixture(t, server.URL, fixtureOptions{})
			response, got := fx.post("/v1/chat/completions", "openai", body)
			if response.StatusCode != http.StatusOK || got != deltaChunk+doneChunk {
				t.Fatalf("status %d, body %q", response.StatusCode, got)
			}
			result := fx.sink.wait(t)
			if !result.UsageComplete || result.Usage.Input.Value != 10 || result.Usage.Output.Value != 2 {
				t.Fatalf("usage must still be recorded: %+v", result)
			}
		})
	}
}

func TestExplicitUsageIsVisible(t *testing.T) {
	_, server := startUpstream(t, sseReply(deltaChunk+usageChunk+doneChunk))
	fx := newFixture(t, server.URL, fixtureOptions{})
	_, got := fx.post("/v1/chat/completions", "openai", `{"model":"fake-gpt","messages":[],"stream":true,"stream_options":{"include_usage":true}}`)
	if got != deltaChunk+usageChunk+doneChunk {
		t.Fatalf("body %q", got)
	}
	if result := fx.sink.wait(t); !result.UsageComplete {
		t.Fatalf("result %+v", result)
	}
}

func TestMixedDeltaUsageIsNotDropped(t *testing.T) {
	mixed := "data: {\"choices\":[{\"delta\":{\"content\":\"x\"}}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n"
	_, server := startUpstream(t, sseReply(mixed+doneChunk))
	fx := newFixture(t, server.URL, fixtureOptions{})
	if _, got := fx.post("/v1/chat/completions", "openai", streamOpenAIBody); got != mixed+doneChunk {
		t.Fatalf("body %q", got)
	}
	if result := fx.sink.wait(t); result.Usage.Input.Value != 1 {
		t.Fatalf("result %+v", result)
	}
}

func TestUnknownEventsAndHeartbeatsPreserved(t *testing.T) {
	text := ": heartbeat\n\nevent: future\ndata: not-json\nretry: 5\n\n" + deltaChunk + doneChunk
	_, server := startUpstream(t, sseReply(text))
	fx := newFixture(t, server.URL, fixtureOptions{})
	if _, got := fx.post("/v1/chat/completions", "openai", streamOpenAIBody); got != text {
		t.Fatalf("body %q", got)
	}
}

func TestAnthropicStreamBytesAndUsage(t *testing.T) {
	text := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":1,\"cache_read_input_tokens\":4}}}\n\n" +
		"event: ping\ndata: {\"type\":\"ping\"}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hi\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":5}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":7}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	_, server := startUpstream(t, sseReply(text))
	fx := newFixture(t, server.URL, fixtureOptions{})
	response, got := fx.post("/v1/messages", "anthropic", streamAnthropicBody)
	if response.StatusCode != http.StatusOK || got != text {
		t.Fatalf("status %d, body %q", response.StatusCode, got)
	}
	result := fx.sink.wait(t)
	if !result.UsageComplete || !result.TerminalSeen {
		t.Fatalf("result %+v", result)
	}
	if result.Usage.Input.Value != 10 || result.Usage.Output.Value != 7 || result.Usage.CacheRead.Value != 4 || result.Usage.CacheWrite.Known {
		t.Fatalf("usage %+v", result.Usage)
	}
}

func TestMidStreamFailureUsesClientFormat(t *testing.T) {
	const canary = "canary-stream-error"
	cases := []struct {
		name, path, format, body, text, code, prefix string
	}{
		{
			"anthropic error event", "/v1/messages", "anthropic", streamAnthropicBody,
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3}}}\n\nevent: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"" + canary + "\"}}\n\n",
			"provider_overloaded", "event: error\ndata: {\"type\":\"error\",\"error\":",
		},
		{
			"openai error object", "/v1/chat/completions", "openai", streamOpenAIBody,
			deltaChunk + "data: {\"error\":{\"message\":\"" + canary + "\",\"type\":\"server_error\"}}\n\n",
			"provider_error", "data: {\"error\":",
		},
		{
			"anthropic early end", "/v1/messages", "anthropic", streamAnthropicBody,
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":3}}}\n\n",
			"invalid_upstream_response", "event: error\ndata: {\"type\":\"error\",\"error\":",
		},
		{
			"openai early end", "/v1/chat/completions", "openai", streamOpenAIBody,
			deltaChunk,
			"invalid_upstream_response", "data: {\"error\":",
		},
		{
			"invalid usage", "/v1/chat/completions", "openai", streamOpenAIBody,
			deltaChunk + "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":-5}}\n\n" + doneChunk,
			"invalid_upstream_response", "data: {\"error\":",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, server := startUpstream(t, sseReply(tc.text))
			fx := newFixture(t, server.URL, fixtureOptions{})
			response, got := fx.post(tc.path, tc.format, tc.body)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("a committed stream cannot change status: %d", response.StatusCode)
			}
			if strings.Contains(got, canary) {
				t.Fatalf("provider text echoed: %s", got)
			}
			if !strings.Contains(got, tc.prefix) || !strings.Contains(got, tc.code) {
				t.Fatalf("body %q", got)
			}
			if strings.Contains(got, "[DONE]") || strings.Contains(got, "message_stop") {
				t.Fatalf("a failed stream must not look finished: %q", got)
			}
			result := fx.sink.wait(t)
			if result.ErrorCode != Code(tc.code) || result.UsageComplete || !result.Committed || result.DownstreamStatus.Value != 200 {
				t.Fatalf("result %+v", result)
			}
		})
	}
}

func TestFailureBeforeFirstEventKeepsHTTPStatus(t *testing.T) {
	cases := map[string]struct {
		reply  http.HandlerFunc
		status int
		code   string
	}{
		"wrong content type": {func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))
		}, 502, "invalid_upstream_response"},
		"empty stream": {sseReply(""), 502, "invalid_upstream_response"},
		"rate limit":   {func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(429) }, 429, "provider_rate_limited"},
		"error event first": {sseReply("data: {\"error\":{\"type\":\"rate_limit_error\",\"message\":\"m\"}}\n\n"),
			429, "provider_rate_limited"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, server := startUpstream(t, tc.reply)
			fx := newFixture(t, server.URL, fixtureOptions{})
			response, got := fx.post("/v1/chat/completions", "openai", streamOpenAIBody)
			if response.StatusCode != tc.status || comportaCode(t, got) != tc.code {
				t.Fatalf("status %d, body %s", response.StatusCode, got)
			}
		})
	}
}

func TestIdleTimeoutAndSlowClient(t *testing.T) {
	t.Run("a silent provider ends the stream", func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		_, server := startUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(deltaChunk))
			w.(http.Flusher).Flush()
			<-release
		})
		fx := newFixture(t, server.URL, fixtureOptions{listeners: "listeners:\n  streaming_idle_timeout: 200ms\n"})
		began := time.Now()
		_, got := fx.post("/v1/chat/completions", "openai", streamOpenAIBody)
		if elapsed := time.Since(began); elapsed > 3*time.Second {
			t.Fatalf("took %s", elapsed)
		}
		if !strings.Contains(got, "provider_timeout") {
			t.Fatalf("body %q", got)
		}
		if result := fx.sink.wait(t); result.ErrorClass != ClassTimeout {
			t.Fatalf("result %+v", result)
		}
	})
	t.Run("heartbeats count as activity", func(t *testing.T) {
		_, server := startUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			for range 6 {
				_, _ = w.Write([]byte(": ping\n\n"))
				w.(http.Flusher).Flush()
				time.Sleep(60 * time.Millisecond)
			}
			_, _ = w.Write([]byte(doneChunk))
		})
		fx := newFixture(t, server.URL, fixtureOptions{listeners: "listeners:\n  streaming_idle_timeout: 200ms\n"})
		_, got := fx.post("/v1/chat/completions", "openai", streamOpenAIBody)
		if !strings.HasSuffix(got, doneChunk) || strings.Contains(got, "provider_timeout") {
			t.Fatalf("body %q", got)
		}
	})
	t.Run("a client that stops reading releases the provider", func(t *testing.T) {
		canceled := make(chan struct{})
		_, server := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
			defer close(canceled)
			w.Header().Set("Content-Type", "text/event-stream")
			chunk := "data: " + strings.Repeat("x", 1<<20) + "\n\n"
			for r.Context().Err() == nil {
				if _, err := w.Write([]byte(chunk)); err != nil {
					return
				}
				w.(http.Flusher).Flush()
			}
		})
		fx := newFixture(t, server.URL, fixtureOptions{listeners: "listeners:\n  streaming_idle_timeout: 200ms\n"})
		target, _ := url.Parse(fx.server.URL)
		conn, err := net.Dial("tcp", target.Host)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = conn.Close() }()
		head := "POST /v1/chat/completions HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer " + fx.key +
			"\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(streamOpenAIBody)) + "\r\n\r\n" + streamOpenAIBody
		if _, err := conn.Write([]byte(head)); err != nil {
			t.Fatal(err)
		}
		select {
		case <-canceled:
		case <-time.After(20 * time.Second):
			t.Fatal("a stuck client held the upstream call")
		}
		if result := fx.sink.wait(t); !result.ClientDisconnected {
			t.Fatalf("result %+v", result)
		}
	})
}

func TestEventSizeLimit(t *testing.T) {
	parser := newSSEParser()
	parser.Feed([]byte("data: " + strings.Repeat("x", maxSSEEventBytes+1)))
	if _, _, err := parser.Next(); err == nil {
		t.Fatal("an oversized partial event was accepted")
	}
	within := newSSEParser()
	within.Feed([]byte("data: " + strings.Repeat("x", maxSSEEventBytes-16) + "\n\n"))
	if _, ok, err := within.Next(); err != nil || !ok {
		t.Fatalf("an event within the limit: %v %v", ok, err)
	}

	_, server := startUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(deltaChunk))
		chunk := strings.Repeat("x", 1<<20)
		_, _ = w.Write([]byte("data: "))
		for range maxSSEEventBytes/(1<<20) + 2 {
			if _, err := w.Write([]byte(chunk)); err != nil {
				return
			}
		}
	})
	fx := newFixture(t, server.URL, fixtureOptions{})
	_, got := fx.post("/v1/chat/completions", "openai", streamOpenAIBody)
	if !strings.Contains(got, "invalid_upstream_response") {
		t.Fatalf("body %.200q", got)
	}
}

func TestEventDepthLimit(t *testing.T) {
	deep := "data: {\"a\":" + strings.Repeat("[", maxJSONDepth+1) + strings.Repeat("]", maxJSONDepth+1) + "}\n\n"
	_, server := startUpstream(t, sseReply(deltaChunk+deep+doneChunk))
	fx := newFixture(t, server.URL, fixtureOptions{})
	_, got := fx.post("/v1/chat/completions", "openai", streamOpenAIBody)
	if !strings.Contains(got, "invalid_upstream_response") {
		t.Fatalf("body %.200q", got)
	}
}

func TestStreamEventHookSeesContentBeforeFlush(t *testing.T) {
	_, server := startUpstream(t, sseReply(deltaChunk+usageChunk+doneChunk))
	var seen []string
	fx := newFixture(t, server.URL, fixtureOptions{hooks: Hooks{
		StreamEvent: func(_ context.Context, _ *Envelope, raw []byte) *Rejection {
			seen = append(seen, string(raw))
			return nil
		},
	}})
	if _, got := fx.post("/v1/chat/completions", "openai", streamOpenAIBody); got != deltaChunk+doneChunk {
		t.Fatalf("body %q", got)
	}
	if strings.Join(seen, "") != deltaChunk+doneChunk {
		t.Fatalf("hook saw %q", seen)
	}
}

func TestClientDisconnectCancelsUpstream(t *testing.T) {
	canceled := make(chan time.Time, 1)
	_, server := startUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10}}\n\n" + deltaChunk))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
		canceled <- time.Now()
	})
	fx := newFixture(t, server.URL, fixtureOptions{})
	response, err := http.DefaultClient.Do(fx.request(http.MethodPost, "/v1/chat/completions", "openai", streamOpenAIBody))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bufio.NewReader(response.Body).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	closedAt := time.Now()
	_ = response.Body.Close()
	http.DefaultClient.CloseIdleConnections()
	select {
	case at := <-canceled:
		if at.Sub(closedAt) > time.Second {
			t.Fatalf("the provider call stayed open for %s", at.Sub(closedAt))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the provider call was not canceled")
	}
	result := fx.sink.wait(t)
	if !result.ClientDisconnected || result.ErrorClass != ClassClientCancel || result.UsageComplete {
		t.Fatalf("result %+v", result)
	}
	if result.Usage.Input.Value != 10 || result.Usage.Output.Known {
		t.Fatalf("observed usage must be kept without inventing the rest: %+v", result.Usage)
	}
}
