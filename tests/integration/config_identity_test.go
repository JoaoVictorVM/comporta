package integration

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/proxy"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

func requireUserFor(app string) func(string) string {
	return func(text string) string {
		return strings.Replace(text, "  "+app+":\n", "  "+app+":\n    require_user: true\n", 1)
	}
}

// removeApp drops an app and its allowed models from the configuration text.
func removeApp(text, app string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	skipping := false
	for _, line := range lines {
		if line == "  "+app+":" {
			skipping = true
			continue
		}
		if skipping && strings.HasPrefix(line, "    ") {
			continue
		}
		skipping = false
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func TestConfigRequireUserControlsIdentity(t *testing.T) {
	gw := startGateway(t, fakeprovider.Options{}, "")
	key := gw.createKey("bot").Key.Reveal()
	other := gw.createKey("other").Key.Reveal()

	if response, body := gw.call("/v1/chat/completions", key, gptBody); response.StatusCode != http.StatusOK {
		t.Fatalf("not required yet: %d %s", response.StatusCode, body)
	}
	gw.lastRequest()
	gw.reload(requireUserFor("bot"))

	response, body := gw.call("/v1/chat/completions", key, gptBody)
	if response.StatusCode != http.StatusBadRequest || !strings.Contains(body, "user_required") ||
		!strings.Contains(body, "this app requires a user ID (X-Comporta-User header)") {
		t.Fatalf("required: %d %s", response.StatusCode, body)
	}
	if len(gw.requests) != 0 {
		t.Fatal("a rejected request reached the provider")
	}
	if response, body := gw.callAs("/v1/chat/completions", key, testkit.UserMarker(), gptBody); response.StatusCode != http.StatusOK {
		t.Fatalf("with a user: %d %s", response.StatusCode, body)
	}
	gw.lastRequest()
	// The flag belongs to one app.
	if response, body := gw.call("/v1/chat/completions", other, gptBody); response.StatusCode != http.StatusOK {
		t.Fatalf("another app: %d %s", response.StatusCode, body)
	}
	gw.lastRequest()

	request, err := http.NewRequest(http.MethodGet, gw.proxyURL("/v1/models"), nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer "+key)
	models, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = models.Body.Close()
	if models.StatusCode != http.StatusOK {
		t.Fatalf("the catalog must not ask for a user: %d", models.StatusCode)
	}
}

// The request under test is served with the snapshot captured before the reload, which
// is how the listener admits a request whose body is still arriving.
func TestIdentityPinsSnapshotAcrossReload(t *testing.T) {
	gw := startGateway(t, fakeprovider.Options{}, "")
	key := gw.createKey("bot").Key.Reveal()
	pinned := gw.config.Capture()
	gw.reload(requireUserFor("bot"))

	if response, body := gw.call("/v1/chat/completions", key, gptBody); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("a new request must see the new configuration: %d %s", response.StatusCode, body)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(gptBody))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+key)
	recorder := httptest.NewRecorder()
	gw.handler.ServeRoute(recorder, request, proxy.RouteChatCompletions, pinned)
	if recorder.Code != http.StatusOK {
		t.Fatalf("the request admitted before the reload changed its rule: %d %s", recorder.Code, recorder.Body)
	}
	if !gw.results.last(t).Anonymous {
		t.Fatal("the pinned rule allows an anonymous request")
	}
}
