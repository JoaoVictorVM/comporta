package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/proxy"
)

func TestProxyUsesAuthenticatedAppForEveryRequest(t *testing.T) {
	gw := startGateway(t, fakeprovider.Options{}, "")
	bot := gw.createKey("bot")
	other := gw.createKey("other")

	if response, _ := gw.call("/v1/chat/completions", bot.Key.Reveal(), `{"model":"fake-gpt","messages":[],"app":"other","app_name":"other"}`); response.StatusCode != http.StatusOK {
		t.Fatalf("status %d", response.StatusCode)
	}
	if result := gw.results.last(t); result.AppName != "bot" || result.KeyID != bot.Metadata.ID {
		t.Fatalf("result %+v", result)
	}
	if response, _ := gw.call("/v1/chat/completions", other.Key.Reveal(), gptBody); response.StatusCode != http.StatusOK {
		t.Fatalf("status %d", response.StatusCode)
	}
	if result := gw.results.last(t); result.AppName != "other" || result.KeyID != other.Metadata.ID {
		t.Fatalf("result %+v", result)
	}

	request, _ := http.NewRequest(http.MethodGet, gw.proxyURL("/v1/models"), nil)
	request.Header.Set("Authorization", "Bearer "+other.Key.Reveal())
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var listing struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&listing); err != nil {
		t.Fatal(err)
	}
	if len(listing.Data) != 1 || listing.Data[0].ID != "fake-gpt" {
		t.Fatalf("catalog of the wrong app: %+v", listing)
	}
}

func TestProxyStripsComportaCredentials(t *testing.T) {
	gw := startGateway(t, fakeprovider.Options{}, "")
	key := gw.createKey("bot").Key.Reveal()
	const body = `{"model":"fake-claude","max_tokens":4,"messages":[],"unknown":{"keep":1.50}}`
	if response, reply := gw.call("/v1/messages", key, body); response.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", response.StatusCode, reply)
	}
	request := gw.lastRequest()
	flat := fmt.Sprint(request.Header)
	if strings.Contains(flat, key) || strings.Contains(flat, "cmp_") {
		t.Fatalf("the app key reached the provider: %s", flat)
	}
	if request.Header.Get("X-Api-Key") != providerSecret || request.Header.Get("Anthropic-Version") != "2023-06-01" {
		t.Fatalf("headers %s", flat)
	}
	anonymous := gw.identity.AnonymousHash("bot").Hex()
	if want := strings.TrimSuffix(body, "}") + `,"metadata":{"user_id":"` + anonymous + `"}}`; string(request.Body) != want {
		t.Fatalf("body %s", request.Body)
	}
}

func TestInFlightRequestKeepsIdentityAfterRevocation(t *testing.T) {
	release := make(chan struct{})
	reached := make(chan struct{}, 8)
	gw := startGateway(t, fakeprovider.Options{
		Scenario: fakeprovider.ScenarioSlowStream,
		Sleep: func(ctx context.Context, _ time.Duration) error {
			reached <- struct{}{}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}, "")
	created := gw.createKey("bot")
	key := created.Key.Reveal()

	finished := make(chan string, 1)
	go func() {
		_, body := gw.call("/v1/chat/completions", key, gptStream)
		finished <- body
	}()
	<-reached

	if _, err := gw.store.Revoke(context.Background(), created.Metadata.ID); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		probe, _ := http.NewRequest(http.MethodGet, gw.proxyURL("/v1/models"), nil)
		probe.Header.Set("Authorization", "Bearer "+key)
		response, err := http.DefaultClient.Do(probe)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode == http.StatusUnauthorized {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the revocation was not applied")
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(release)
	if body := <-finished; !strings.HasSuffix(body, "data: [DONE]\n\n") {
		t.Fatalf("the operation in flight was cut: %q", body)
	}
	result := gw.results.last(t)
	if result.AppName != "bot" || result.KeyID != created.Metadata.ID {
		t.Fatalf("result %+v", result)
	}
}

func TestFakeProviderCompleteFlows(t *testing.T) {
	gw := startGateway(t, fakeprovider.Options{}, "")
	key := gw.createKey("bot").Key.Reveal()

	t.Run("anthropic stream", func(t *testing.T) {
		_, body := gw.call("/v1/messages", key, `{"model":"fake-claude","max_tokens":4,"messages":[],"stream":true}`)
		if !strings.Contains(body, "message_stop") {
			t.Fatalf("body %s", body)
		}
		result := gw.results.last(t)
		if !result.UsageComplete || result.Usage.Input.Value != fakeprovider.FixtureInputTokens || result.Usage.Output.Value != fakeprovider.FixtureOutputTokens {
			t.Fatalf("result %+v", result)
		}
	})
	t.Run("openai stream hides the injected usage", func(t *testing.T) {
		_, body := gw.call("/v1/chat/completions", key, gptStream)
		if strings.Contains(body, `"usage"`) || !strings.HasSuffix(body, "data: [DONE]\n\n") {
			t.Fatalf("body %s", body)
		}
		if result := gw.results.last(t); !result.UsageComplete || result.Usage.Output.Value != fakeprovider.FixtureOutputTokens {
			t.Fatalf("result %+v", result)
		}
	})
	t.Run("count tokens", func(t *testing.T) {
		_, body := gw.call("/v1/messages/count_tokens", key, `{"model":"fake-claude","messages":[]}`)
		if body != `{"input_tokens":10}` {
			t.Fatalf("body %s", body)
		}
		result := gw.results.last(t)
		if result.Operation != proxy.OperationCountTokens || result.Usage.Input.Known || result.CountedInputTokens.Value != 10 {
			t.Fatalf("result %+v", result)
		}
	})
}

func TestProviderFailuresThroughTheGateway(t *testing.T) {
	cases := []struct {
		scenario fakeprovider.Scenario
		status   int
		code     string
	}{
		{fakeprovider.ScenarioRateLimit, http.StatusTooManyRequests, "provider_rate_limited"},
		{fakeprovider.ScenarioOverload, 529, "provider_overloaded"},
	}
	for _, tc := range cases {
		t.Run(string(tc.scenario), func(t *testing.T) {
			gw := startGateway(t, fakeprovider.Options{Scenario: tc.scenario}, "")
			response, body := gw.call("/v1/chat/completions", gw.createKey("bot").Key.Reveal(), gptBody)
			if response.StatusCode != tc.status || !strings.Contains(body, tc.code) || strings.Contains(body, "fake rate") {
				t.Fatalf("status %d, body %s", response.StatusCode, body)
			}
		})
	}
	t.Run("disconnect mid-stream", func(t *testing.T) {
		gw := startGateway(t, fakeprovider.Options{Scenario: fakeprovider.ScenarioDisconnectMidstream}, "")
		response, body := gw.call("/v1/messages", gw.createKey("bot").Key.Reveal(), `{"model":"fake-claude","max_tokens":4,"messages":[],"stream":true}`)
		if response.StatusCode != http.StatusOK || !strings.Contains(body, "event: error") || strings.Contains(body, "message_stop") {
			t.Fatalf("status %d, body %s", response.StatusCode, body)
		}
		result := gw.results.last(t)
		if result.UsageComplete || result.TerminalSeen || result.Usage.Input.Value != fakeprovider.FixtureInputTokens {
			t.Fatalf("result %+v", result)
		}
	})
	t.Run("header timeout", func(t *testing.T) {
		gw := startGateway(t, fakeprovider.Options{Scenario: fakeprovider.ScenarioHeaderTimeout}, "  upstream_header_timeout: 150ms\n")
		response, body := gw.call("/v1/chat/completions", gw.createKey("bot").Key.Reveal(), gptBody)
		if response.StatusCode != http.StatusGatewayTimeout || !strings.Contains(body, "provider_timeout") {
			t.Fatalf("status %d, body %s", response.StatusCode, body)
		}
	})
}

func TestClientDisconnectCancelsTheFakeWithinOneSecond(t *testing.T) {
	canceled := make(chan time.Time, 1)
	reached := make(chan struct{}, 8)
	gw := startGateway(t, fakeprovider.Options{
		Scenario: fakeprovider.ScenarioSlowStream,
		Sleep: func(ctx context.Context, _ time.Duration) error {
			reached <- struct{}{}
			<-ctx.Done()
			return ctx.Err()
		},
		OnCancel: func() { canceled <- time.Now() },
	}, "")
	key := gw.createKey("bot").Key.Reveal()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, gw.proxyURL("/v1/messages"),
		strings.NewReader(`{"model":"fake-claude","max_tokens":4,"messages":[],"stream":true}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", key)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			return
		}
		defer func() { _ = response.Body.Close() }()
		buffer := make([]byte, 1024)
		for {
			if _, readErr := response.Body.Read(buffer); readErr != nil {
				return
			}
		}
	}()
	<-reached
	closedAt := time.Now()
	cancel()
	select {
	case at := <-canceled:
		if at.Sub(closedAt) > time.Second {
			t.Fatalf("the provider call stayed open for %s", at.Sub(closedAt))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the provider call was not canceled")
	}
	<-finished
	result := gw.results.last(t)
	if !result.ClientDisconnected || result.Usage.Input.Value != fakeprovider.FixtureInputTokens || result.UsageComplete {
		t.Fatalf("result %+v", result)
	}
}
