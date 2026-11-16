package proxy

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/responsecache"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

// benchmarkCache measures the gateway alone: the provider answers at once, so
// the numbers compare cache work with no cache, not with a real provider.
func benchmarkCache(b *testing.B, settings string, body string, reply http.HandlerFunc, distinct bool) {
	server := httptest.NewServer(reply)
	b.Cleanup(server.Close)
	fx := newFixture(b, server.URL, fixtureOptions{listeners: settings, accounting: &cacheAccounting{}, cache: responsecache.New(responsecache.Options{})})
	b.ResetTimer()
	i := 0
	for b.Loop() {
		text := body
		if distinct {
			i++
			text = strings.Replace(body, cachePrompt, testkit.Prompt(fmt.Sprintf("oi %d", i)), 1)
		}
		request, _ := http.NewRequest(http.MethodPost, fx.server.URL+"/v1/chat/completions", strings.NewReader(text))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+fx.key)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
	}
}

func BenchmarkCacheMissJSON(b *testing.B) {
	b.Run("disabled", func(b *testing.B) { benchmarkCache(b, "", zeroTempOpenAI, jsonReply(cachedOpenAI), true) })
	b.Run("enabled", func(b *testing.B) { benchmarkCache(b, cacheSettings, zeroTempOpenAI, jsonReply(cachedOpenAI), true) })
}

func BenchmarkCacheReplaySSE(b *testing.B) {
	body := `{"model":"fake-gpt","messages":[{"role":"user","content":"` + cachePrompt + `"}],"temperature":0,"stream":true}`
	stream := sseReply(strings.Repeat(deltaChunk, 50) + usageChunk + doneChunk)
	b.Run("provider", func(b *testing.B) { benchmarkCache(b, "", body, stream, false) })
	b.Run("replay", func(b *testing.B) { benchmarkCache(b, cacheSettings, body, stream, false) })
}
