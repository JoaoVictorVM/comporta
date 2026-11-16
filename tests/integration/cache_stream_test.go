package integration

import (
	"bufio"
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

// sseEvents splits a stream body into its events, in order.
func sseEvents(body string) []string {
	var out []string
	for _, event := range strings.Split(body, "\n\n") {
		if strings.TrimSpace(event) != "" {
			out = append(out, event)
		}
	}
	return out
}

func checkReplay(t *testing.T, first, second *http.Response, firstBody, secondBody, terminal string) {
	t.Helper()
	if second.StatusCode != 200 || second.Header.Get("X-Comporta-Cache") != "hit" || !strings.HasPrefix(second.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("replay status %d headers %v", second.StatusCode, second.Header)
	}
	a, b := sseEvents(firstBody), sseEvents(secondBody)
	if len(a) < 3 || len(a) != len(b) {
		t.Fatalf("replayed %d events of %d", len(b), len(a))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("event %d changed in the replay: %q != %q", i, b[i], a[i])
		}
	}
	if !strings.Contains(b[len(b)-1], terminal) {
		t.Fatalf("the replay ends with its terminal: %q", b[len(b)-1])
	}
}

func TestCacheReplayOpenAIStream(t *testing.T) {
	gw := startCacheGateway(t, cacheOn, fakeprovider.Options{}, gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	for _, body := range []string{zeroTemp(gptStream), zeroTemp(testkit.WithField(gptStream, `"stream_options":{"include_usage":true}`))} {
		first, firstBody := gw.call("/v1/chat/completions", key, body)
		second, secondBody := gw.call("/v1/chat/completions", key, body)
		checkReplay(t, first, second, firstBody, secondBody, "[DONE]")
		asked := strings.Contains(body, "include_usage")
		if strings.Contains(secondBody, `"usage":{`) != asked {
			t.Fatalf("usage chunk in the replay must follow what the client asked: %v", asked)
		}
	}
	if calls := gw.providerCalls(); calls != 2 {
		t.Fatalf("each stream reached the provider once: %d", calls)
	}
	if hits := gw.cachedRecords(t); len(hits) != 2 {
		t.Fatalf("hits %+v", hits)
	}
}

func TestCacheReplayAnthropicStream(t *testing.T) {
	gw := startCacheGateway(t, cacheOn, fakeprovider.Options{}, gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	body := zeroTemp(testkit.AnthropicStreamBody())
	first, firstBody := gw.call("/v1/messages", key, body)
	second, secondBody := gw.call("/v1/messages", key, body)
	checkReplay(t, first, second, firstBody, secondBody, "message_stop")
	if calls := gw.providerCalls(); calls != 1 {
		t.Fatalf("provider calls %d", calls)
	}
}

func TestIncompleteStreamNotCached(t *testing.T) {
	for _, scenario := range []fakeprovider.Scenario{fakeprovider.ScenarioDisconnectMidstream, fakeprovider.ScenarioStreamErrorAfterChunk} {
		t.Run(string(scenario), func(t *testing.T) {
			gw := startCacheGateway(t, cacheOn, fakeprovider.Options{Scenario: scenario}, gatewayOptions{})
			key := gw.createKey("bot").Key.Reveal()
			body := zeroTemp(gptStream)
			gw.call("/v1/chat/completions", key, body)
			response, _ := gw.call("/v1/chat/completions", key, body)
			if response.Header.Get("X-Comporta-Cache") != "" || gw.providerCalls() != 2 {
				t.Fatal("a stream without a confirmed terminal is never stored")
			}
			if s := gw.cache.Stats(); s.Entries != 0 || s.Builders != 0 || s.UsedBytes != 0 {
				t.Fatalf("stats %+v", s)
			}
		})
	}
}

func TestStreamErrorNotCached(t *testing.T) {
	gw := startCacheGateway(t, cacheOn, fakeprovider.Options{Scenario: fakeprovider.ScenarioStreamErrorBeforeChunk}, gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	body := zeroTemp(testkit.AnthropicStreamBody())
	gw.call("/v1/messages", key, body)
	gw.call("/v1/messages", key, body)
	if gw.providerCalls() != 2 || gw.cache.Stats().Entries != 0 {
		t.Fatal("an error is never cached")
	}
}

func TestCacheReplaySlowClientBounded(t *testing.T) {
	gw := startCacheGateway(t, cacheOn, fakeprovider.Options{}, gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	body := zeroTemp(gptStream)
	gw.call("/v1/chat/completions", key, body)
	// A client that leaves during the replay: the lease ends with the request.
	ctx, cancel := context.WithCancel(context.Background())
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, gw.proxyURL("/v1/chat/completions"), strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+key)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	if response.Header.Get("X-Comporta-Cache") != "hit" {
		t.Fatal("expected a hit")
	}
	cancel()
	_ = response.Body.Close()
	deadline := time.Now().Add(5 * time.Second)
	for gw.cache.Stats().Leases != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("lease kept after the client left: %+v", gw.cache.Stats())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if gw.providerCalls() != 1 {
		t.Fatal("a failed replay never falls back to the provider")
	}
	gw.waitRecords(t, 2)
	for _, r := range gw.cachedRecords(t) {
		if r.Cost != 0 {
			t.Fatalf("an interrupted hit still costs nothing: %+v", r)
		}
	}
}

func TestCaptureDoesNotDelayFirstEvent(t *testing.T) {
	const delay = 150 * time.Millisecond
	gw := startCacheGateway(t, cacheOn, fakeprovider.Options{Scenario: fakeprovider.ScenarioSlowStream, SlowDelay: delay}, gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	request, _ := http.NewRequest(http.MethodPost, gw.proxyURL("/v1/chat/completions"), strings.NewReader(zeroTemp(gptStream)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+key)
	start := time.Now()
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	reader := bufio.NewReader(response.Body)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	first := time.Since(start)
	for {
		if _, err := reader.ReadString('\n'); err != nil {
			break
		}
	}
	total := time.Since(start)
	if total < 3*delay || first > total/2 {
		t.Fatalf("the first event waited for the capture: first %v of %v", first, total)
	}
}
