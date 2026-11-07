package integration

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/proxy"
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
