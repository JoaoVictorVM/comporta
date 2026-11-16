package integration

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/events"
	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/proxy"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

type digestRecorder struct {
	mu      sync.Mutex
	digests [][32]byte
}

func (r *digestRecorder) prepare(_ context.Context, envelope *proxy.Envelope, _ proxy.RequestContent) *proxy.Rejection {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.digests = append(r.digests, envelope.RequestHash)
	return nil
}

func TestEffectiveRequestHashIsolatesUsers(t *testing.T) {
	recorder := &digestRecorder{}
	gw := startGatewayWithHooks(t, fakeprovider.Options{}, "", proxy.Hooks{Prepare: recorder.prepare})
	key := gw.createKey("bot").Key.Reveal()
	other := gw.createKey("other").Key.Reveal()
	send := func(appKey, user, body string) {
		t.Helper()
		if response, reply := gw.callAs("/v1/chat/completions", appKey, user, body); response.StatusCode != http.StatusOK {
			t.Fatalf("status %d: %s", response.StatusCode, reply)
		}
	}
	id := testkit.UserMarker()
	send(key, id, gptBody)
	send(key, id, gptBody)
	send(key, id+"-b", gptBody)
	send(key, "", gptBody)
	send(key, "", gptBody)
	send(other, "", gptBody)
	send(key, id, gptStream)

	got := recorder.digests
	if len(got) != 7 {
		t.Fatalf("%d requests observed", len(got))
	}
	if got[0] != got[1] {
		t.Fatal("the same user and body must keep the digest")
	}
	if got[0] == got[2] {
		t.Fatal("two users must not share a digest")
	}
	if got[3] != got[4] {
		t.Fatal("anonymous requests of an app must share a digest")
	}
	if got[3] == got[5] {
		t.Fatal("anonymous requests of two apps must not share a digest")
	}
	if got[0] == got[6] {
		t.Fatal("a streaming request differs from a normal one")
	}
}

func TestUsagePatchStaysOutOfTheDigest(t *testing.T) {
	recorder := &digestRecorder{}
	gw := startGatewayWithHooks(t, fakeprovider.Options{}, "", proxy.Hooks{Prepare: recorder.prepare})
	key := gw.createKey("bot").Key.Reveal()
	id := testkit.UserMarker()
	plain := gptStream
	asked := testkit.WithField(gptStream, `"stream_options":{"include_usage":false}`)
	for _, body := range []string{plain, asked} {
		if response, reply := gw.callAs("/v1/chat/completions", key, id, body); response.StatusCode != http.StatusOK {
			t.Fatalf("status %d: %s", response.StatusCode, reply)
		}
	}
	if recorder.digests[0] == recorder.digests[1] {
		t.Fatal("what the client asked for must stay in the digest")
	}
}

const cacheOn = "    cache:\n      enabled: true\n"

// startCacheGateway is a metered gateway with the response cache, as serve
// composes it, and settings appended to the bot app.
func startCacheGateway(t *testing.T, settings string, fake fakeprovider.Options, options gatewayOptions) *gateway {
	t.Helper()
	options.metered, options.cache = true, true
	edit := options.edit
	options.edit = func(text string) string {
		text = withBot(settings)(text)
		if edit != nil {
			text = edit(text)
		}
		return text
	}
	return startConfiguredGateway(t, fake, options)
}

func zeroTemp(body string) string { return testkit.WithField(body, `"temperature":0`) }

// providerCalls drains the requests the fake received.
func (g *gateway) providerCalls() int {
	n := 0
	for {
		select {
		case <-g.requests:
			n++
		case <-time.After(150 * time.Millisecond):
			return n
		}
	}
}

func (g *gateway) cachedRecords(t *testing.T) []storage.Record {
	t.Helper()
	var out []storage.Record
	for _, r := range g.records(t) {
		if r.Outcome == "cached" {
			out = append(out, r)
		}
	}
	return out
}

func (g *gateway) waitRecords(t *testing.T, want int) []storage.Record {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		records := g.records(t)
		if len(records) >= want || time.Now().After(deadline) {
			return records
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestResponseCacheExactHit(t *testing.T) {
	gw := startCacheGateway(t, cacheOn, fakeprovider.Options{}, gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	user := testkit.UserMarker()
	body := zeroTemp(gptBody)
	first, firstAnswer := gw.callAs("/v1/chat/completions", key, user, body)
	second, secondAnswer := gw.callAs("/v1/chat/completions", key, user, body)
	if first.StatusCode != 200 || second.StatusCode != 200 || firstAnswer != secondAnswer {
		t.Fatalf("statuses %d %d", first.StatusCode, second.StatusCode)
	}
	if calls := gw.providerCalls(); calls != 1 {
		t.Fatalf("an identical request reached the provider again: %d calls", calls)
	}
	if first.Header.Get("X-Comporta-Cache") != "" || second.Header.Get("X-Comporta-Cache") != "hit" {
		t.Fatal("only the hit carries x-comporta-cache: hit")
	}
	records := gw.waitRecords(t, 2)
	cached := gw.cachedRecords(t)
	if len(records) != 2 || len(cached) != 1 || cached[0].Cost != 0 || cached[0].UpstreamStatus != nil || cached[0].RequestCount != 1 {
		t.Fatalf("records %+v", records)
	}
	if hits := gw.policyEvents(t, events.Cached); len(hits) != 1 || hits[0].RequestID != cached[0].RequestID {
		t.Fatalf("cached events %+v", hits)
	}
}

func TestResponseCacheUserIsolation(t *testing.T) {
	gw := startCacheGateway(t, cacheOn, fakeprovider.Options{}, gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	body := zeroTemp(gptBody)
	gw.callAs("/v1/chat/completions", key, "user-a", body)
	response, _ := gw.callAs("/v1/chat/completions", key, "user-b", body)
	if gw.providerCalls() != 2 || response.Header.Get("X-Comporta-Cache") != "" {
		t.Fatal("another user got the first user's entry")
	}
}

func TestResponseCacheSharedUsers(t *testing.T) {
	gw := startCacheGateway(t, cacheOn+"      shared_across_users: true\n", fakeprovider.Options{}, gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	body := zeroTemp(gptBody)
	gw.callAs("/v1/chat/completions", key, "user-a", body)
	response, _ := gw.callAs("/v1/chat/completions", key, "user-b", body)
	if gw.providerCalls() != 1 || response.Header.Get("X-Comporta-Cache") != "hit" {
		t.Fatal("shared_across_users lets users of the same app share entries")
	}
	hits := gw.cachedRecords(t)
	hashB, _ := gw.identity.LookupHash("bot", "user-b")
	if len(hits) != 1 || hits[0].UserHash != hashB.Hex() {
		t.Fatalf("a shared hit is recorded for the user who asked: %+v", hits)
	}
}

func TestResponseCacheAppIsolation(t *testing.T) {
	gw := startCacheGateway(t, cacheOn+"      shared_across_users: true\n", fakeprovider.Options{}, gatewayOptions{
		edit: func(text string) string {
			return strings.Replace(text, "  other:\n    allowed_models: [fake-gpt]\n", "  other:\n    allowed_models: [fake-gpt]\n"+cacheOn+"      shared_across_users: true\n", 1)
		},
	})
	bot := gw.createKey("bot").Key.Reveal()
	other := gw.createKey("other").Key.Reveal()
	body := zeroTemp(gptBody)
	gw.call("/v1/chat/completions", bot, body)
	response, _ := gw.call("/v1/chat/completions", other, body)
	if gw.providerCalls() != 2 || response.Header.Get("X-Comporta-Cache") != "" {
		t.Fatal("two apps never share an entry, even when both share across users")
	}
}

func TestResponseCacheTemperatureEligibility(t *testing.T) {
	gw := startCacheGateway(t, cacheOn, fakeprovider.Options{}, gatewayOptions{})
	key := gw.createKey("bot").Key.Reveal()
	for _, body := range []string{
		gptBody,
		testkit.WithField(gptBody, `"temperature":0.2`),
		testkit.WithField(gptBody, `"temperature":2`),
		testkit.WithField(gptBody, `"temperature":null`),
	} {
		for range 2 {
			if response, _ := gw.call("/v1/chat/completions", key, body); response.Header.Get("X-Comporta-Cache") != "" {
				t.Fatalf("cached without temperature 0: %s", body)
			}
		}
	}
	if calls := gw.providerCalls(); calls != 8 {
		t.Fatalf("every request above 0 or without temperature reaches the provider: %d", calls)
	}
	if len(gw.cachedRecords(t)) != 0 {
		t.Fatal("nothing was served from cache")
	}
}
