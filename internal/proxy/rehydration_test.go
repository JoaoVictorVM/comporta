package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/auth"
	"github.com/JoaoVictorVM/comporta/internal/sensitivedata"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

// echoReply answers with the placeholders the provider received, inside the
// response fields that are restored and inside fields that must stay untouched.
func echoReply(upstream **recordingUpstream, build func(tokens []string) string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		(*upstream).mu.Lock()
		tokens := tokenPattern.FindAllString(string((*upstream).body), -1)
		(*upstream).mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(build(tokens)))
	}
}

func rehydrationFixture(t *testing.T, settings string, build func(tokens []string) string) (*fixture, *recordingUpstream) {
	t.Helper()
	var upstream *recordingUpstream
	fx, up := sensitiveFixture(t, sensitiveSetup{settings: settings, recorder: &sensitiveRecorder{}, reply: echoReply(&upstream, build)})
	upstream = up
	return fx, up
}

func TestRehydrateExactTokens(t *testing.T) {
	fx, upstream := rehydrationFixture(t, detectionSettings("email: redact", "rehydrate: true"), func(tokens []string) string {
		return `{"id":"msg_` + tokens[0] + `","type":"message","content":[{"type":"text","text":"Contato: ` + tokens[0] + `"},` +
			`{"type":"tool_use","id":"toolu_1","name":"` + tokens[0] + `","input":{"to":"` + tokens[0] + `"}}],` +
			`"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":5}}`
	})
	body := `{"model":"fake-claude","max_tokens":10,"messages":[{"role":"user","content":"` + testkit.Prompt("Responda para cliente@example.test") + `"}]}`
	response, answer := fx.post("/v1/messages", "anthropic", body)
	if response.StatusCode != 200 {
		t.Fatalf("status %d: %s", response.StatusCode, answer)
	}
	if strings.Contains(upstreamBody(t, upstream), "cliente@") {
		t.Fatal("the provider received the original value")
	}
	var parsed struct {
		ID      string `json:"id"`
		Content []struct {
			Text  string            `json:"text"`
			Name  string            `json:"name"`
			Input map[string]string `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(answer), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Content[0].Text != "Contato: cliente@example.test" || parsed.Content[1].Input["to"] != "cliente@example.test" {
		t.Fatalf("not restored: %s", answer)
	}
	if !tokenPattern.MatchString(parsed.ID) || !tokenPattern.MatchString(parsed.Content[1].Name) {
		t.Fatalf("control fields were restored: %s", answer)
	}
	if response.Header.Get("Content-Length") != "" && response.ContentLength != int64(len(answer)) {
		t.Fatalf("content length %d for %d bytes", response.ContentLength, len(answer))
	}
	result := fx.sink.wait(t)
	if !result.Usage.Input.Known || result.Usage.Input.Value != 7 || result.Usage.Output.Value != 5 {
		t.Fatalf("usage = %+v", result.Usage)
	}
}

func TestRehydrateOpenAI(t *testing.T) {
	fx, _ := rehydrationFixture(t, detectionSettings("email: redact", "rehydrate: true"), func(tokens []string) string {
		args, _ := json.Marshal(`{"to": "` + tokens[0] + `"}`)
		return `{"id":"chatcmpl-1","object":"chat.completion","model":"fake-gpt","choices":[{"index":0,"message":{"role":"assistant","content":"Contato ` + tokens[0] + `",` +
			`"tool_calls":[{"id":"call_1","type":"function","function":{"name":"send","arguments":` + string(args) + `}}]},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":3,"completion_tokens":2}}`
	})
	body := `{"model":"fake-gpt","messages":[{"role":"user","content":"` + testkit.Prompt("para ana@example.org") + `"}]}`
	response, answer := fx.post("/v1/chat/completions", "openai", body)
	if response.StatusCode != 200 {
		t.Fatalf("status %d: %s", response.StatusCode, answer)
	}
	if !strings.Contains(answer, `"content":"Contato ana@example.org"`) || !strings.Contains(answer, `"arguments":"{\"to\": \"ana@example.org\"}"`) {
		t.Fatalf("answer = %s", answer)
	}
}

func TestForeignAndPartialTokens(t *testing.T) {
	foreign := "[EMAIL_1_" + strings.Repeat("0", 24) + "]"
	fx, _ := rehydrationFixture(t, detectionSettings("email: redact", "rehydrate: true"), func(tokens []string) string {
		nonce := tokenPattern.FindStringSubmatch(tokens[0])[3]
		text := foreign + " [EMAIL_2_" + nonce + "] EMAIL_1_" + nonce + "] [EMAIL_1_" + nonce + " [email_1_" + nonce + "]"
		return `{"content":[{"type":"text","text":"` + text + `"}],"usage":{"input_tokens":1,"output_tokens":1}}`
	})
	body := `{"model":"fake-claude","max_tokens":10,"messages":[{"role":"user","content":"` + testkit.Prompt("para ana@example.org") + `"}]}`
	response, answer := fx.post("/v1/messages", "anthropic", body)
	if response.StatusCode != 200 || strings.Contains(answer, "ana@example.org") || !strings.Contains(answer, foreign) {
		t.Fatalf("status %d: %s", response.StatusCode, answer)
	}
}

func TestOnePassRestoration(t *testing.T) {
	scanner := sensitivedata.NewScanner()
	policy := sensitivedata.Policy{Rehydrate: true}
	policy.Set(sensitivedata.Email, sensitivedata.ModeRedact)
	outcome, err := scanner.Scan(context.Background(), policy, func(emit sensitivedata.Emit) error { return emit("ana@example.org") })
	if err != nil {
		t.Fatal(err)
	}
	token, _ := outcome.Rewritten(0)
	body := []byte(`{"content":[{"type":"text","text":"` + token + token + `"}]}`)
	got, err := rehydrateResponse(auth.FormatAnthropic, body, outcome.Map(), maxResponseBytes)
	if err != nil || string(got) != `{"content":[{"type":"text","text":"ana@example.organa@example.org"}]}` {
		t.Fatalf("got %s, %v", got, err)
	}
}

func TestJSONEscaping(t *testing.T) {
	scanner := sensitivedata.NewScanner()
	policy := sensitivedata.Policy{Rehydrate: true}
	policy.Set(sensitivedata.Secret, sensitivedata.ModeRedact)
	key := "-----BEGIN " + "PRIVATE KEY-----\nQUJDRA==\n-----END PRIVATE KEY-----"
	outcome, err := scanner.Scan(context.Background(), policy, func(emit sensitivedata.Emit) error { return emit("k " + key) })
	if err != nil {
		t.Fatal(err)
	}
	rewritten, _ := outcome.Rewritten(0)
	token := strings.TrimPrefix(rewritten, "k ")
	args, _ := json.Marshal(`{"pem":"` + token + `"}`)
	body := []byte(`{"choices":[{"message":{"content":"` + token + ` <b>","tool_calls":[{"function":{"arguments":` + string(args) + `}}]}}],"usage":{"prompt_tokens":1}}`)
	got, err := rehydrateResponse(auth.FormatOpenAI, body, outcome.Map(), maxResponseBytes)
	if err != nil || !json.Valid(got) {
		t.Fatalf("invalid result %s: %v", got, err)
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Function struct{ Arguments string } `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatal(err)
	}
	message := parsed.Choices[0].Message
	var inner map[string]string
	if message.Content != key+" <b>" || json.Unmarshal([]byte(message.ToolCalls[0].Function.Arguments), &inner) != nil || inner["pem"] != key {
		t.Fatalf("parsed = %+v", message)
	}
	if !strings.Contains(string(got), `-----END PRIVATE KEY----- <b>"`) {
		t.Fatalf("HTML escaping changed the text: %s", got)
	}
}

func TestNoUsageMutation(t *testing.T) {
	scanner := sensitivedata.NewScanner()
	policy := sensitivedata.Policy{Rehydrate: true}
	policy.Set(sensitivedata.Email, sensitivedata.ModeRedact)
	outcome, _ := scanner.Scan(context.Background(), policy, func(emit sensitivedata.Emit) error { return emit("ana@example.org") })
	token, _ := outcome.Rewritten(0)
	prefix := `{"id":"msg_1" , "model":"` + token + `","content":[{"type":"text","text":"`
	suffix := `"}],"stop_reason":"` + token + `","usage":{"input_tokens":1.0e1,"output_tokens":2},"x":{"text":"` + token + `"}}`
	got, err := rehydrateResponse(auth.FormatAnthropic, []byte(prefix+token+suffix), outcome.Map(), maxResponseBytes)
	if err != nil || string(got) != prefix+"ana@example.org"+suffix {
		t.Fatalf("got %s, %v", got, err)
	}
	unchanged := []byte(`{"content":[{"type":"text","text":"nada"}],"usage":{}}`)
	if same, _ := rehydrateResponse(auth.FormatAnthropic, unchanged, outcome.Map(), maxResponseBytes); &same[0] != &unchanged[0] {
		t.Fatal("an untouched body must be forwarded as is")
	}
}

func TestRehydrationExpansionLimit(t *testing.T) {
	scanner := sensitivedata.NewScanner()
	policy := sensitivedata.Policy{Rehydrate: true}
	policy.Set(sensitivedata.Email, sensitivedata.ModeRedact)
	value := strings.Repeat("a", 60) + "@" + strings.Repeat("b", 60) + ".org"
	outcome, _ := scanner.Scan(context.Background(), policy, func(emit sensitivedata.Emit) error { return emit(value) })
	token, _ := outcome.Rewritten(0)
	body := []byte(`{"content":[{"type":"text","text":"` + strings.Repeat(token, 4) + `"}]}`)
	if _, err := rehydrateResponse(auth.FormatAnthropic, body, outcome.Map(), len(body)+10); err == nil {
		t.Fatal("expansion over the limit accepted")
	}
	if _, err := rehydrateResponse(auth.FormatAnthropic, []byte(`{"a":1,"a":2}`), outcome.Map(), maxResponseBytes); err == nil {
		t.Fatal("an ambiguous response accepted")
	}

	// End to end: the failure answers 502 without the provider's body and still
	// reports the usage the provider generated.
	fx, _ := rehydrationFixture(t, detectionSettings("email: redact", "rehydrate: true"), func(tokens []string) string {
		return `{"content":[{"type":"text","text":"` + tokens[0] + `"}],"content":[],"usage":{"input_tokens":4,"output_tokens":3}}`
	})
	body2 := `{"model":"fake-claude","max_tokens":10,"messages":[{"role":"user","content":"` + testkit.Prompt("para ana@example.org") + `"}]}`
	response, answer := fx.post("/v1/messages", "anthropic", body2)
	if response.StatusCode != 502 || comportaCode(t, answer) != "response_rehydration_failed" || strings.Contains(answer, "EMAIL") {
		t.Fatalf("status %d: %s", response.StatusCode, answer)
	}
	result := fx.sink.wait(t)
	if result.Usage.Input.Value != 4 || result.Usage.Output.Value != 3 || result.ErrorCode != CodeRehydrationFailed {
		t.Fatalf("result = %+v", result)
	}
}

func TestStreamWarning(t *testing.T) {
	stream := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":1}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}
	for _, content := range []string{"para ana@example.org", "sem dados"} {
		fx, upstream := sensitiveFixture(t, sensitiveSetup{settings: detectionSettings("email: redact", "rehydrate: true"), recorder: &sensitiveRecorder{}, reply: stream})
		body := `{"model":"fake-claude","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"` + testkit.Prompt(content) + `"}]}`
		response, answer := fx.post("/v1/messages", "anthropic", body)
		if response.StatusCode != 200 || response.Header.Get("X-Comporta-Rehydration") != "unsupported-for-stream" || !strings.Contains(answer, "message_stop") {
			t.Fatalf("status %d header %q: %s", response.StatusCode, response.Header.Get("X-Comporta-Rehydration"), answer)
		}
		if strings.Contains(upstreamBody(t, upstream), "ana@") {
			t.Fatal("a stream must still redact its input")
		}
	}
	// Without rehydration asked for, no header.
	fx, _ := sensitiveFixture(t, sensitiveSetup{settings: detectionSettings("email: redact"), recorder: &sensitiveRecorder{}, reply: stream})
	body := `{"model":"fake-claude","max_tokens":10,"stream":true,"messages":[{"role":"user","content":"` + testkit.Prompt("x") + `"}]}`
	if response, _ := fx.post("/v1/messages", "anthropic", body); response.Header.Get("X-Comporta-Rehydration") != "" {
		t.Fatal("warning without rehydration configured")
	}
}

func TestSensitiveCleanupOnDisconnect(t *testing.T) {
	scanner := sensitivedata.NewScanner()
	policy := sensitivedata.Policy{Rehydrate: true}
	policy.Set(sensitivedata.Email, sensitivedata.ModeRedact)
	newState := func() *sensitiveState {
		outcome, err := scanner.Scan(context.Background(), policy, func(emit sensitivedata.Emit) error { return emit("ana@example.org") })
		if err != nil {
			t.Fatal(err)
		}
		state := &sensitiveState{reverse: outcome.Map()}
		outcome.DetachMap()
		outcome.Release()
		if state.restoring() == nil {
			t.Fatal("the map was released with the outcome")
		}
		return state
	}
	handler := NewHandler(Options{})

	state := newState()
	reverse := state.reverse
	op := &operation{handler: handler, w: newTrackedWriter(httptest.NewRecorder()), sensitive: state}
	op.finish()
	if reverse.Len() != 0 || state.restoring() != nil {
		t.Fatal("finish kept the reverse map")
	}

	// A disconnect aborts the handler with ErrAbortHandler: the map still goes.
	state = newState()
	reverse = state.reverse
	op = &operation{handler: handler, w: newTrackedWriter(httptest.NewRecorder()), sensitive: state}
	func() {
		defer func() { _ = recover() }()
		defer op.finish()
		panic(http.ErrAbortHandler)
	}()
	if reverse.Len() != 0 {
		t.Fatal("an aborted operation kept the reverse map")
	}
}
