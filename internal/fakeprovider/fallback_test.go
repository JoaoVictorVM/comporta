package fakeprovider

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serve(t *testing.T, h http.Handler, path, body string) (int, string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	recorder := httptest.NewRecorder()
	h.ServeHTTP(recorder, request)
	data, _ := io.ReadAll(recorder.Result().Body)
	return recorder.Code, string(data)
}

func TestModelScenariosPlayAChain(t *testing.T) {
	h := New(Options{ByModel: map[string]Scenario{"first": ScenarioOverload, "second": ScenarioServerError, "bad": ScenarioBadRequest}})
	for model, want := range map[string]int{"first": 529, "second": 500, "bad": 400, "third": 200} {
		status, _ := serve(t, h, "/v1/messages", `{"model":"`+model+`","max_tokens":1,"messages":[]}`)
		if status != want {
			t.Errorf("%s: %d, want %d", model, status, want)
		}
	}
}

func TestStreamErrorScenarios(t *testing.T) {
	before := New(Options{Scenario: ScenarioStreamErrorBeforeChunk})
	status, body := serve(t, before, "/v1/messages", `{"model":"m","stream":true,"messages":[]}`)
	if status != 200 || !strings.HasPrefix(body, "event: error\n") || strings.Contains(body, "message_start") {
		t.Fatalf("before chunk: %d %q", status, body)
	}
	if status, _ = serve(t, before, "/v1/messages", `{"model":"m","messages":[]}`); status != 529 {
		t.Fatalf("non-streaming request of the before-chunk scenario: %d", status)
	}
	status, body = serve(t, before, "/v1/chat/completions", `{"model":"m","stream":true,"messages":[]}`)
	if status != 200 || !strings.HasPrefix(body, `data: {"error":{"type":"overloaded_error"`) {
		t.Fatalf("openai before chunk: %d %q", status, body)
	}
	after := New(Options{Scenario: ScenarioStreamErrorAfterChunk})
	status, body = serve(t, after, "/v1/messages", `{"model":"m","stream":true,"messages":[]}`)
	if status != 200 || !strings.HasPrefix(body, "event: message_start") || !strings.Contains(body, "event: error") || strings.Contains(body, "message_stop") {
		t.Fatalf("after chunk: %d %q", status, body)
	}
	if status, _ = serve(t, after, "/v1/messages", `{"model":"m","messages":[]}`); status != 200 {
		t.Fatalf("non-streaming request of the after-chunk scenario: %d", status)
	}
}
