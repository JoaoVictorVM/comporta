package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/useridentity"
)

const (
	messagesPath = "/v1/messages"
	chatPath     = "/v1/chat/completions"
	countPath    = "/v1/messages/count_tokens"
)

// lastBody returns what the provider received.
func (u *recordingUpstream) lastBody() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return string(u.body)
}

func hashOf(t *testing.T, fx *fixture, id string) string {
	t.Helper()
	hash, err := fx.identity.LookupHash("bot", id)
	if err != nil {
		t.Fatal(err)
	}
	return hash.Hex()
}

func field(t *testing.T, body string, path ...string) (any, bool) {
	t.Helper()
	var current any
	if err := json.Unmarshal([]byte(body), &current); err != nil {
		t.Fatalf("not JSON: %s", body)
	}
	for _, key := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = object[key]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func TestAllUserSourcePrecedence(t *testing.T) {
	upstream, server := startUpstream(t, nil)
	fx := newFixture(t, server.URL, fixtureOptions{})
	cases := []struct {
		name   string
		header string
		body   string
		want   string
		source UserSource
	}{
		{"header over both", "head", `"metadata":{"user_id":"meta"},"user":"field"`, "head", UserSourceHeader},
		{"header over metadata", "head", `"metadata":{"user_id":"meta"}`, "head", UserSourceHeader},
		{"header over field", "head", `"user":"field"`, "head", UserSourceHeader},
		{"header only", "head", "", "head", UserSourceHeader},
		{"metadata over field", "", `"metadata":{"user_id":"meta"},"user":"field"`, "meta", UserSourceMetadata},
		{"metadata only", "", `"metadata":{"user_id":"meta"}`, "meta", UserSourceMetadata},
		{"field only", "", `"user":"field"`, "field", UserSourceField},
	}
	routes := []struct {
		path, format, prefix string
	}{
		{messagesPath, "anthropic", `{"model":"fake-claude","max_tokens":1,"messages":[]`},
		{chatPath, "openai", `{"model":"fake-gpt","messages":[]`},
	}
	for _, tc := range cases {
		for _, route := range routes {
			t.Run(tc.name+" on "+route.format, func(t *testing.T) {
				body := route.prefix
				if tc.body != "" {
					body += "," + tc.body
				}
				request := fx.request(http.MethodPost, route.path, route.format, body+"}")
				if tc.header != "" {
					request.Header.Set("X-Comporta-User", tc.header)
				}
				if response, text := fx.do(request); response.StatusCode != http.StatusOK {
					t.Fatalf("status %d: %s", response.StatusCode, text)
				}
				sent := upstream.lastBody()
				want := hashOf(t, fx, tc.want)
				if route.format == "anthropic" || strings.Contains(tc.body, "metadata") {
					if got, _ := field(t, sent, "metadata", "user_id"); got != want {
						t.Fatalf("metadata.user_id %v, want %s: %s", got, want, sent)
					}
				}
				if route.format == "openai" || strings.Contains(tc.body, `"user"`) {
					if got, _ := field(t, sent, "user"); got != want {
						t.Fatalf("user %v, want %s: %s", got, want, sent)
					}
				}
				for _, raw := range []string{"head", "meta", "field"} {
					if strings.Contains(sent, `"`+raw+`"`) {
						t.Fatalf("%s reached the provider: %s", raw, sent)
					}
				}
			})
		}
	}
	mustParseSource := func(body string, headers ...string) UserSource {
		return mustParse(t, RouteChatCompletions, body, headers...).envelope.UserSource
	}
	if got := mustParseSource(`{"model":"m","messages":[],"user":"field"}`, "X-Comporta-User", "head"); got != UserSourceHeader {
		t.Fatalf("source %s", got)
	}
}

func TestPresentEmptyDoesNotFallBack(t *testing.T) {
	upstream, server := startUpstream(t, nil)
	fx := newFixture(t, server.URL, fixtureOptions{})
	cases := map[string]struct {
		header bool
		body   string
	}{
		"empty header with a valid body": {true, `{"model":"fake-gpt","messages":[],"user":"valid_id"}`},
		"empty metadata with a valid field": {false,
			`{"model":"fake-gpt","messages":[],"metadata":{"user_id":""},"user":"valid_id"}`},
		"empty field": {false, `{"model":"fake-gpt","messages":[],"user":""}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			request := fx.request(http.MethodPost, chatPath, "openai", tc.body)
			if tc.header {
				request.Header["X-Comporta-User"] = []string{""}
			}
			response, body := fx.do(request)
			if response.StatusCode != http.StatusBadRequest || comportaCode(t, body) != string(CodeUserInvalid) {
				t.Fatalf("status %d: %s", response.StatusCode, body)
			}
		})
	}
	if upstream.count() != 0 {
		t.Fatal("a rejected request reached the provider")
	}
}

func TestIdentityValidationErrors(t *testing.T) {
	upstream, server := startUpstream(t, nil)
	fx := newFixture(t, server.URL, fixtureOptions{})
	cases := []struct {
		id   string
		code Code
		text string
	}{
		{"joao@mail.com", CodeUserEmailNotAllowed, "user ID looks like an email address; send an opaque ID instead"},
		{"529.982.247-25", CodeUserDocumentNotAllowed, "user ID looks like a CPF or CNPJ; send an opaque ID instead"},
		{"11.222.333/0001-81", CodeUserDocumentNotAllowed, "user ID looks like a CPF or CNPJ; send an opaque ID instead"},
		{"12.ABC.345/01DE-35", CodeUserDocumentNotAllowed, "user ID looks like a CPF or CNPJ; send an opaque ID instead"},
		{"has space", CodeUserInvalid, "user ID must be 1-128 characters of [A-Za-z0-9_.:-]"},
	}
	routes := map[string]struct{ path, format, body string }{
		"anthropic": {messagesPath, "anthropic", `{"model":"fake-claude","max_tokens":1,"messages":[]}`},
		"openai":    {chatPath, "openai", `{"model":"fake-gpt","messages":[]}`},
		"count":     {countPath, "anthropic", `{"model":"fake-claude","messages":[]}`},
	}
	for _, tc := range cases {
		for name, route := range routes {
			t.Run(tc.id+" on "+name, func(t *testing.T) {
				request := fx.request(http.MethodPost, route.path, route.format, route.body)
				request.Header.Set("X-Comporta-User", tc.id)
				response, body := fx.do(request)
				if response.StatusCode != http.StatusBadRequest {
					t.Fatalf("status %d: %s", response.StatusCode, body)
				}
				var parsed struct {
					Error struct {
						Type    string `json:"type"`
						Message string `json:"message"`
						Code    Code   `json:"comporta_code"`
					} `json:"error"`
				}
				if err := json.Unmarshal([]byte(body), &parsed); err != nil {
					t.Fatal(err)
				}
				if parsed.Error.Code != tc.code || parsed.Error.Message != tc.text || parsed.Error.Type != "invalid_request_error" {
					t.Fatalf("error %+v", parsed.Error)
				}
				if strings.Contains(body, tc.id) {
					t.Fatalf("the response echoes the identifier: %s", body)
				}
				for header := range response.Header {
					if strings.Contains(strings.Join(response.Header[header], " "), tc.id) {
						t.Fatalf("header %s echoes the identifier", header)
					}
				}
			})
		}
	}
	if upstream.count() != 0 {
		t.Fatal("a rejected request reached the provider")
	}
	if results := fx.sink.all(); len(results) != 0 {
		t.Fatalf("a request that never reached the provider produced a result: %d", len(results))
	}
}

func TestShadowedIDsAreReplaced(t *testing.T) {
	upstream, server := startUpstream(t, nil)
	fx := newFixture(t, server.URL, fixtureOptions{})
	// A shadowed field is pseudonymized without judging it: only the winner is validated.
	body := `{"model":"fake-gpt","messages":[],"metadata":{"user_id":"shadow@mail.com","channel":"demo"},"user":"has space"}`
	request := fx.request(http.MethodPost, chatPath, "openai", body)
	request.Header.Set("X-Comporta-User", "winner")
	if response, text := fx.do(request); response.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", response.StatusCode, text)
	}
	want := hashOf(t, fx, "winner")
	expected := `{"model":"fake-gpt","messages":[],"metadata":{"user_id":"` + want + `","channel":"demo"},"user":"` + want + `"}`
	if got := upstream.lastBody(); got != expected {
		t.Fatalf("got %s", got)
	}
}

func TestProviderNativeFieldInjected(t *testing.T) {
	upstream, server := startUpstream(t, nil)
	fx := newFixture(t, server.URL, fixtureOptions{})
	hash := hashOf(t, fx, "head")
	cases := map[string]struct{ path, format, body, want string }{
		"messages without metadata": {messagesPath, "anthropic",
			`{"model":"fake-claude","max_tokens":1,"messages":[]}`,
			`{"model":"fake-claude","max_tokens":1,"messages":[],"metadata":{"user_id":"` + hash + `"}}`},
		"messages with empty metadata": {messagesPath, "anthropic",
			`{"model":"fake-claude","max_tokens":1,"metadata":{},"messages":[]}`,
			`{"model":"fake-claude","max_tokens":1,"metadata":{"user_id":"` + hash + `"},"messages":[]}`},
		"messages keeps other metadata bytes": {messagesPath, "anthropic",
			`{"model":"fake-claude","max_tokens":1,"metadata":{ "x" : 1.50 },"messages":[]}`,
			`{"model":"fake-claude","max_tokens":1,"metadata":{ "x" : 1.50,"user_id":"` + hash + `" },"messages":[]}`},
		"chat without user": {chatPath, "openai",
			`{"model":"fake-gpt","messages":[]}`,
			`{"model":"fake-gpt","messages":[],"user":"` + hash + `"}`},
		"chat leaves metadata alone": {chatPath, "openai",
			`{"model":"fake-gpt","messages":[],"metadata":"free text"}`,
			`{"model":"fake-gpt","messages":[],"metadata":"free text","user":"` + hash + `"}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			request := fx.request(http.MethodPost, tc.path, tc.format, tc.body)
			request.Header.Set("X-Comporta-User", "head")
			if response, text := fx.do(request); response.StatusCode != http.StatusOK {
				t.Fatalf("status %d: %s", response.StatusCode, text)
			}
			if got := upstream.lastBody(); got != tc.want {
				t.Fatalf("got  %s\nwant %s", got, tc.want)
			}
		})
	}
	t.Run("the header never goes upstream", func(t *testing.T) {
		upstream.mu.Lock()
		defer upstream.mu.Unlock()
		if upstream.header.Get("X-Comporta-User") != "" {
			t.Fatal("the identity header was forwarded")
		}
	})
	for _, metadata := range []string{`"text"`, `1`, `null`, `[]`, `true`} {
		t.Run("messages with metadata "+metadata, func(t *testing.T) {
			before := upstream.count()
			response, text := fx.post(messagesPath, "anthropic", `{"model":"fake-claude","max_tokens":1,"messages":[],"metadata":`+metadata+`}`)
			if response.StatusCode != http.StatusBadRequest || comportaCode(t, text) != string(CodeInvalidRequest) ||
				!strings.Contains(text, "metadata must be an object when setting user identity") {
				t.Fatalf("status %d: %s", response.StatusCode, text)
			}
			if upstream.count() != before {
				t.Fatal("the request reached the provider")
			}
		})
	}
}

func TestAnonymousProviderPseudonym(t *testing.T) {
	upstream, server := startUpstream(t, nil)
	fx := newFixture(t, server.URL, fixtureOptions{})
	anonymous := fx.identity.AnonymousHash("bot").Hex()
	if response, text := fx.post(chatPath, "openai", `{"model":"fake-gpt","messages":[]}`); response.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", response.StatusCode, text)
	}
	if got, _ := field(t, upstream.lastBody(), "user"); got != anonymous {
		t.Fatalf("user %v", got)
	}
	result := fx.sink.wait(t)
	if !result.Anonymous || result.UserHash.Hex() != anonymous {
		t.Fatalf("anonymous %v", result.Anonymous)
	}
	if hashOf(t, fx, "anonymous") == anonymous {
		t.Fatal("a user named anonymous merged with requests without a user")
	}
	if response, text := fx.post(messagesPath, "anthropic", `{"model":"fake-claude","max_tokens":1,"messages":[]}`); response.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", response.StatusCode, text)
	}
	if got, _ := field(t, upstream.lastBody(), "metadata", "user_id"); got != anonymous {
		t.Fatalf("metadata.user_id %v", got)
	}
}

func TestRequireUserRejectsAbsence(t *testing.T) {
	upstream, server := startUpstream(t, nil)
	fx := newFixture(t, server.URL, fixtureOptions{requireUser: true})
	cases := map[string]struct{ path, format, body string }{
		"anthropic": {messagesPath, "anthropic", `{"model":"fake-claude","max_tokens":1,"messages":[]}`},
		"openai":    {chatPath, "openai", `{"model":"fake-gpt","messages":[]}`},
		"count":     {countPath, "anthropic", `{"model":"fake-claude","messages":[]}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			response, text := fx.post(tc.path, tc.format, tc.body)
			if response.StatusCode != http.StatusBadRequest || comportaCode(t, text) != string(CodeUserRequired) ||
				!strings.Contains(text, "this app requires a user ID (X-Comporta-User header)") {
				t.Fatalf("status %d: %s", response.StatusCode, text)
			}
		})
	}
	if upstream.count() != 0 {
		t.Fatal("a rejected request reached the provider")
	}
	request := fx.request(http.MethodPost, chatPath, "openai", `{"model":"fake-gpt","messages":[]}`)
	request.Header.Set("X-Comporta-User", "user_123")
	if response, text := fx.do(request); response.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", response.StatusCode, text)
	}
}

func TestCountTokensIdentityWithoutInjection(t *testing.T) {
	upstream, server := startUpstream(t, nil)
	fx := newFixture(t, server.URL, fixtureOptions{})
	plain := `{"model":"fake-claude","messages":[],"system":"s"}`
	request := fx.request(http.MethodPost, countPath, "anthropic", plain)
	request.Header.Set("X-Comporta-User", "head")
	if response, text := fx.do(request); response.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", response.StatusCode, text)
	}
	if got := upstream.lastBody(); got != plain {
		t.Fatalf("a field was added: %s", got)
	}
	result := fx.sink.wait(t)
	if result.Anonymous || result.UserHash.Hex() != hashOf(t, fx, "head") {
		t.Fatal("the identity must still reach the result")
	}

	withField := `{"model":"fake-claude","messages":[],"metadata":{"user_id":"meta"},"user":"field"}`
	if response, text := fx.post(countPath, "anthropic", withField); response.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", response.StatusCode, text)
	}
	want := hashOf(t, fx, "meta")
	if got := upstream.lastBody(); got != `{"model":"fake-claude","messages":[],"metadata":{"user_id":"`+want+`"},"user":"`+want+`"}` {
		t.Fatalf("got %s", got)
	}

	// Nothing has to be built here, so a metadata of another type stays untouched.
	odd := `{"model":"fake-claude","messages":[],"metadata":"text"}`
	if response, text := fx.post(countPath, "anthropic", odd); response.StatusCode != http.StatusOK {
		t.Fatalf("status %d: %s", response.StatusCode, text)
	}
	if got := upstream.lastBody(); got != odd {
		t.Fatalf("got %s", got)
	}
}

func TestAppsGetDifferentPseudonymsForTheSameUser(t *testing.T) {
	service := newTestIdentityService(t)
	first, err := service.Resolve("bot", "user_123", true, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Resolve("other", "user_123", true, false)
	if err != nil {
		t.Fatal(err)
	}
	if first.Hash() == second.Hash() {
		t.Fatal("apps must not correlate")
	}
}

func newTestIdentityService(t testing.TB) *useridentity.Service {
	t.Helper()
	service, err := useridentity.Open(t.Context(), useridentity.Options{Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return service
}
