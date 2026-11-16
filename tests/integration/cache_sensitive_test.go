package integration

import (
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
)

func sensitiveCacheGateway(t *testing.T, detection ...string) *gateway {
	t.Helper()
	return startCacheGateway(t, cacheOn, fakeprovider.Options{}, gatewayOptions{sensitive: true, edit: withDetection(detection...)})
}

func emailBody() string {
	return zeroTemp(messagesBody("escreva para cliente@example.test"))
}

func TestRedactedRequestBypassesCache(t *testing.T) {
	gw := sensitiveCacheGateway(t, "email: redact")
	key := gw.createKey("bot").Key.Reveal()
	for range 2 {
		if response, text := gw.call("/v1/messages", key, emailBody()); response.StatusCode != 200 || response.Header.Get("X-Comporta-Cache") != "" {
			t.Fatalf("%d %s", response.StatusCode, text)
		}
	}
	if gw.providerCalls() != 2 {
		t.Fatal("a redacted request never reads nor feeds the cache")
	}
	if s := gw.cache.Stats(); s.Entries != 0 || s.UsedBytes != 0 {
		t.Fatalf("stats %+v", s)
	}
}

func TestRedactWithoutMatchCanCache(t *testing.T) {
	gw := sensitiveCacheGateway(t, "email: redact")
	key := gw.createKey("bot").Key.Reveal()
	body := zeroTemp(messagesBody("nada sensivel aqui"))
	gw.call("/v1/messages", key, body)
	if response, _ := gw.call("/v1/messages", key, body); response.Header.Get("X-Comporta-Cache") != "hit" {
		t.Fatal("a redact rule that changed nothing does not stop the cache")
	}
}

func TestDetectWithoutRedactionCanCache(t *testing.T) {
	gw := sensitiveCacheGateway(t, "email: detect")
	key := gw.createKey("bot").Key.Reveal()
	gw.call("/v1/messages", key, emailBody())
	if response, _ := gw.call("/v1/messages", key, emailBody()); response.Header.Get("X-Comporta-Cache") != "hit" {
		t.Fatal("detect alone keeps the content unchanged and may be cached")
	}
	if gw.providerCalls() != 1 {
		t.Fatal("provider calls")
	}
}

func TestRehydratedOutputNeverCached(t *testing.T) {
	gw := startCacheGateway(t, cacheOn, fakeprovider.Options{}, gatewayOptions{sensitive: true, upstream: echoPlaceholders, edit: withDetection("email: redact", "rehydrate: true")})
	key := gw.createKey("bot").Key.Reveal()
	for range 2 {
		if response, text := gw.call("/v1/messages", key, emailBody()); response.StatusCode != 200 || response.Header.Get("X-Comporta-Cache") != "" {
			t.Fatalf("%d %s", response.StatusCode, text)
		}
	}
	if s := gw.cache.Stats(); s.Entries != 0 || s.UsedBytes != 0 || s.Builders != 0 {
		t.Fatalf("a restored answer is never kept: %+v", s)
	}
}
