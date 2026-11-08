package pricing_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/fakeprovider"
	"github.com/JoaoVictorVM/comporta/internal/pricing"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

func TestFixtureEstimateNeverUnderActualInput(t *testing.T) {
	upstream := httptest.NewServer(fakeprovider.New(fakeprovider.Options{}))
	defer upstream.Close()
	for _, format := range []string{"anthropic", "openai"} {
		for _, kind := range []string{"plain", "unicode", "tools", "remote-image", "base64-image"} {
			t.Run(format+"/"+kind, func(t *testing.T) {
				body, path := testkit.OpenAIBody(), "/v1/chat/completions"
				if format == "anthropic" {
					body, path = testkit.AnthropicBody(), "/v1/messages"
				}
				images := int64(0)
				switch kind {
				case "unicode":
					body = strings.ReplaceAll(body, testkit.Marker(), testkit.Prompt("日本語 العربية português 🧪"))
				case "tools":
					body = testkit.WithField(body, `"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]`)
				case "remote-image", "base64-image":
					var doc map[string]any
					if err := json.Unmarshal([]byte(body), &doc); err != nil {
						t.Fatal(err)
					}
					url := "https://image.example/test.png"
					if kind == "base64-image" {
						url = "data:image/png;base64,aGVsbG8="
					}
					var image any = map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}
					if format == "anthropic" {
						image = map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": url}}
					}
					doc["messages"] = []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": testkit.Prompt("synthetic image")}, image}}}
					encoded, err := json.Marshal(doc)
					if err != nil {
						t.Fatal(err)
					}
					body = string(encoded)
					images = 1
				}
				response, err := http.Post(upstream.URL+path, "application/json", strings.NewReader(body))
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				data, err := io.ReadAll(response.Body)
				if err != nil {
					t.Fatal(err)
				}
				var result struct {
					Usage struct {
						Input  int64 `json:"input_tokens"`
						Prompt int64 `json:"prompt_tokens"`
					}
				}
				if err = json.Unmarshal(data, &result); err != nil {
					t.Fatal(err)
				}
				actual := max(result.Usage.Input, result.Usage.Prompt)
				if actual == 0 {
					t.Fatal("fixture has no observed input")
				}
				estimate, err := pricing.EstimateInput(int64(len(body)), int64(len(body)), images)
				if err != nil || estimate < actual {
					t.Fatalf("estimate %d below observed %d: %v", estimate, actual, err)
				}
			})
		}
	}
}
