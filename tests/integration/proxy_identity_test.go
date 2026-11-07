package integration

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/proxy"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

func (g *gateway) callAs(path, key, user, body string) (*http.Response, string) {
	g.t.Helper()
	g.userHeader = user
	defer func() { g.userHeader = "" }()
	return g.call(path, key, body)
}

func sentField(t *testing.T, body []byte, path ...string) any {
	t.Helper()
	var current any
	if err := json.Unmarshal(body, &current); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	for _, key := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = object[key]
	}
	return current
}

func TestProxyEnvelopeDrivesUserIdentity(t *testing.T) {
	gw := startGateway(t, fakeprovider.Options{}, "")
	bot := gw.createKey("bot").Key.Reveal()
	other := gw.createKey("other").Key.Reveal()
	id := testkit.UserMarker()

	if response, body := gw.callAs("/v1/chat/completions", bot, id, gptBody); response.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", response.StatusCode, body)
	}
	botHash, err := gw.identity.LookupHash("bot", id)
	if err != nil {
		t.Fatal(err)
	}
	if got := sentField(t, gw.lastRequest().Body, "user"); got != botHash.Hex() {
		t.Fatalf("user %v", got)
	}
	result := gw.results.last(t)
	if result.AppName != "bot" || result.UserHash != botHash || result.Anonymous {
		t.Fatalf("app %s anonymous %v", result.AppName, result.Anonymous)
	}

	// Identity follows the authenticated app, whatever the body claims.
	spoof := testkit.WithField(gptBody, `"app":"bot"`)
	if response, body := gw.callAs("/v1/chat/completions", other, id, spoof); response.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", response.StatusCode, body)
	}
	otherHash, err := gw.identity.LookupHash("other", id)
	if err != nil {
		t.Fatal(err)
	}
	if otherHash == botHash {
		t.Fatal("two apps correlate the same user")
	}
	if got := sentField(t, gw.lastRequest().Body, "user"); got != otherHash.Hex() {
		t.Fatalf("user %v", got)
	}

	if response, body := gw.call("/v1/messages", bot, testkit.WithField(testkit.AnthropicBody(), `"metadata":{"user_id":"`+id+`"}`)); response.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", response.StatusCode, body)
	}
	if got := sentField(t, gw.lastRequest().Body, "metadata", "user_id"); got != botHash.Hex() {
		t.Fatalf("metadata.user_id %v", got)
	}

	if response, body := gw.callAs("/v1/chat/completions", bot, testkit.EmailID(), gptBody); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d: %s", response.StatusCode, body)
	}
	if len(gw.requests) != 0 {
		t.Fatal("a rejected identity reached the provider")
	}
}

func TestAnonymousUsersShareAHashWithinAnApp(t *testing.T) {
	gw := startGateway(t, fakeprovider.Options{}, "")
	bot := gw.createKey("bot").Key.Reveal()
	other := gw.createKey("other").Key.Reveal()
	var hashes []any
	for range 2 {
		if response, body := gw.call("/v1/chat/completions", bot, gptBody); response.StatusCode != http.StatusOK {
			t.Fatalf("status %d: %s", response.StatusCode, body)
		}
		hashes = append(hashes, sentField(t, gw.lastRequest().Body, "user"))
		if result := gw.results.last(t); !result.Anonymous {
			t.Fatal("the anonymous flag is missing")
		}
	}
	if hashes[0] != hashes[1] || hashes[0] != gw.identity.AnonymousHash("bot").Hex() {
		t.Fatalf("hashes %v", hashes)
	}
	if response, body := gw.call("/v1/chat/completions", other, gptBody); response.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", response.StatusCode, body)
	}
	if got := sentField(t, gw.lastRequest().Body, "user"); got == hashes[0] {
		t.Fatal("anonymous users of two apps correlate")
	}
}

// resultCarriesIdentity covers what a usage record needs from this stage. The price and
// the storage of the record belong to the metering feature.
func resultCarriesIdentity(t *testing.T, result proxy.AttemptResult, wantHash string, anonymous bool) {
	t.Helper()
	if result.UserHash.Hex() != wantHash || result.Anonymous != anonymous {
		t.Fatalf("anonymous %v, want %v", result.Anonymous, anonymous)
	}
}
