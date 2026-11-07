package integration

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
)

const (
	gptBody   = `{"model":"fake-gpt","messages":[{"role":"user","content":"Ola"}]}`
	gptStream = `{"model":"fake-gpt","messages":[{"role":"user","content":"Ola"}],"stream":true}`
)

func TestConfigDeterminesListenersAndUpstream(t *testing.T) {
	gw := startGateway(t, fakeprovider.Options{}, "  allowed_hosts: [gateway.example]\n")
	key := gw.createKey("bot").Key.Reveal()

	if response, _ := gw.call("/v1/chat/completions", key, gptBody); response.StatusCode != http.StatusOK {
		t.Fatalf("status %d", response.StatusCode)
	}
	if request := gw.lastRequest(); request.Path != "/v1/chat/completions" || request.Header.Get("Authorization") != "Bearer "+providerSecret {
		t.Fatalf("upstream got %s %v", request.Path, request.Header)
	}

	for _, tc := range []struct {
		addr net.Addr
		host string
		want int
	}{
		{gw.runtime.ProxyAddr(), "evil.example", http.StatusMisdirectedRequest},
		{gw.runtime.AdminAddr(), "evil.example", http.StatusMisdirectedRequest},
		{gw.runtime.AdminAddr(), "gateway.example", http.StatusOK},
	} {
		request, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://%s/healthz", tc.addr), nil)
		request.Host = tc.host
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != tc.want {
			t.Errorf("%s on %s: %d", tc.host, tc.addr, response.StatusCode)
		}
	}

	request, _ := http.NewRequest(http.MethodPost, gw.proxyURL("/v1/files"), strings.NewReader("x"))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("files: %d", response.StatusCode)
	}
}

func TestHTTPRequestPinsConfigAcrossReload(t *testing.T) {
	release := make(chan struct{})
	reached := make(chan struct{})
	gw := startGateway(t, fakeprovider.Options{
		Scenario:  fakeprovider.ScenarioSlowStream,
		SlowDelay: time.Hour,
		Sleep: func(ctx context.Context, _ time.Duration) error {
			select {
			case reached <- struct{}{}:
			default:
			}
			select {
			case <-release:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	}, "")
	key := gw.createKey("bot").Key.Reveal()

	type outcome struct {
		status int
		body   string
	}
	finished := make(chan outcome, 1)
	go func() {
		response, body := gw.call("/v1/chat/completions", key, gptStream)
		finished <- outcome{response.StatusCode, body}
	}()
	<-reached

	gw.reload(func(text string) string {
		text = strings.ReplaceAll(text, "base_url: "+gw.upstream.URL, "base_url: http://127.0.0.1:9")
		text = strings.Replace(text, "models: [fake-gpt]\napps", "models: [fake-gpt-new]\napps", 1)
		text = strings.ReplaceAll(text, "allowed_models: [fake-claude, fake-gpt]", "allowed_models: [fake-claude, fake-gpt-new]")
		return strings.ReplaceAll(text, "allowed_models: [fake-gpt]", "allowed_models: [fake-gpt-new]")
	})
	close(release)

	got := <-finished
	if got.status != http.StatusOK || !strings.HasSuffix(got.body, "data: [DONE]\n\n") {
		t.Fatalf("the stream in flight changed with the reload: %d %q", got.status, got.body)
	}
	if response, body := gw.call("/v1/chat/completions", key, gptBody); response.StatusCode != http.StatusBadRequest || !strings.Contains(body, "model_unknown") {
		t.Fatalf("a new request must see the new configuration: %d %s", response.StatusCode, body)
	}
}
