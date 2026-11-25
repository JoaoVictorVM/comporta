package proxy

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/responsecache"
	"github.com/JoaoVictorVM/comporta/internal/sensitivedata"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

// cacheAccounting admits everything and confirms each conclusion, or fails it
// when fail is set.
type cacheAccounting struct {
	mu      sync.Mutex
	results []AttemptResult
	fail    error
}

func (a *cacheAccounting) Admit(context.Context, *config.Snapshot, *Envelope, string, config.Provider) (Admission, *Rejection) {
	return Admission{Conclude: func(result AttemptResult) error {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.results = append(a.results, result)
		return a.fail
	}}, nil
}

func (a *cacheAccounting) failWith(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.fail = err
}

func (a *cacheAccounting) all() []AttemptResult {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]AttemptResult(nil), a.results...)
}

const cacheSettings = "    cache:\n      enabled: true\n"

func cacheFixture(t *testing.T, settings string, reply http.HandlerFunc) (*fixture, *recordingUpstream, *cacheAccounting, *responsecache.Cache) {
	t.Helper()
	upstream, server := startUpstream(t, reply)
	accounting := &cacheAccounting{}
	cache := responsecache.New(responsecache.Options{})
	fx := newFixture(t, server.URL, fixtureOptions{listeners: settings, accounting: accounting, cache: cache})
	return fx, upstream, accounting, cache
}

func jsonReply(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", "upstream-request")
		_, _ = w.Write([]byte(body))
	}
}

const cachedOpenAI = `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"fake-gpt","choices":[{"index":0,"message":{"role":"assistant","content":"oi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":12,"completion_tokens":1,"total_tokens":13}}`

var (
	cachePrompt    = testkit.Prompt("oi")
	zeroTempOpenAI = `{"model":"fake-gpt","messages":[{"role":"user","content":"` + cachePrompt + `"}],"temperature":0,"max_tokens":16}`
)

func TestCacheJSONHitAndMiss(t *testing.T) {
	fx, upstream, accounting, _ := cacheFixture(t, cacheSettings, jsonReply(cachedOpenAI))
	first, firstBody := fx.post("/v1/chat/completions", "openai", zeroTempOpenAI)
	second, secondBody := fx.post("/v1/chat/completions", "openai", zeroTempOpenAI)
	if upstream.count() != 1 {
		t.Fatalf("an identical request must be served from cache, upstream calls = %d", upstream.count())
	}
	if first.Header.Get(cacheHeader) != "" || second.Header.Get(cacheHeader) != cacheHit {
		t.Fatalf("only the hit carries the header: %q %q", first.Header.Get(cacheHeader), second.Header.Get(cacheHeader))
	}
	if secondBody != firstBody || second.StatusCode != 200 || second.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("hit must replay the stored payload: %d %q", second.StatusCode, secondBody)
	}
	if second.Header.Get("X-Request-Id") != "" {
		t.Fatal("the provider's request id is never replayed")
	}
	// The hit is concluded when its operation ends, which can be after the
	// client already read the answer.
	results := accounting.all()
	for deadline := time.Now().Add(5 * time.Second); len(results) < 2 && time.Now().Before(deadline); results = accounting.all() {
		time.Sleep(10 * time.Millisecond)
	}
	if len(results) != 2 || results[0].CacheHit || !results[1].CacheHit || !results[1].NotSent || results[1].UpstreamStatus.Known || results[1].DownstreamStatus != known(200) {
		t.Fatalf("results = %+v", results)
	}
}

func TestCacheDisabledByDefault(t *testing.T) {
	fx, upstream, _, _ := cacheFixture(t, "", jsonReply(cachedOpenAI))
	fx.post("/v1/chat/completions", "openai", zeroTempOpenAI)
	response, _ := fx.post("/v1/chat/completions", "openai", zeroTempOpenAI)
	if upstream.count() != 2 || response.Header.Get(cacheHeader) != "" {
		t.Fatal("an app without cache.enabled never uses the cache")
	}
}

// settled waits for the captures still open to end: the client can read the
// answer before the operation that produced it discards or publishes its
// capture.
func settled(t *testing.T, cache *responsecache.Cache) responsecache.Stats {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s := cache.Stats()
		if s.Builders == 0 || time.Now().After(deadline) {
			return s
		}
		time.Sleep(time.Millisecond)
	}
}

func TestCacheNeedsConfirmedConclusion(t *testing.T) {
	fx, upstream, accounting, cache := cacheFixture(t, cacheSettings, jsonReply(cachedOpenAI))
	accounting.failWith(errors.New("storage down"))
	fx.post("/v1/chat/completions", "openai", zeroTempOpenAI)
	accounting.failWith(nil)
	fx.post("/v1/chat/completions", "openai", zeroTempOpenAI)
	if upstream.count() != 2 {
		t.Fatal("a response whose result was not made durable is never stored")
	}
	if s := settled(t, cache); s.Entries != 1 || s.Builders != 0 {
		t.Fatalf("stats = %+v", s)
	}
}

func TestCacheSkipsUsageMissingAndErrors(t *testing.T) {
	for name, reply := range map[string]http.HandlerFunc{
		"usage missing": jsonReply(`{"id":"x","choices":[]}`),
		"upstream 500": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(500)
		},
	} {
		t.Run(name, func(t *testing.T) {
			fx, upstream, _, cache := cacheFixture(t, cacheSettings, reply)
			fx.post("/v1/chat/completions", "openai", zeroTempOpenAI)
			fx.post("/v1/chat/completions", "openai", zeroTempOpenAI)
			if s := settled(t, cache); upstream.count() != 2 || s.Entries != 0 || s.UsedBytes != 0 {
				t.Fatalf("nothing incomplete is stored: %+v", s)
			}
		})
	}
}

func TestZeroTemperatureWithoutUnderflow(t *testing.T) {
	for lexeme, zero := range map[string]bool{
		"0": true, "-0": true, "0.0": true, "0.000": true, "0e10": true, "0.0E-5": true, "-0.0e+3": true,
		"1e-400": false, "5e-324": false, "0.0001": false, "1": false, "-1": false, "0.5": false, "10e-1": false,
	} {
		if zeroNumber(lexeme) != zero {
			t.Errorf("zeroNumber(%q) = %v", lexeme, !zero)
		}
	}
	fx, upstream, _, _ := cacheFixture(t, cacheSettings, jsonReply(cachedOpenAI))
	tiny := strings.Replace(zeroTempOpenAI, `"temperature":0`, `"temperature":1e-400`, 1)
	fx.post("/v1/chat/completions", "openai", tiny)
	fx.post("/v1/chat/completions", "openai", tiny)
	if upstream.count() != 2 {
		t.Fatal("a value that underflows to zero is not zero")
	}
	written := strings.Replace(zeroTempOpenAI, `"temperature":0`, `"temperature":0.0`, 1)
	fx.post("/v1/chat/completions", "openai", written)
	response, _ := fx.post("/v1/chat/completions", "openai", written)
	if upstream.count() != 3 || response.Header.Get(cacheHeader) != cacheHit {
		t.Fatal("0.0 is zero and may be cached under its own bytes")
	}
}

func TestTemperatureEligibility(t *testing.T) {
	for _, temperature := range []string{`"temperature":0.7,`, `"temperature":-1,`, ``, `"temperature":null,`} {
		fx, upstream, _, _ := cacheFixture(t, cacheSettings, jsonReply(cachedOpenAI))
		body := `{"model":"fake-gpt",` + temperature + `"messages":[{"role":"user","content":"` + cachePrompt + `"}]}`
		fx.post("/v1/chat/completions", "openai", body)
		response, _ := fx.post("/v1/chat/completions", "openai", body)
		if upstream.count() != 2 || response.Header.Get(cacheHeader) != "" {
			t.Fatalf("%q must bypass the cache", temperature)
		}
	}
}

func TestUnknownFieldsAndHeadersPartitionCache(t *testing.T) {
	fx, upstream, _, _ := cacheFixture(t, cacheSettings, jsonReply(cachedOpenAI))
	variants := []string{
		zeroTempOpenAI,
		strings.Replace(zeroTempOpenAI, `}`, `,"x_unknown":1}`, 1),
		strings.Replace(zeroTempOpenAI, `"max_tokens":16`, `"max_tokens": 16`, 1),
		strings.Replace(zeroTempOpenAI, cachePrompt, testkit.Prompt("ola"), 1),
	}
	for _, body := range variants {
		fx.post("/v1/chat/completions", "openai", body)
	}
	if upstream.count() != len(variants) {
		t.Fatalf("unknown fields, whitespace and content partition the cache: %d calls", upstream.count())
	}

	anthropicUpstream, server := startUpstream(t, jsonReply(`{"id":"msg","type":"message","content":[],"usage":{"input_tokens":3,"output_tokens":1}}`))
	afx := newFixture(t, server.URL, fixtureOptions{listeners: cacheSettings, accounting: &cacheAccounting{}, cache: responsecache.New(responsecache.Options{})})
	body := `{"model":"fake-claude","messages":[{"role":"user","content":"` + cachePrompt + `"}],"temperature":0,"max_tokens":16}`
	for _, version := range []string{"2023-06-01", "2023-01-01", "2023-06-01"} {
		request := afx.request(http.MethodPost, "/v1/messages", "anthropic", body)
		request.Header.Set("Anthropic-Version", version)
		afx.do(request)
	}
	if anthropicUpstream.count() != 2 {
		t.Fatalf("a semantic header partitions the cache: %d calls", anthropicUpstream.count())
	}
}

func TestSharedHashNeutralizesOnlyIdentity(t *testing.T) {
	var mu sync.Mutex
	var hashes [][32]byte
	upstream, server := startUpstream(t, jsonReply(cachedOpenAI))
	fx := newFixture(t, server.URL, fixtureOptions{
		listeners: cacheSettings + "      shared_across_users: true\n", accounting: &cacheAccounting{}, cache: responsecache.New(responsecache.Options{}),
		hooks: Hooks{BeforeSend: func(_ context.Context, e *Envelope, _ RequestContent) *Rejection {
			mu.Lock()
			hashes = append(hashes, e.RequestHash)
			mu.Unlock()
			return nil
		}},
	})
	withUser := func(user string) string {
		return strings.Replace(zeroTempOpenAI, `{"model"`, `{"user":"`+user+`","model"`, 1)
	}
	fx.post("/v1/chat/completions", "openai", withUser("alice"))
	sent := upstream.body
	response, _ := fx.post("/v1/chat/completions", "openai", withUser("bob"))
	if upstream.count() != 1 || response.Header.Get(cacheHeader) != cacheHit {
		t.Fatal("shared_across_users lets another user of the same app hit")
	}
	if strings.Contains(string(sent), sharedIdentityMarker) || strings.Contains(string(sent), "alice") {
		t.Fatal("the provider gets the pseudonym, never the raw user or the digest marker")
	}
	if sha256.Sum256(sent) != hashes[0] || hashes[0] == hashes[1] {
		t.Fatal("RequestHash keeps hashing the effective body, pseudonym included")
	}
	fx.post("/v1/chat/completions", "openai", strings.Replace(withUser("carol"), cachePrompt, testkit.Prompt("alice"), 1))
	if upstream.count() != 2 {
		t.Fatal("anything other than the identity fields still partitions the cache")
	}
}

func TestCacheUserIsolationByDefault(t *testing.T) {
	fx, upstream, _, _ := cacheFixture(t, cacheSettings, jsonReply(cachedOpenAI))
	withUser := func(user string) string {
		return strings.Replace(zeroTempOpenAI, `{"model"`, `{"user":"`+user+`","model"`, 1)
	}
	fx.post("/v1/chat/completions", "openai", withUser("alice"))
	response, _ := fx.post("/v1/chat/completions", "openai", withUser("bob"))
	if upstream.count() != 2 || response.Header.Get(cacheHeader) != "" {
		t.Fatal("another user never gets the first user's entry by default")
	}
	fx.post("/v1/chat/completions", "openai", zeroTempOpenAI)
	response, _ = fx.post("/v1/chat/completions", "openai", zeroTempOpenAI)
	if upstream.count() != 3 || response.Header.Get(cacheHeader) != cacheHit {
		t.Fatal("anonymous requests share the anonymous namespace of the app")
	}
}

func TestStreamingUsageOptionsPartitionCache(t *testing.T) {
	stream := deltaChunk + usageChunk + doneChunk
	fx, upstream, _, _ := cacheFixture(t, cacheSettings, sseReply(stream))
	plain := `{"model":"fake-gpt","messages":[{"role":"user","content":"` + cachePrompt + `"}],"temperature":0,"stream":true}`
	asked := strings.Replace(plain, `"stream":true`, `"stream":true,"stream_options":{"include_usage":true}`, 1)
	_, firstPlain := fx.post("/v1/chat/completions", "openai", plain)
	hit, replayed := fx.post("/v1/chat/completions", "openai", plain)
	if upstream.count() != 1 || hit.Header.Get(cacheHeader) != cacheHit || hit.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("a complete stream must be replayed: %d calls", upstream.count())
	}
	if replayed != firstPlain || strings.Contains(replayed, "prompt_tokens") || !strings.HasSuffix(replayed, doneChunk) {
		t.Fatalf("the replay holds what the client got, without the usage it did not ask for: %q", replayed)
	}
	_, withUsage := fx.post("/v1/chat/completions", "openai", asked)
	if upstream.count() != 2 || !strings.Contains(withUsage, "prompt_tokens") {
		t.Fatal("include_usage partitions the cache")
	}
	_, replayedUsage := fx.post("/v1/chat/completions", "openai", asked)
	if upstream.count() != 2 || replayedUsage != withUsage {
		t.Fatal("the usage chunk the client asked for is replayed")
	}
}

func TestFailedDetectScanBypassesCache(t *testing.T) {
	tight := sensitivedata.DefaultLimits()
	tight.MaxCandidates = 1
	upstream, server := startUpstream(t, formatReply)
	cache := responsecache.New(responsecache.Options{})
	fx := newFixture(t, server.URL, fixtureOptions{
		listeners: cacheSettings + detectionSettings("email: detect"), accounting: &cacheAccounting{}, cache: cache,
		sensitive: &SensitiveStage{Scanner: sensitivedata.NewScanner(sensitivedata.WithLimits(tight)), Recorder: &sensitiveRecorder{}},
	})
	body := `{"model":"fake-claude","max_tokens":10,"temperature":0,"messages":[{"role":"user","content":"` + testkit.Prompt("a@example.org b@example.org") + `"}]}`
	for range 2 {
		if response, answer := fx.post("/v1/messages", "anthropic", body); response.StatusCode != 200 || response.Header.Get(cacheHeader) != "" {
			t.Fatalf("%d %s", response.StatusCode, answer)
		}
	}
	if upstream.count() != 2 || cache.Stats().Entries != 0 {
		t.Fatal("a scan that could not finish never reads nor feeds the cache")
	}
}

func TestTranslatedHeaderOnReplay(t *testing.T) {
	fx, upstream, _, cache := cacheFixture(t, cacheSettings, jsonReply(cachedOpenAI))
	body := `{"model":"fake-gpt","input":"` + cachePrompt + `","temperature":0,"max_output_tokens":16}`
	first, firstBody := fx.post("/v1/responses", "openai", body)
	settled(t, cache)
	second, secondBody := fx.post("/v1/responses", "openai", body)
	if upstream.count() != 1 || second.Header.Get(cacheHeader) != cacheHit || secondBody != firstBody {
		t.Fatalf("translated answer not replayed: %d calls, %q", upstream.count(), second.Header.Get(cacheHeader))
	}
	if first.Header.Get(translatedHeader) != "openai-chat->openai-responses" || second.Header.Get(translatedHeader) != first.Header.Get(translatedHeader) {
		t.Fatalf("translated header %q then %q", first.Header.Get(translatedHeader), second.Header.Get(translatedHeader))
	}
	if !strings.Contains(secondBody, `"object":"response"`) {
		t.Fatalf("the replay must be in the client's format: %s", secondBody)
	}
}

func TestTranslationFormatsDoNotShareCacheKey(t *testing.T) {
	fx, upstream, _, cache := cacheFixture(t, cacheSettings, jsonReply(cachedOpenAI))
	fx.post("/v1/chat/completions", "openai", zeroTempOpenAI)
	settled(t, cache)
	response, body := fx.post("/v1/responses", "openai", `{"model":"fake-gpt","input":[{"role":"user","content":"`+cachePrompt+`"}],"temperature":0,"max_output_tokens":16}`)
	if upstream.count() != 2 || response.Header.Get(cacheHeader) != "" || strings.Contains(body, "chat.completion") {
		t.Fatalf("a Chat answer served a Responses request: %d calls, %s", upstream.count(), body)
	}
}
