package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/sensitivedata"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

var tokenPattern = regexp.MustCompile(`\[([A-Z]+)_(\d+)_([0-9a-f]{24})\]`)

// syntheticAnthropicKey is assembled at run time so the repository holds no
// complete credential shape.
func syntheticAnthropicKey() string { return "sk-" + "ant-" + "api03-" + strings.Repeat("Zq7_", 10) }

type sensitiveRecorder struct {
	mu         sync.Mutex
	published  []sensitivedata.Summary
	cuts       []SensitiveCut
	publishErr error
	cutErr     error
	order      *[]string
}

func (r *sensitiveRecorder) PublishSensitive(_ context.Context, _ *Envelope, _ time.Time, summary sensitivedata.Summary) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.order != nil {
		*r.order = append(*r.order, "sensitive-event")
	}
	r.published = append(r.published, summary.Clone())
	return r.publishErr
}

func (r *sensitiveRecorder) RecordSensitiveCut(_ context.Context, _ *config.Snapshot, _ *Envelope, cut SensitiveCut) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.order != nil {
		*r.order = append(*r.order, "sensitive-cut")
	}
	r.cuts = append(r.cuts, cut)
	return r.cutErr
}

func detectionSettings(lines ...string) string {
	out := "    detection:\n"
	for _, line := range lines {
		out += "      " + line + "\n"
	}
	return out
}

// formatReply answers with the usage shape of the route's format.
func formatReply(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if strings.HasSuffix(r.URL.Path, "/chat/completions") {
		_, _ = w.Write([]byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
		return
	}
	if strings.HasSuffix(r.URL.Path, "/count_tokens") {
		_, _ = w.Write([]byte(`{"input_tokens":3}`))
		return
	}
	_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
}

type sensitiveSetup struct {
	settings   string
	recorder   *sensitiveRecorder
	accounting Accounting
	hooks      Hooks
	scanner    *sensitivedata.Scanner
	reply      http.HandlerFunc
}

func sensitiveFixture(t *testing.T, setup sensitiveSetup) (*fixture, *recordingUpstream) {
	t.Helper()
	reply := setup.reply
	if reply == nil {
		reply = formatReply
	}
	upstream, server := startUpstream(t, reply)
	scanner := setup.scanner
	if scanner == nil {
		scanner = sensitivedata.NewScanner()
	}
	var recorder SensitiveRecorder
	if setup.recorder != nil {
		recorder = setup.recorder
	}
	stage := &SensitiveStage{Scanner: scanner, Recorder: recorder}
	fx := newFixture(t, server.URL, fixtureOptions{listeners: setup.settings, sensitive: stage, accounting: setup.accounting, hooks: setup.hooks})
	return fx, upstream
}

func upstreamBody(t *testing.T, upstream *recordingUpstream) string {
	t.Helper()
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if !json.Valid(upstream.body) {
		t.Fatalf("forwarded body is not JSON: %s", upstream.body)
	}
	return string(upstream.body)
}

func TestAnthropicTextSurfaces(t *testing.T) {
	recorder := &sensitiveRecorder{}
	fx, upstream := sensitiveFixture(t, sensitiveSetup{settings: detectionSettings("email: redact"), recorder: recorder})
	body := `{"model":"fake-claude","max_tokens":10,
"system":[{"type":"text","text":"system s1@example.org"}],
"tools":[{"name":"lookup","description":"tool t1@example.org","input_schema":{"type":"object","properties":{"p1@example.org":{"type":"string","description":"prop d1@example.org","enum":["e1@example.org"],"default":"f1@example.org","examples":["x1@example.org"]}}}}],
"messages":[
 {"role":"user","content":"plain m1@example.org"},
 {"role":"assistant","content":[{"type":"text","text":"block m2@example.org"},{"type":"tool_use","id":"toolu_01","name":"lookup","input":{"to":"i1@example.org","list":["i2@example.org",3]}}]},
 {"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01","content":"result r1@example.org"},{"type":"tool_result","tool_use_id":"toolu_01","content":[{"type":"text","text":"nested r2@example.org"}]}]}
]}`
	response, answer := fx.post("/v1/messages", "anthropic", body)
	if response.StatusCode != 200 {
		t.Fatalf("status %d: %s", response.StatusCode, answer)
	}
	sent := upstreamBody(t, upstream)
	for _, value := range []string{"s1@", "t1@", "d1@", "e1@", "f1@", "x1@", "m1@", "m2@", "i1@", "i2@", "r1@", "r2@"} {
		if strings.Contains(sent, value) {
			t.Errorf("%s reached the provider", value)
		}
	}
	if !strings.Contains(sent, `"p1@example.org":`) {
		t.Error("a property name was changed")
	}
	if got := len(tokenPattern.FindAllString(sent, -1)); got != 12 {
		t.Fatalf("placeholders = %d, want 12", got)
	}
	if len(recorder.published) != 1 || recorder.published[0][0].Count != 12 {
		t.Fatalf("published = %+v", recorder.published)
	}
}

func TestOpenAITextSurfaces(t *testing.T) {
	fx, upstream := sensitiveFixture(t, sensitiveSetup{settings: detectionSettings("email: redact"), recorder: &sensitiveRecorder{}})
	body := `{"model":"fake-gpt",
"tools":[{"type":"function","function":{"name":"send","description":"desc t1@example.org","parameters":{"type":"object","properties":{"to":{"type":"string","description":"to d1@example.org"}}}}}],
"functions":[{"name":"legacy","description":"legacy l1@example.org","parameters":{"type":"object"}}],
"messages":[
 {"role":"system","content":"system s1@example.org"},
 {"role":"user","content":[{"type":"text","text":"part p1@example.org"},{"type":"image_url","image_url":{"url":"https://img.example/a@b.org.png"}}]},
 {"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"send","arguments":"{\"to\":\"a1@example.org\"}"}}]},
 {"role":"assistant","content":null,"function_call":{"name":"legacy","arguments":"{\"to\":\"a2@example.org\"}"}},
 {"role":"tool","tool_call_id":"call_1","content":"tool result r1@example.org"}
]}`
	response, answer := fx.post("/v1/chat/completions", "openai", body)
	if response.StatusCode != 200 {
		t.Fatalf("status %d: %s", response.StatusCode, answer)
	}
	sent := upstreamBody(t, upstream)
	for _, value := range []string{"t1@", "d1@", "l1@", "s1@", "p1@", "a1@", "a2@", "r1@"} {
		if strings.Contains(sent, value) {
			t.Errorf("%s reached the provider", value)
		}
	}
	if !strings.Contains(sent, "https://img.example/a@b.org.png") {
		t.Error("an image reference was changed")
	}
}

func TestToolArgumentsJSON(t *testing.T) {
	cpf := testkit.DocumentID()
	fx, upstream := sensitiveFixture(t, sensitiveSetup{settings: detectionSettings("email: redact", "cpf: redact"), recorder: &sensitiveRecorder{}})
	arguments := `{"email": "ana@example.org", "n": ` + cpf + `, "nested": ["doc ` + cpf + `", true, null]}`
	encoded, _ := json.Marshal(arguments)
	body := `{"model":"fake-gpt","messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":` + string(encoded) + `}}]},{"role":"assistant","content":null,"function_call":{"name":"g","arguments":"free text ana@example.org"}}]}`
	if response, answer := fx.post("/v1/chat/completions", "openai", body); response.StatusCode != 200 {
		t.Fatalf("status %d: %s", response.StatusCode, answer)
	}
	var sent struct {
		Messages []struct {
			ToolCalls []struct {
				Function struct{ Arguments string } `json:"function"`
			} `json:"tool_calls"`
			FunctionCall struct{ Arguments string } `json:"function_call"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(upstreamBody(t, upstream)), &sent); err != nil {
		t.Fatal(err)
	}
	rebuilt := sent.Messages[0].ToolCalls[0].Function.Arguments
	var parsed map[string]any
	if err := json.Unmarshal([]byte(rebuilt), &parsed); err != nil {
		t.Fatalf("arguments are no longer JSON: %q", rebuilt)
	}
	// The CPF sent as a JSON number is a declared limitation and stays as is.
	if !strings.Contains(rebuilt, `"n": `+cpf) || strings.Contains(rebuilt, "doc "+cpf) || strings.Contains(rebuilt, "ana@") {
		t.Fatalf("arguments = %s", rebuilt)
	}
	if !strings.HasPrefix(rebuilt, `{"email": "[EMAIL_1_`) || !strings.Contains(rebuilt, `true, null]}`) {
		t.Fatalf("formatting outside the strings changed: %s", rebuilt)
	}
	if text := sent.Messages[1].FunctionCall.Arguments; strings.Contains(text, "ana@") || !strings.HasPrefix(text, "free text [EMAIL_1_") {
		t.Fatalf("plain arguments = %q", text)
	}
}

func TestUnicodeEscapedMatches(t *testing.T) {
	recorder := &sensitiveRecorder{}
	fx, upstream := sensitiveFixture(t, sensitiveSetup{settings: detectionSettings("email: redact"), recorder: recorder})
	body := `{"model":"fake-claude","max_tokens":10,"messages":[{"role":"user","content":"para ana@example.org é \"isso\""}]}`
	if response, answer := fx.post("/v1/messages", "anthropic", body); response.StatusCode != 200 {
		t.Fatalf("status %d: %s", response.StatusCode, answer)
	}
	sent := upstreamBody(t, upstream)
	if strings.Contains(sent, `ana@`) || !strings.Contains(sent, `"para [EMAIL_1_`) || !strings.Contains(sent, `é \"isso\""`) {
		t.Fatalf("sent = %s", sent)
	}
}

func TestControlsAndBinaryUntouched(t *testing.T) {
	cpf := testkit.DocumentID()
	fx, upstream := sensitiveFixture(t, sensitiveSetup{settings: detectionSettings("cpf: redact", "email: redact"), recorder: &sensitiveRecorder{}})
	body := `{"model":"fake-claude","max_tokens":10,"metadata":{"user_id":"u-1"},"messages":[{"role":"user","content":[` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + cpf + `ana@example.org"}},` +
		`{"type":"document","source":{"type":"text","media_type":"text/plain","data":"` + cpf + `"}},` +
		`{"type":"tool_use","id":"toolu_` + cpf + `","name":"n` + cpf + `","input":{}},` +
		`{"type":"text","text":"ok"}]}],"future":{"note":"` + cpf + `"}}`
	if response, answer := fx.post("/v1/messages", "anthropic", body); response.StatusCode != 200 {
		t.Fatalf("status %d: %s", response.StatusCode, answer)
	}
	sent := upstreamBody(t, upstream)
	if strings.Count(sent, cpf) != 5 || !strings.Contains(sent, "ana@example.org") || tokenPattern.MatchString(sent) {
		t.Fatalf("controls or binary changed: %s", sent)
	}
}

func TestDetectPreservesBytes(t *testing.T) {
	body := `{"model":"fake-claude","max_tokens":10,"metadata":{"user_id":"u-1"},"messages":[{"role":"user","content":"para ana@example.org  e ` + testkit.DocumentID() + `"}],"x":1.50}`
	off, offUpstream := sensitiveFixture(t, sensitiveSetup{settings: detectionSettings("email: off")})
	if response, _ := off.post("/v1/messages", "anthropic", body); response.StatusCode != 200 {
		t.Fatal("off request failed")
	}
	recorder := &sensitiveRecorder{}
	detect, detectUpstream := sensitiveFixture(t, sensitiveSetup{settings: detectionSettings("email: detect", "cpf: detect"), recorder: recorder})
	if response, _ := detect.post("/v1/messages", "anthropic", body); response.StatusCode != 200 {
		t.Fatal("detect request failed")
	}
	// Each fixture has its own HMAC key, so only the pseudonym may differ.
	pseudonym := regexp.MustCompile(`"user_id":"[0-9a-f]+"`)
	a := pseudonym.ReplaceAllString(upstreamBody(t, offUpstream), "")
	b := pseudonym.ReplaceAllString(upstreamBody(t, detectUpstream), "")
	if a != b {
		t.Fatalf("detect changed bytes:\n%s\n%s", a, b)
	}
	if len(recorder.published) != 1 || len(recorder.published[0]) != 2 {
		t.Fatalf("published = %+v", recorder.published)
	}
	result := detect.sink.wait(t)
	if len(result.Detections) != 2 || result.Detections[0].DataType != sensitivedata.CPF {
		t.Fatalf("result detections = %+v", result.Detections)
	}
}

func TestCountTokensIsScanned(t *testing.T) {
	recorder := &sensitiveRecorder{}
	fx, upstream := sensitiveFixture(t, sensitiveSetup{settings: detectionSettings("secret: block"), recorder: recorder})
	body := `{"model":"fake-claude","messages":[{"role":"user","content":"chave ` + syntheticAnthropicKey() + `"}]}`
	response, answer := fx.post("/v1/messages/count_tokens", "anthropic", body)
	if response.StatusCode != 400 || comportaCode(t, answer) != "sensitive_data_blocked" || upstream.count() != 0 {
		t.Fatalf("status %d upstream %d: %s", response.StatusCode, upstream.count(), answer)
	}
	if strings.Contains(answer, "Zq7_") || len(recorder.cuts) != 1 || recorder.cuts[0].Summary[0].DataType != sensitivedata.Secret {
		t.Fatalf("cut = %+v answer = %s", recorder.cuts, answer)
	}
}
