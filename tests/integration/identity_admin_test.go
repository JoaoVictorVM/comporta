package integration

import (
	"errors"
	"net/http"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
	"github.com/JoaoVictorVM/comporta/internal/useridentity"
)

func TestLookupUsesSameAppScopedHash(t *testing.T) {
	gw := startGateway(t, fakeprovider.Options{}, "")
	key := gw.createKey("bot").Key.Reveal()
	id := testkit.UserMarker()
	if response, body := gw.callAs("/v1/chat/completions", key, id, gptBody); response.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", response.StatusCode, body)
	}
	stored := gw.results.last(t).UserHash
	found, err := gw.identity.LookupHash("bot", id)
	if err != nil {
		t.Fatal(err)
	}
	if found != stored {
		t.Fatal("a search would not find the record")
	}
	elsewhere, err := gw.identity.LookupHash("other", id)
	if err != nil {
		t.Fatal(err)
	}
	if elsewhere == stored {
		t.Fatal("a search in another app would find this record")
	}
	if _, err := gw.identity.LookupHash("bot", testkit.EmailID()); !errors.Is(err, useridentity.ErrUserEmail) {
		t.Fatalf("a search must validate like a request: %v", err)
	}
}

func TestLookupStillWorksForRemovedApps(t *testing.T) {
	gw := startGateway(t, fakeprovider.Options{}, "")
	key := gw.createKey("other").Key.Reveal()
	id := testkit.UserMarker()
	if response, body := gw.callAs("/v1/chat/completions", key, id, gptBody); response.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", response.StatusCode, body)
	}
	stored := gw.results.last(t).UserHash
	gw.reload(func(text string) string {
		return removeApp(text, "other")
	})
	if _, ok := gw.config.Capture().App("other"); ok {
		t.Fatal("the control failed: the app is still configured")
	}
	found, err := gw.identity.LookupHash("other", id)
	if err != nil {
		t.Fatal(err)
	}
	if found != stored {
		t.Fatal("the history of a removed app must stay reachable")
	}
}

func TestAnonymousSelectionIsExplicit(t *testing.T) {
	gw := startGateway(t, fakeprovider.Options{}, "")
	key := gw.createKey("bot").Key.Reveal()
	if response, body := gw.call("/v1/chat/completions", key, gptBody); response.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", response.StatusCode, body)
	}
	stored := gw.results.last(t).UserHash
	if stored != gw.identity.AnonymousHash("bot") {
		t.Fatal("the anonymous hash must find anonymous records")
	}
	typed, err := gw.identity.LookupHash("bot", "anonymous")
	if err != nil {
		t.Fatal(err)
	}
	if typed == stored {
		t.Fatal("typing anonymous must look for a user with that name")
	}
}
