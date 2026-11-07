package integration

import (
	"net/http"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

func TestProxyResultCarriesUserHashForRecord(t *testing.T) {
	gw := startGateway(t, fakeprovider.Options{}, "")
	key := gw.createKey("bot").Key.Reveal()
	id := testkit.UserMarker()
	hash, err := gw.identity.LookupHash("bot", id)
	if err != nil {
		t.Fatal(err)
	}
	if response, body := gw.callAs("/v1/chat/completions", key, id, gptBody); response.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", response.StatusCode, body)
	}
	result := gw.results.last(t)
	resultCarriesIdentity(t, result, hash.Hex(), false)
	if !result.Usage.Input.Known || result.Usage.Input.Value != fakeprovider.FixtureInputTokens {
		t.Fatalf("usage %+v", result.Usage)
	}
}

func TestProxyResultCarriesIdentityOnFailure(t *testing.T) {
	gw := startGateway(t, fakeprovider.Options{Scenario: fakeprovider.ScenarioRateLimit}, "")
	key := gw.createKey("bot").Key.Reveal()
	if response, body := gw.call("/v1/chat/completions", key, gptBody); response.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status %d: %s", response.StatusCode, body)
	}
	resultCarriesIdentity(t, gw.results.last(t), gw.identity.AnonymousHash("bot").Hex(), true)
}
