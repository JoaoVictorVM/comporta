package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/auth"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLocalCatalogFilteredByAppAndFormat(t *testing.T) {
	upstream, server := startUpstream(t, nil)
	fx := newFixture(t, server.URL, fixtureOptions{})

	_, body := fx.do(fx.request(http.MethodGet, "/v1/models", "openai", ""))
	want := `{"object":"list","data":[{"id":"fake-gpt","object":"model","created":0,"owned_by":"comporta"}]}`
	if body != want {
		t.Fatalf("openai: %s", body)
	}
	_, body = fx.do(fx.request(http.MethodGet, "/v1/models", "anthropic", ""))
	want = `{"data":[{"id":"fake-claude","type":"model","display_name":"fake-claude","created_at":"1970-01-01T00:00:00Z"}],"has_more":false,"first_id":"fake-claude","last_id":"fake-claude"}`
	if body != want {
		t.Fatalf("anthropic: %s", body)
	}
	if upstream.count() != 0 {
		t.Fatal("the catalog called a provider")
	}
}

func TestModelsRequiresAuthentication(t *testing.T) {
	_, server := startUpstream(t, nil)
	fx := newFixture(t, server.URL, fixtureOptions{})
	request := fx.request(http.MethodGet, "/v1/models", "openai", "")
	request.Header.Del("Authorization")
	response, body := fx.do(request)
	if response.StatusCode != http.StatusUnauthorized || strings.Contains(body, "fake-") {
		t.Fatalf("status %d, body %s", response.StatusCode, body)
	}
}

func TestModelsNeverCallsUpstream(t *testing.T) {
	upstream, server := startUpstream(t, nil)
	fx := newFixture(t, server.URL, fixtureOptions{})
	for _, format := range []string{"openai", "anthropic"} {
		if response, _ := fx.do(fx.request(http.MethodGet, "/v1/models", format, "")); response.StatusCode != http.StatusOK {
			t.Fatalf("status %d", response.StatusCode)
		}
	}
	if upstream.count() != 0 {
		t.Fatal("provider contacted")
	}
	if len(fx.sink.all()) != 0 {
		t.Fatal("a local listing is not an attempt")
	}
}

func TestOpenAIModelsRefusesQuery(t *testing.T) {
	_, server := startUpstream(t, nil)
	fx := newFixture(t, server.URL, fixtureOptions{})
	response, body := fx.do(fx.request(http.MethodGet, "/v1/models?limit=1", "openai", ""))
	if response.StatusCode != http.StatusBadRequest || comportaCode(t, body) != "invalid_request" {
		t.Fatalf("status %d, body %s", response.StatusCode, body)
	}
}

func pageOf(t *testing.T, models []string, query string) anthropicPage {
	t.Helper()
	payload, failure := anthropicModels(models, query)
	if failure != nil {
		t.Fatalf("%q rejected: %+v", query, failure)
	}
	return payload.(anthropicPage)
}

func TestAnthropicPagination(t *testing.T) {
	models := []string{"a", "b", "c", "d"}
	ids := func(page anthropicPage) string {
		names := make([]string, 0, len(page.Data))
		for _, model := range page.Data {
			names = append(names, model.ID)
		}
		return strings.Join(names, ",")
	}
	cases := []struct {
		query   string
		want    string
		hasMore bool
	}{
		{"", "a,b,c,d", false},
		{"limit=2", "a,b", true},
		{"limit=4", "a,b,c,d", false},
		{"limit=2&after_id=a", "b,c", true},
		{"after_id=c", "d", false},
		{"after_id=d", "", false},
		{"before_id=d&limit=2", "b,c", true},
		{"before_id=c&limit=2", "a,b", false},
		{"before_id=a", "", false},
	}
	for _, tc := range cases {
		page := pageOf(t, models, tc.query)
		if ids(page) != tc.want || page.HasMore != tc.hasMore {
			t.Errorf("%q: got %s, more %v", tc.query, ids(page), page.HasMore)
		}
	}
	for _, query := range []string{
		"limit=0", "limit=1001", "limit=x", "limit=1&limit=2", "after_id=zz", "before_id=zz",
		"after_id=a&before_id=b", "unknown=1", "limit=%zz",
	} {
		if _, failure := anthropicModels(models, query); failure == nil || failure.Status != http.StatusBadRequest {
			t.Errorf("%q: got %+v", query, failure)
		}
	}
}

func TestEmptyCatalogBothFormats(t *testing.T) {
	for _, format := range []auth.Format{auth.FormatOpenAI, auth.FormatAnthropic} {
		var payload any
		if format == auth.FormatOpenAI {
			payload = openAIModels(nil)
		} else {
			payload = pageOf(t, nil, "")
		}
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		want := `{"object":"list","data":[]}`
		if format == auth.FormatAnthropic {
			want = `{"data":[],"has_more":false,"first_id":null,"last_id":null}`
		}
		if string(data) != want {
			t.Fatalf("got %s", data)
		}
	}
}

func TestCatalogUsesAllowedModels(t *testing.T) {
	_, server := startUpstream(t, nil)
	fx := newFixture(t, server.URL, fixtureOptions{})
	snapshot := fx.config.Capture()
	if got := catalog(snapshot, "missing-app", auth.FormatOpenAI); got != nil {
		t.Fatalf("an unknown app has no catalog: %v", got)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	serveModels(recorder, request, auth.FormatOpenAI, snapshot, "missing-app", "")
	if !strings.Contains(recorder.Body.String(), `"data":[]`) {
		t.Fatalf("body %s", recorder.Body.String())
	}
}
