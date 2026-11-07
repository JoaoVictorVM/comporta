package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/keys"
)

func requestFrom(remote string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/test", nil)
	r.RemoteAddr = remote
	return r
}

type captured struct {
	identity keys.Identity
	snapshot *config.Snapshot
	calls    int
}

type httpFixture struct {
	*fixture
	adapter *HTTP
	next    *captured
	clock   *fakeClock
}

func newHTTPFixture(t *testing.T, f *fixture, source KeySource) *httpFixture {
	t.Helper()
	clock := newFakeClock()
	return &httpFixture{
		fixture: f,
		adapter: NewHTTP(source, NewAuthenticator(source, clock), NewGuard(clock, nil)),
		next:    &captured{},
		clock:   clock,
	}
}

func (h *httpFixture) serve(format FormatSelector, r *http.Request) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	h.adapter.Protect(format, h.config, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		h.next.calls++
		h.next.identity, _ = IdentityFrom(r.Context())
		h.next.snapshot, _ = SnapshotFrom(r.Context())
	})).ServeHTTP(recorder, r)
	return recorder
}

func withAPIKey(r *http.Request, value string) *http.Request {
	r.Header.Set("x-api-key", value)
	return r
}

func withBearer(r *http.Request, value string) *http.Request {
	r.Header.Set("Authorization", "Bearer "+value)
	return r
}

func TestAuthenticatedIdentityReachesNextHandler(t *testing.T) {
	f := newFixture(t)
	created := f.create(t, "bot")
	h := newHTTPFixture(t, f, f.keyManager(t))

	r := withAPIKey(requestFrom("192.0.2.1:1234"), created.Key.Reveal())
	r.Header.Set("X-Comporta-App", "other")
	r.Header.Set("X-App-Name", "other")
	response := h.serve(Anthropic, r)
	if response.Code != http.StatusOK || h.next.calls != 1 {
		t.Fatalf("status %d, calls %d", response.Code, h.next.calls)
	}
	if h.next.identity.AppName() != "bot" || h.next.identity.KeyID() != created.Metadata.ID {
		t.Fatalf("the identity must come from the key: %+v", h.next.identity)
	}
	if h.next.snapshot == nil || h.next.snapshot.Generation() != f.config.Capture().Generation() {
		t.Fatal("the handler must receive the snapshot used to authenticate")
	}
	if _, ok := IdentityFrom(context.Background()); ok {
		t.Fatal("no identity without authentication")
	}
	data, err := json.Marshal(h.next.identity)
	if err != nil || strings.Contains(string(data), created.Key.Reveal()) {
		t.Fatalf("projection %s %v", data, err)
	}

	bearer := withBearer(requestFrom("192.0.2.1:1234"), created.Key.Reveal())
	if response := h.serve(OpenAI, bearer); response.Code != http.StatusOK {
		t.Fatalf("bearer status %d", response.Code)
	}
	lower := requestFrom("192.0.2.1:1234")
	lower.Header.Set("Authorization", "bEaReR "+created.Key.Reveal())
	if response := h.serve(OpenAI, lower); response.Code != http.StatusOK {
		t.Fatalf("the scheme ignores case, status %d", response.Code)
	}
}

func decodeError(t *testing.T, response *httptest.ResponseRecorder) (root map[string]any, inner map[string]any) {
	t.Helper()
	if got := response.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type %q", got)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("cache control %q", got)
	}
	if err := json.Unmarshal(response.Body.Bytes(), &root); err != nil {
		t.Fatal(err)
	}
	inner, ok := root["error"].(map[string]any)
	if !ok {
		t.Fatalf("body %s", response.Body)
	}
	return root, inner
}

func TestAnthropicAndOpenAIAuthenticationErrors(t *testing.T) {
	f := newFixture(t)
	created := f.create(t, "bot")
	if _, err := f.store.Revoke(context.Background(), created.Metadata.ID); err != nil {
		t.Fatal(err)
	}
	h := newHTTPFixture(t, f, f.keyManager(t))
	invalid := "cmp_" + strings.Repeat("0", 43)

	cases := []struct {
		name    string
		format  FormatSelector
		request *http.Request
		message string
		code    string
	}{
		{"anthropic invalid", Anthropic, withAPIKey(requestFrom("192.0.2.1:1"), invalid), "invalid Comporta API key", "invalid_api_key"},
		{"anthropic missing", Anthropic, requestFrom("192.0.2.1:1"), "invalid Comporta API key", "invalid_api_key"},
		{"anthropic revoked", Anthropic, withAPIKey(requestFrom("192.0.2.1:1"), created.Key.Reveal()), "this Comporta API key was revoked", "key_revoked"},
		{"openai invalid", OpenAI, withBearer(requestFrom("192.0.2.1:1"), invalid), "invalid Comporta API key", "invalid_api_key"},
		{"openai syntax", OpenAI, withBearer(requestFrom("192.0.2.1:1"), "nonsense"), "invalid Comporta API key", "invalid_api_key"},
		{"openai revoked", OpenAI, withBearer(requestFrom("192.0.2.1:1"), created.Key.Reveal()), "this Comporta API key was revoked", "key_revoked"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := h.serve(tc.format, tc.request)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status %d", response.Code)
			}
			root, inner := decodeError(t, response)
			if inner["type"] != "authentication_error" || inner["message"] != tc.message || inner["comporta_code"] != tc.code {
				t.Fatalf("body %s", response.Body)
			}
			if _, anthropic := root["type"]; anthropic != (strings.HasPrefix(tc.name, "anthropic")) {
				t.Fatalf("wrong envelope: %s", response.Body)
			}
		})
	}
	if h.next.calls != 0 {
		t.Fatal("next must not run")
	}
}

func TestExactErrorBodies(t *testing.T) {
	f := newFixture(t)
	created := f.create(t, "bot")
	if _, err := f.store.Revoke(context.Background(), created.Metadata.ID); err != nil {
		t.Fatal(err)
	}
	h := newHTTPFixture(t, f, f.keyManager(t))
	got := h.serve(Anthropic, withAPIKey(requestFrom("192.0.2.1:1"), "x")).Body.String()
	want := `{"type":"error","error":{"type":"authentication_error","message":"invalid Comporta API key","comporta_code":"invalid_api_key"}}`
	if got != want {
		t.Fatalf("got %s", got)
	}
	got = h.serve(OpenAI, withBearer(requestFrom("192.0.2.1:1"), created.Key.Reveal())).Body.String()
	want = `{"error":{"type":"authentication_error","message":"this Comporta API key was revoked","comporta_code":"key_revoked"}}`
	if got != want {
		t.Fatalf("got %s", got)
	}
}

func TestAmbiguousCredentialsRejected(t *testing.T) {
	f := newFixture(t)
	key := f.create(t, "bot").Key.Reveal()
	h := newHTTPFixture(t, f, f.keyManager(t))
	cases := map[string]func(*http.Request){
		"two methods": func(r *http.Request) {
			r.Header.Set("x-api-key", key)
			r.Header.Set("Authorization", "Bearer "+key)
		},
		"repeated api key": func(r *http.Request) {
			r.Header.Add("x-api-key", key)
			r.Header.Add("x-api-key", key)
		},
		"repeated authorization": func(r *http.Request) {
			r.Header.Add("Authorization", "Bearer "+key)
			r.Header.Add("Authorization", "Bearer "+key)
		},
		"empty api key":     func(r *http.Request) { r.Header.Set("x-api-key", "") },
		"list":              func(r *http.Request) { r.Header.Set("x-api-key", key+","+key) },
		"untrimmed":         func(r *http.Request) { r.Header.Set("x-api-key", " "+key) },
		"basic scheme":      func(r *http.Request) { r.Header.Set("Authorization", "Basic "+key) },
		"no scheme":         func(r *http.Request) { r.Header.Set("Authorization", key) },
		"two tokens":        func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+key+" "+key) },
		"double space":      func(r *http.Request) { r.Header.Set("Authorization", "Bearer  "+key) },
		"empty bearer":      func(r *http.Request) { r.Header.Set("Authorization", "Bearer ") },
		"oversized api key": func(r *http.Request) { r.Header.Set("x-api-key", strings.Repeat("a", 257)) },
		"oversized bearer":  func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+strings.Repeat("a", 257)) },
		"cookie only":       func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "x-api-key", Value: key}) },
		"query string only": func(r *http.Request) { r.URL.RawQuery = "api_key=" + key + "&x-api-key=" + key },
	}
	octet := 0
	for name, prepare := range cases {
		for format, selector := range map[string]FormatSelector{"anthropic": Anthropic, "openai": OpenAI} {
			t.Run(name+"/"+format, func(t *testing.T) {
				octet++
				r := requestFrom("192.0.2." + strconv.Itoa(octet) + ":1")
				prepare(r)
				response := h.serve(selector, r)
				if response.Code != http.StatusUnauthorized {
					t.Fatalf("status %d", response.Code)
				}
				if _, inner := decodeError(t, response); inner["comporta_code"] != "invalid_api_key" {
					t.Fatalf("body %s", response.Body)
				}
			})
		}
	}
	if h.next.calls != 0 {
		t.Fatal("next must not run")
	}
}

func TestCredentialMustMatchTheRouteFormat(t *testing.T) {
	f := newFixture(t)
	key := f.create(t, "bot").Key.Reveal()
	h := newHTTPFixture(t, f, f.keyManager(t))
	if response := h.serve(Anthropic, withBearer(requestFrom("192.0.2.1:1"), key)); response.Code != http.StatusOK {
		t.Fatalf("bearer on an Anthropic route, as recent SDKs send it: %d", response.Code)
	}
	if response := h.serve(Anthropic, withAPIKey(requestFrom("192.0.2.1:1"), key)); response.Code != http.StatusOK {
		t.Fatalf("x-api-key on an Anthropic route: %d", response.Code)
	}
	if response := h.serve(OpenAI, withAPIKey(requestFrom("192.0.2.1:1"), key)); response.Code != http.StatusUnauthorized {
		t.Fatalf("x-api-key on an OpenAI route: %d", response.Code)
	}
	if response := h.serve(ModelsFormat, withAPIKey(requestFrom("192.0.2.1:1"), key)); response.Code != http.StatusOK {
		t.Fatalf("x-api-key on the models route: %d", response.Code)
	}
	if response := h.serve(ModelsFormat, withBearer(requestFrom("192.0.2.1:1"), key)); response.Code != http.StatusOK {
		t.Fatalf("bearer on the models route: %d", response.Code)
	}
}

func TestModelsFormatSelection(t *testing.T) {
	withKey := requestFrom("192.0.2.1:1")
	withKey.Header.Set("x-api-key", "anything")
	if ModelsFormat(withKey) != FormatAnthropic {
		t.Fatal("x-api-key selects Anthropic")
	}
	if ModelsFormat(requestFrom("192.0.2.1:1")) != FormatOpenAI {
		t.Fatal("no credential selects OpenAI")
	}
	bearer := withBearer(requestFrom("192.0.2.1:1"), "anything")
	if ModelsFormat(bearer) != FormatOpenAI {
		t.Fatal("bearer selects OpenAI")
	}
	both := withBearer(requestFrom("192.0.2.1:1"), "anything")
	both.Header.Set("x-api-key", "anything")
	if ModelsFormat(both) != FormatAnthropic {
		t.Fatal("conflicts are rejected later, in the Anthropic format")
	}
	versioned := withBearer(requestFrom("192.0.2.1:1"), "anything")
	versioned.Header.Set("Anthropic-Version", "2023-06-01")
	if ModelsFormat(versioned) != FormatAnthropic {
		t.Fatal("an Anthropic SDK sending a bearer token selects Anthropic")
	}
}

func TestCredentialNeverAppearsInErrorOrLog(t *testing.T) {
	f := newFixture(t)
	created := f.create(t, "bot")
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	source := f.keyManager(t)
	clock := newFakeClock()
	adapter := NewHTTP(source, NewAuthenticator(source, clock), NewGuard(clock, logger))

	canary := "cmp_" + strings.Repeat("Q", 43)
	var bodies strings.Builder
	var headers strings.Builder
	for range 25 {
		recorder := httptest.NewRecorder()
		adapter.Protect(Anthropic, f.config, http.NotFoundHandler()).
			ServeHTTP(recorder, withAPIKey(requestFrom("192.0.2.1:1"), canary))
		bodies.WriteString(recorder.Body.String())
		for name, values := range recorder.Header() {
			headers.WriteString(name + strings.Join(values, ""))
		}
	}
	clock.advance(guardSweepEvery + guardCooldown)
	adapter.guard.Admit(testAddr)

	sum := sha256.Sum256([]byte(canary))
	canaryDigest := hex.EncodeToString(sum[:])
	for _, text := range []string{bodies.String(), headers.String(), logs.String()} {
		if strings.Contains(text, canary) || strings.Contains(text, created.Key.Reveal()) || strings.Contains(text, canaryDigest) {
			t.Fatalf("credential exposed: %s", text)
		}
		if strings.Contains(text, "192.0.2.1") {
			t.Fatalf("address exposed: %s", text)
		}
	}
}

func TestForwardedHeadersCannotChangeIP(t *testing.T) {
	f := newFixture(t)
	f.create(t, "bot")
	h := newHTTPFixture(t, f, f.keyManager(t))
	for i := range 20 {
		r := withAPIKey(requestFrom("192.0.2.1:"+strconv.Itoa(1000+i)), "wrong")
		r.Header.Set("X-Forwarded-For", "198.51.100."+strconv.Itoa(i))
		r.Header.Set("X-Real-IP", "198.51.100."+strconv.Itoa(i))
		r.Header.Set("Forwarded", "for=198.51.100."+strconv.Itoa(i))
		if response := h.serve(Anthropic, r); response.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: status %d", i+1, response.Code)
		}
	}
	r := withAPIKey(requestFrom("192.0.2.1:9999"), "wrong")
	r.Header.Set("X-Forwarded-For", "203.0.113.5")
	if response := h.serve(Anthropic, r); response.Code != http.StatusTooManyRequests {
		t.Fatalf("spoofed headers must not escape the block: %d", response.Code)
	}
}

func TestBlockedAddressGetsRateLimitEvenWithValidKey(t *testing.T) {
	f := newFixture(t)
	created := f.create(t, "bot")
	h := newHTTPFixture(t, f, f.keyManager(t))
	for range 20 {
		if response := h.serve(OpenAI, withBearer(requestFrom("192.0.2.1:1"), "wrong")); response.Code != http.StatusUnauthorized {
			t.Fatalf("status %d", response.Code)
		}
	}
	for _, format := range []FormatSelector{Anthropic, OpenAI} {
		r := withAPIKey(requestFrom("192.0.2.1:1"), created.Key.Reveal())
		if format(r) == FormatOpenAI {
			r = withBearer(requestFrom("192.0.2.1:1"), created.Key.Reveal())
		}
		response := h.serve(format, r)
		if response.Code != http.StatusTooManyRequests {
			t.Fatalf("status %d", response.Code)
		}
		if got := response.Header().Get("Retry-After"); got != "300" {
			t.Fatalf("retry after %q", got)
		}
		_, inner := decodeError(t, response)
		if inner["type"] != "rate_limit_error" || inner["comporta_code"] != "auth_rate_limited" || inner["message"] != "too many authentication attempts" {
			t.Fatalf("body %s", response.Body)
		}
	}
	if h.next.calls != 0 {
		t.Fatal("a valid key must not bypass the block")
	}
	h.clock.advance(guardCooldown)
	r := withAPIKey(requestFrom("192.0.2.1:1"), created.Key.Reveal())
	if response := h.serve(Anthropic, r); response.Code != http.StatusOK {
		t.Fatalf("after the cooldown: status %d", response.Code)
	}
}

type unavailableSource struct{}

func (unavailableSource) Capture() *keys.Generation   { return &keys.Generation{} }
func (unavailableSource) RecordUse(string, time.Time) {}

func TestUnavailableStoreRespondsWithoutCounting(t *testing.T) {
	f := newFixture(t)
	h := newHTTPFixture(t, f, unavailableSource{})
	for range 40 {
		response := h.serve(OpenAI, withBearer(requestFrom("192.0.2.1:1"), "wrong"))
		if response.Code != http.StatusServiceUnavailable {
			t.Fatalf("status %d", response.Code)
		}
		want := `{"error":{"type":"api_error","message":"authentication storage unavailable","comporta_code":"auth_unavailable"}}`
		if got := response.Body.String(); got != want {
			t.Fatalf("got %s", got)
		}
	}
	if len(h.adapter.guard.entries) != 0 {
		t.Fatal("an outage is not a failed credential")
	}
	if h.next.calls != 0 {
		t.Fatal("next must not run")
	}
}

func TestInvalidRemoteAddressIsRejected(t *testing.T) {
	f := newFixture(t)
	key := f.create(t, "bot").Key.Reveal()
	h := newHTTPFixture(t, f, f.keyManager(t))
	for _, remote := range []string{"", "not-an-address", "192.0.2.1", "unix:@"} {
		response := h.serve(OpenAI, withBearer(requestFrom(remote), key))
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%q: status %d", remote, response.Code)
		}
		_, inner := decodeError(t, response)
		if inner["type"] != "invalid_request_error" || inner["comporta_code"] != "invalid_client_address" {
			t.Fatalf("body %s", response.Body)
		}
		if remote != "" && strings.Contains(response.Body.String(), remote) {
			t.Fatal("the address must not be echoed")
		}
	}
	if len(h.adapter.guard.entries) != 0 {
		t.Fatal("no shared identity may be invented")
	}
}

func TestRevocationDoesNotAffectAcceptedOperation(t *testing.T) {
	f := newFixture(t)
	created := f.create(t, "bot")
	manager := f.keyManager(t)
	h := newHTTPFixture(t, f, manager)

	var identity keys.Identity
	recorder := httptest.NewRecorder()
	h.adapter.Protect(Anthropic, f.config, http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if _, err := f.store.Revoke(r.Context(), created.Metadata.ID); err != nil {
			t.Error(err)
		}
		identity, _ = IdentityFrom(r.Context())
	})).ServeHTTP(recorder, withAPIKey(requestFrom("192.0.2.1:1"), created.Key.Reveal()))
	if identity.KeyID() != created.Metadata.ID {
		t.Fatalf("the accepted identity must stay fixed: %+v", identity)
	}
}
