package integration

import (
	"context"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/keys"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

func expectPaused(t *testing.T, status int, body, app string) {
	t.Helper()
	if status != http.StatusServiceUnavailable || !strings.Contains(body, `"comporta_code":"app_paused"`) || !strings.Contains(body, "app "+app+" is paused by an operator") {
		t.Fatalf("expected a pause refusal for %s: %d %s", app, status, body)
	}
}

func TestPauseBlocksWithinOneSecondAndSurvivesRestart(t *testing.T) {
	s := startServed(t, fakeprovider.Options{})
	if status, body := s.call("bot", "", gptBody); status != http.StatusOK {
		t.Fatalf("before the pause: %d %s", status, body)
	}
	began := time.Now()
	if status, body := s.admin(http.MethodPost, "/admin/v1/pause", `{"app":"bot"}`); status != http.StatusOK || !strings.Contains(body, `"effective_paused":true`) {
		t.Fatalf("pause: %d %s", status, body)
	}
	status, body := s.call("bot", "", gptBody)
	expectPaused(t, status, body, "bot")
	if elapsed := time.Since(began); elapsed >= time.Second {
		t.Fatalf("the pause took %v to apply", elapsed)
	}
	if status, _ := s.call("other", "", gptBody); status != http.StatusOK {
		t.Fatalf("another app was paused too: %d", status)
	}

	s.restart()
	status, body = s.call("bot", "", gptBody)
	expectPaused(t, status, body, "bot")
	if status, body := s.admin(http.MethodGet, "/admin/v1/pause", ""); status != http.StatusOK || !strings.Contains(body, `"app":"bot","paused":true`) {
		t.Fatalf("state after restart: %d %s", status, body)
	}
	if status, _ := s.admin(http.MethodPost, "/admin/v1/resume", `{"app":"bot"}`); status != http.StatusOK {
		t.Fatalf("resume: %d", status)
	}
	if status, body := s.call("bot", "", gptBody); status != http.StatusOK {
		t.Fatalf("after resume: %d %s", status, body)
	}
}

func TestResumeRestoresTraffic(t *testing.T) {
	s := startServed(t, fakeprovider.Options{})
	s.admin(http.MethodPost, "/admin/v1/pause", `{"app":"bot"}`)
	status, body := s.call("bot", "", gptBody)
	expectPaused(t, status, body, "bot")
	if status, body := s.admin(http.MethodPost, "/admin/v1/resume", `{"app":"bot"}`); status != http.StatusOK || !strings.Contains(body, `"changed":true`) || !strings.Contains(body, `"effective_paused":false`) {
		t.Fatalf("resume: %d %s", status, body)
	}
	if status, body := s.call("bot", "", gptBody); status != http.StatusOK {
		t.Fatalf("after resume: %d %s", status, body)
	}
	if status, body := s.admin(http.MethodPost, "/admin/v1/resume", `{"app":"bot"}`); status != http.StatusOK || !strings.Contains(body, `"changed":false`) {
		t.Fatalf("a repeated resume: %d %s", status, body)
	}
}

func TestPauseAllBlocksEveryApp(t *testing.T) {
	s := startServed(t, fakeprovider.Options{})
	if status, body := s.admin(http.MethodPost, "/admin/v1/pause", `{"all":true}`); status != http.StatusOK || !strings.Contains(body, `"global_paused":true`) {
		t.Fatalf("pause all: %d %s", status, body)
	}
	for _, app := range []string{"bot", "other"} {
		status, body := s.call(app, "", gptBody)
		expectPaused(t, status, body, app)
	}

	// An app added by a reload while everything is paused starts paused.
	text, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	updated := strings.Replace(string(text), "apps:\n", "apps:\n  fresh:\n    allowed_models: [fake-gpt]\n", 1)
	if err := os.WriteFile(s.path, []byte(updated), 0o600); err != nil {
		t.Fatal(err)
	}
	created, err := keys.NewStore(s.dataDir).Create(context.Background(), "fresh")
	if err != nil {
		t.Fatal(err)
	}
	s.keys["fresh"] = created.Key.Reveal()
	deadline := time.Now().Add(15 * time.Second)
	for {
		status, body := s.call("fresh", "", gptBody)
		if status == http.StatusServiceUnavailable {
			expectPaused(t, status, body, "fresh")
			break
		}
		if status == http.StatusOK {
			t.Fatal("an app added by reload escaped pause --all")
		}
		if time.Now().After(deadline) {
			t.Fatalf("the new app never arrived: %d %s", status, body)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if status, _ := s.admin(http.MethodPost, "/admin/v1/resume", `{"all":true}`); status != http.StatusOK {
		t.Fatal("resume all")
	}
	for _, app := range []string{"bot", "other", "fresh"} {
		if status, body := s.call(app, "", gptBody); status != http.StatusOK {
			t.Fatalf("%s after resume --all: %d %s", app, status, body)
		}
	}
}

func TestPauseLetsInFlightStreamFinish(t *testing.T) {
	received := make(chan struct{}, 1)
	release := make(chan struct{})
	var once sync.Once
	s := startServed(t, fakeprovider.Options{
		Scenario: fakeprovider.ScenarioSlowStream,
		OnRequest: func(fakeprovider.Request) {
			select {
			case received <- struct{}{}:
			default:
			}
		},
		Sleep: func(ctx context.Context, _ time.Duration) error {
			held := false
			once.Do(func() { held = true })
			if !held {
				return nil
			}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	})
	type outcome struct {
		status int
		body   string
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		request, _ := http.NewRequest(http.MethodPost, s.proxyURL("/v1/chat/completions"), strings.NewReader(testkit.OpenAIStreamBody()))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+s.keys["bot"])
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			done <- outcome{err: err}
			return
		}
		defer func() { _ = response.Body.Close() }()
		data, err := io.ReadAll(response.Body)
		done <- outcome{status: response.StatusCode, body: string(data), err: err}
	}()
	select {
	case <-received:
	case <-time.After(10 * time.Second):
		t.Fatal("the stream never reached the provider")
	}
	if status, _ := s.admin(http.MethodPost, "/admin/v1/pause", `{"app":"bot"}`); status != http.StatusOK {
		t.Fatal("pause")
	}
	status, body := s.call("bot", "", gptBody)
	expectPaused(t, status, body, "bot")
	close(release)
	select {
	case result := <-done:
		if result.err != nil || result.status != http.StatusOK || !strings.Contains(result.body, "[DONE]") {
			t.Fatalf("the admitted stream was cut by the pause: %v %d %s", result.err, result.status, result.body)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the stream did not finish")
	}
}

func TestAdminEndpointsAbsentOnProxyPort(t *testing.T) {
	s := startServed(t, fakeprovider.Options{})
	paths := []string{"/admin/v1/pause", "/admin/v1/resume", "/admin/v1/session", "/admin/v1/users/forget", "/admin/v1/usage/query",
		"/admin/v1/pause/", "/ADMIN/v1/pause", "/admin/v1/../v1/pause", "//admin/v1/pause", "/admin/v1/pause?x=1"}
	for _, path := range paths {
		for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
			request, err := http.NewRequest(method, s.proxyURL(""), strings.NewReader(`{"all":true}`))
			if err != nil {
				t.Fatal(err)
			}
			request.URL.Opaque = path
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", "Bearer "+s.token)
			status, body := send(t, request)
			if (status != http.StatusNotFound && status != http.StatusBadRequest) || strings.Contains(body, "global_paused") || strings.Contains(body, "expires_at") {
				t.Fatalf("%s %s on the proxy port: %d %s", method, path, status, body)
			}
		}
	}
	if status, body := s.admin(http.MethodGet, "/admin/v1/pause", ""); status != http.StatusOK || strings.Contains(body, `"global_paused":true`) {
		t.Fatalf("nothing on the proxy port may change the admin state: %d %s", status, body)
	}
	request, _ := http.NewRequest(http.MethodGet, s.adminURL("/admin/v1/pause"), nil)
	if status, body := send(t, request); status != http.StatusUnauthorized || !strings.Contains(body, "invalid admin token") {
		t.Fatalf("admin listener without a token: %d %s", status, body)
	}
}
