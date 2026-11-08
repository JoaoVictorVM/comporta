package integration

import (
	"context"
	"encoding/json"
	"github.com/JoaoVictorVM/comporta/internal/storage"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

func TestUnpricedModelReturnsDocumented400WithoutProvider(t *testing.T) {
	for _, format := range []string{"anthropic", "openai"} {
		t.Run(format, func(t *testing.T) {
			var calls atomic.Int64
			gw := startMeteredGateway(t, fakeprovider.Options{OnRequest: func(fakeprovider.Request) { calls.Add(1) }})
			key := gw.createKey("bot").Key.Reveal()
			gw.reload(func(text string) string {
				text = strings.ReplaceAll(text, "models: [fake-claude]", "models: [fake-claude, unpriced-claude]")
				text = strings.ReplaceAll(text, "models: [fake-gpt]", "models: [fake-gpt, unpriced-gpt]")
				return strings.ReplaceAll(text, "base_url: "+gw.upstream.URL+"/v1", "base_url: https://api.openai.com/v1")
			})
			path, body, model := "/v1/messages", testkit.AnthropicBody(), "unpriced-claude"
			if format == "openai" {
				path, body, model = "/v1/chat/completions", testkit.OpenAIBody(), "unpriced-gpt"
				body = strings.Replace(body, "fake-gpt", model, 1)
			} else {
				body = strings.Replace(body, "fake-claude", model, 1)
			}
			response, answer := gw.call(path, key, body)
			var decoded map[string]json.RawMessage
			if err := json.Unmarshal([]byte(answer), &decoded); err != nil {
				t.Fatal(err)
			}
			var detail struct {
				Message string `json:"message"`
				Code    string `json:"comporta_code"`
			}
			if err := json.Unmarshal(decoded["error"], &detail); err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != 400 || detail.Code != "price_unknown" || detail.Message != "no price defined for model \""+model+"\"; add it under pricing in comporta.yaml" || calls.Load() != 0 {
				t.Fatal("unpriced model did not fail before provider")
			}
		})
	}
}

func TestAdmissionWriteFailureNeverCallsProvider(t *testing.T) {
	var calls atomic.Int64
	gw := startMeteredGateway(t, fakeprovider.Options{OnRequest: func(fakeprovider.Request) { calls.Add(1) }})
	key := gw.createKey("bot").Key.Reveal()
	gw.usage.Close()
	response, answer := gw.call("/v1/chat/completions", key, testkit.OpenAIBody())
	if response.StatusCode != 503 || !strings.Contains(answer, "usage_storage_unavailable") || calls.Load() != 0 {
		t.Fatal("failed admission reached provider")
	}
}

func TestProxyUsageProducesCorrectPricedRecord(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "stream"}[stream], func(t *testing.T) {
			gw := startMeteredGateway(t, fakeprovider.Options{})
			key := gw.createKey("bot").Key.Reveal()
			body := testkit.OpenAIBody()
			if stream {
				body = testkit.OpenAIStreamBody()
			}
			if response, _ := gw.callAs("/v1/chat/completions", key, testkit.UserMarker(), body); response.StatusCode != 200 {
				t.Fatalf("status %d", response.StatusCode)
			}
			gw.results.last(t)
			if err := gw.accounting.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			hash, err := gw.identity.LookupHash("bot", testkit.UserMarker())
			if err != nil {
				t.Fatal(err)
			}
			rows, err := gw.usage.Daily(context.Background(), storage.Filter{AppName: "bot", UserHash: hash.Hex()})
			if err != nil || len(rows) != 1 || rows[0].Cost != 32 || rows[0].Input != 10 || rows[0].Output != 2 {
				t.Fatalf("unexpected priced totals: %+v %v", rows, err)
			}
			request := gw.lastRequest()
			var document map[string]json.RawMessage
			if err = json.Unmarshal(request.Body, &document); err != nil {
				t.Fatal(err)
			}
			if string(document["max_tokens"]) != "4096" {
				t.Fatal("effective output cap missing")
			}
		})
	}
}

func TestMeteringPinsPricesAcrossReload(t *testing.T) {
	reached, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int64
	gw := startMeteredGateway(t, fakeprovider.Options{OnRequest: func(fakeprovider.Request) {
		if calls.Add(1) == 1 {
			close(reached)
			<-release
		}
	}})
	key := gw.createKey("bot").Key.Reveal()
	first := make(chan int, 1)
	go func() {
		response, _ := gw.call("/v1/chat/completions", key, testkit.OpenAIBody())
		first <- response.StatusCode
	}()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("provider barrier not reached")
	}
	gw.reload(func(text string) string {
		return strings.ReplaceAll(text, "input: 2, output: 6", "input: 4, output: 12")
	})
	close(release)
	if status := <-first; status != 200 {
		t.Fatal(status)
	}
	gw.results.last(t)
	if response, _ := gw.call("/v1/chat/completions", key, testkit.OpenAIBody()); response.StatusCode != 200 {
		t.Fatal(response.StatusCode)
	}
	gw.results.waitCount(t, 2)
	if err := gw.accounting.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := gw.usage.Daily(context.Background(), storage.Filter{AppName: "bot"})
	if err != nil || len(rows) != 1 || rows[0].Cost != 96 {
		t.Fatalf("price reload totals: %+v %v", rows, err)
	}
}

func TestNoProviderBeforeAdmissionCommit(t *testing.T) {
	var gw *gateway
	durable := make(chan bool, 1)
	gw = startMeteredGateway(t, fakeprovider.Options{OnRequest: func(fakeprovider.Request) {
		pending, err := gw.usage.Pending(context.Background())
		durable <- err == nil && len(pending) == 1
	}})
	key := gw.createKey("bot").Key.Reveal()
	if response, _ := gw.call("/v1/chat/completions", key, testkit.OpenAIBody()); response.StatusCode != 200 {
		t.Fatal(response.StatusCode)
	}
	if !<-durable {
		t.Fatal("provider reached before admission commit")
	}
}

func TestInterruptedProxyUsageSettlesConservatively(t *testing.T) {
	gw := startMeteredGateway(t, fakeprovider.Options{Scenario: fakeprovider.ScenarioDisconnectMidstream})
	key := gw.createKey("bot").Key.Reveal()
	gw.call("/v1/chat/completions", key, testkit.OpenAIStreamBody())
	gw.results.last(t)
	if err := gw.accounting.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := gw.usage.Daily(context.Background(), storage.Filter{AppName: "bot"})
	if err != nil || len(rows) != 1 || rows[0].UsageMissing != 1 || rows[0].Cost == 0 {
		t.Fatalf("partial usage: %+v %v", rows, err)
	}
}

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
