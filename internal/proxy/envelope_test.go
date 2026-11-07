package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/keys"
)

const rawUserCanary = "raw-user-canary"

func testIdentity(t *testing.T) keys.Identity {
	t.Helper()
	dir := t.TempDir()
	if _, err := keys.NewStore(dir).Create(context.Background(), "bot"); err != nil {
		t.Fatal(err)
	}
	manager, err := keys.NewManager(keys.ManagerOptions{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	for entry := range manager.Capture().Entries() {
		return entry.Identity
	}
	t.Fatal("no entry")
	return keys.Identity{}
}

func parseFor(t *testing.T, route Route, body string, headers ...string) (*prepared, *Failure) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, route.path(), strings.NewReader(body))
	for i := 0; i+1 < len(headers); i += 2 {
		request.Header.Add(headers[i], headers[i+1])
	}
	return parseRequest(route, request, []byte(body), testIdentity(t), 7)
}

func mustParse(t *testing.T, route Route, body string, headers ...string) *prepared {
	t.Helper()
	result, failure := parseFor(t, route, body, headers...)
	if failure != nil {
		t.Fatalf("rejected: %+v", failure)
	}
	return result
}

func TestEnvelopeUsesAuthIdentity(t *testing.T) {
	result := mustParse(t, RouteMessages, `{"model":"m","max_tokens":3,"messages":[],"app_name":"other","key_id":"spoofed"}`,
		"X-Comporta-App", "other")
	envelope := result.envelope
	if envelope.AppName != "bot" || !strings.HasPrefix(envelope.KeyID, "k_") {
		t.Fatalf("identity taken from the client: %+v", envelope.public())
	}
	if envelope.ConfigGeneration != 7 || envelope.Endpoint != "/v1/messages" || envelope.Operation != OperationGeneration {
		t.Fatalf("metadata: %+v", envelope.public())
	}
	if len(envelope.RequestID) != 32 {
		t.Fatalf("request id %q", envelope.RequestID)
	}
	other := mustParse(t, RouteMessages, `{"model":"m","max_tokens":3,"messages":[]}`)
	if other.envelope.RequestID == envelope.RequestID {
		t.Fatal("request ids repeat")
	}
}

func TestOptionalValuesKeepPresence(t *testing.T) {
	absent := mustParse(t, RouteChatCompletions, `{"model":"m","messages":[]}`).envelope
	if absent.MaxTokens.Known || absent.Temperature.Known || absent.OutputLimitField != "" || absent.Stream {
		t.Fatalf("absent fields look present: %+v", absent)
	}
	zero := mustParse(t, RouteChatCompletions, `{"model":"m","messages":[],"temperature":0.0,"max_completion_tokens":5}`).envelope
	if !zero.Temperature.Known || zero.Temperature.Value != "0.0" {
		t.Fatalf("temperature %+v", zero.Temperature)
	}
	if !zero.MaxTokens.Known || zero.MaxTokens.Value != 5 || zero.OutputLimitField != "max_completion_tokens" {
		t.Fatalf("limit %+v %s", zero.MaxTokens, zero.OutputLimitField)
	}
	nulls := mustParse(t, RouteChatCompletions, `{"model":"m","messages":[],"temperature":null,"max_tokens":null,"stream":null}`).envelope
	if nulls.Temperature.Known || nulls.MaxTokens.Known || nulls.Stream {
		t.Fatalf("nulls must read as absent on the OpenAI format: %+v", nulls)
	}
}

func TestRequestFieldValidation(t *testing.T) {
	cases := map[string]struct {
		route Route
		body  string
	}{
		"missing model":        {RouteMessages, `{"max_tokens":1,"messages":[]}`},
		"empty model":          {RouteMessages, `{"model":"","max_tokens":1,"messages":[]}`},
		"model not string":     {RouteMessages, `{"model":1,"max_tokens":1,"messages":[]}`},
		"messages not array":   {RouteMessages, `{"model":"m","max_tokens":1,"messages":{}}`},
		"max_tokens missing":   {RouteMessages, `{"model":"m","messages":[]}`},
		"max_tokens zero":      {RouteMessages, `{"model":"m","max_tokens":0,"messages":[]}`},
		"max_tokens float":     {RouteMessages, `{"model":"m","max_tokens":1.5,"messages":[]}`},
		"max_tokens overflow":  {RouteMessages, `{"model":"m","max_tokens":9223372036854775808,"messages":[]}`},
		"stream not bool":      {RouteMessages, `{"model":"m","max_tokens":1,"messages":[],"stream":"yes"}`},
		"anthropic null":       {RouteMessages, `{"model":"m","max_tokens":1,"messages":[],"stream":null}`},
		"infinite temperature": {RouteMessages, `{"model":"m","max_tokens":1,"messages":[],"temperature":1e999}`},
		"temperature string":   {RouteMessages, `{"model":"m","max_tokens":1,"messages":[],"temperature":"1"}`},
		"count stream":         {RouteCountTokens, `{"model":"m","messages":[],"stream":true}`},
		"both limits":          {RouteChatCompletions, `{"model":"m","messages":[],"max_tokens":1,"max_completion_tokens":1}`},
		"negative limit":       {RouteChatCompletions, `{"model":"m","messages":[],"max_completion_tokens":-1}`},
		"bad stream_options":   {RouteChatCompletions, `{"model":"m","messages":[],"stream":true,"stream_options":1}`},
		"bad include_usage":    {RouteChatCompletions, `{"model":"m","messages":[],"stream":true,"stream_options":{"include_usage":"x"}}`},
		"user not string":      {RouteChatCompletions, `{"model":"m","messages":[],"user":1}`},
		"metadata user number": {RouteMessages, `{"model":"m","max_tokens":1,"messages":[],"metadata":{"user_id":1}}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, failure := parseFor(t, tc.route, tc.body)
			if failure == nil || failure.Status != http.StatusBadRequest || failure.Code != CodeInvalidRequest {
				t.Fatalf("got %+v", failure)
			}
		})
	}
	count := mustParse(t, RouteCountTokens, `{"model":"m","messages":[],"system":"s"}`).envelope
	if count.Operation != OperationCountTokens || count.MaxTokens.Known {
		t.Fatalf("count_tokens: %+v", count)
	}
}

func TestUserSourcePrecedence(t *testing.T) {
	both := `{"model":"m","messages":[],"metadata":{"user_id":"meta"},"user":"field"}`
	cases := []struct {
		name    string
		body    string
		headers []string
		value   string
		source  UserSource
	}{
		{"header first", both, []string{"X-Comporta-User", "head"}, "head", UserSourceHeader},
		{"metadata second", both, nil, "meta", UserSourceMetadata},
		{"field last", `{"model":"m","messages":[],"user":"field"}`, nil, "field", UserSourceField},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			envelope := mustParse(t, RouteChatCompletions, tc.body, tc.headers...).envelope
			value, source, ok := envelope.RawUser()
			if !ok || value != tc.value || source != tc.source {
				t.Fatalf("got %q %q %v", value, source, ok)
			}
		})
	}
	if _, _, ok := mustParse(t, RouteChatCompletions, `{"model":"m","messages":[]}`).envelope.RawUser(); ok {
		t.Fatal("no user was sent")
	}
	if _, failure := parseFor(t, RouteChatCompletions, `{"model":"m","messages":[]}`, "X-Comporta-User", "a", "X-Comporta-User", "b"); failure == nil {
		t.Fatal("a repeated header must be refused")
	}
}

func TestEnvelopeRepresentationsHideRawUser(t *testing.T) {
	envelope := mustParse(t, RouteChatCompletions, `{"model":"m","messages":[],"user":"`+rawUserCanary+`"}`).envelope
	marshaled, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	var logged strings.Builder
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("x", "envelope", envelope, "value", *envelope)
	for name, text := range map[string]string{
		"json":   string(marshaled),
		"v":      fmt.Sprintf("%v", envelope),
		"plus":   fmt.Sprintf("%+v", *envelope),
		"gosyn":  fmt.Sprintf("%#v", *envelope),
		"string": fmt.Sprint(envelope),
		"slog":   logged.String(),
	} {
		if strings.Contains(text, rawUserCanary) {
			t.Errorf("%s exposes the user: %s", name, text)
		}
	}
}

func TestImageCountKnownBlocks(t *testing.T) {
	anthropic := mustParse(t, RouteMessages, `{"model":"m","max_tokens":1,"messages":[
		{"role":"user","content":[{"type":"image"},{"type":"text"},{"type":"image_url"},{"type":"image"}]},
		{"role":"user","content":"text"},
		{"role":"assistant","content":[{"type":"future"},1,{"no":"type"}]}]}`).envelope
	if anthropic.ImageCount != 2 {
		t.Fatalf("anthropic %d", anthropic.ImageCount)
	}
	openai := mustParse(t, RouteChatCompletions, `{"model":"m","image":{"type":"image_url"},"messages":[
		{"role":"user","content":[{"type":"image_url"},{"type":"input_image"},{"type":"image"}]}]}`).envelope
	if openai.ImageCount != 2 {
		t.Fatalf("openai %d", openai.ImageCount)
	}
}

func TestLastMessagesHashStableForGrowingContext(t *testing.T) {
	last := `{"role":"user","content":[{"type":"tool_result","content":"same","tool_use_id":"1"}]}`
	short := mustParse(t, RouteMessages, `{"model":"m","max_tokens":1,"messages":[`+last+`]}`).envelope
	grown := mustParse(t, RouteMessages, `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":"old"},{"role":"assistant","content":"reply"},`+last+`]}`).envelope
	if !short.LastMessagesHash.Known || short.LastMessagesHash != grown.LastMessagesHash {
		t.Fatal("older context changed the signature")
	}

	respelled := mustParse(t, RouteMessages, `{"model":"m","max_tokens":1,"messages":[ {"content":[{"tool_use_id":"1","content":"same","type":"tool_result"}], "role":"user"} ]}`).envelope
	if short.LastMessagesHash != respelled.LastMessagesHash {
		t.Fatal("key order changed the signature")
	}

	changed := mustParse(t, RouteMessages, `{"model":"m","max_tokens":1,"messages":[{"role":"user","content":[{"type":"tool_result","content":"different","tool_use_id":"1"}]}]}`).envelope
	if short.LastMessagesHash == changed.LastMessagesHash {
		t.Fatal("a different message kept the signature")
	}

	numbers := func(lexeme string) Optional[[32]byte] {
		return mustParse(t, RouteChatCompletions, `{"model":"m","messages":[{"role":"user","content":`+lexeme+`}]}`).envelope.LastMessagesHash
	}
	if numbers("1.0") == numbers("1") {
		t.Fatal("distinct number spellings must stay distinct")
	}

	empty := mustParse(t, RouteMessages, `{"model":"m","max_tokens":1,"messages":[{"role":"assistant","content":"x"}]}`).envelope
	if empty.LastMessagesHash.Known {
		t.Fatal("no user message and no tool result must produce no signature")
	}
	userOnly := mustParse(t, RouteChatCompletions, `{"model":"m","messages":[{"role":"user","content":"x"}]}`).envelope
	toolOnly := mustParse(t, RouteChatCompletions, `{"model":"m","messages":[{"role":"tool","content":"x"}]}`).envelope
	if userOnly.LastMessagesHash == toolOnly.LastMessagesHash {
		t.Fatal("absent parts must be distinguishable")
	}
}

func TestRequestHashFollowsEffectiveBody(t *testing.T) {
	plain := `{"model":"m","messages":[],"stream":true,"unknown":1}`
	result := mustParse(t, RouteChatCompletions, plain)
	if result.envelope.RequestHash != sha256.Sum256([]byte(plain)) {
		t.Fatal("the injected usage option must stay out of the request hash")
	}
	if result.envelope.BodySizeOriginal != int64(len(plain)) || result.envelope.BodySizeEffective <= result.envelope.BodySizeOriginal {
		t.Fatalf("sizes %d %d", result.envelope.BodySizeOriginal, result.envelope.BodySizeEffective)
	}
	different := mustParse(t, RouteChatCompletions, `{"model":"m","messages":[],"stream":true,"unknown":2}`)
	if result.envelope.RequestHash == different.envelope.RequestHash {
		t.Fatal("an unknown field must change the hash")
	}
	optedOut := mustParse(t, RouteChatCompletions, `{"model":"m","messages":[],"stream":true,"stream_options":{"include_usage":false}}`)
	optedIn := mustParse(t, RouteChatCompletions, `{"model":"m","messages":[],"stream":true,"stream_options":{"include_usage":true}}`)
	if optedOut.envelope.RequestHash == optedIn.envelope.RequestHash {
		t.Fatal("the client's own option must stay in the hash")
	}
}

func TestUsagePatch(t *testing.T) {
	cases := map[string]struct {
		body   string
		want   string
		asked  bool
		format Route
	}{
		"omitted": {
			`{"model":"m","messages":[],"stream":true}`,
			`{"model":"m","messages":[],"stream":true,"stream_options":{"include_usage":true}}`, false, RouteChatCompletions,
		},
		"preserves other options": {
			`{"model":"fake-gpt","messages":[{"role":"user","content":"Olá"}],"stream":true,"stream_options":{"future_option":1}}`,
			`{"model":"fake-gpt","messages":[{"role":"user","content":"Olá"}],"stream":true,"stream_options":{"future_option":1,"include_usage":true}}`, false, RouteChatCompletions,
		},
		"false": {
			`{"model":"m","messages":[],"stream":true,"stream_options":{"include_usage":false}}`,
			`{"model":"m","messages":[],"stream":true,"stream_options":{"include_usage":true}}`, false, RouteChatCompletions,
		},
		"null": {
			`{"model":"m","messages":[],"stream":true,"stream_options":null}`,
			`{"model":"m","messages":[],"stream":true,"stream_options":{"include_usage":true}}`, false, RouteChatCompletions,
		},
		"asked": {
			`{"model":"m","messages":[],"stream":true,"stream_options":{"include_usage":true}}`,
			`{"model":"m","messages":[],"stream":true,"stream_options":{"include_usage":true}}`, true, RouteChatCompletions,
		},
		"not streaming": {`{"model":"m","messages":[]}`, `{"model":"m","messages":[]}`, false, RouteChatCompletions},
		"anthropic": {
			`{"model":"m","max_tokens":1,"messages":[],"stream":true}`,
			`{"model":"m","max_tokens":1,"messages":[],"stream":true}`, false, RouteMessages,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			result := mustParse(t, tc.format, tc.body)
			if got := string(result.effectiveBody()); got != tc.want {
				t.Fatalf("got %s", got)
			}
			if result.envelope.ClientAskedUsage != tc.asked {
				t.Fatalf("asked %v", result.envelope.ClientAskedUsage)
			}
		})
	}
}
