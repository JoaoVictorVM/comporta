package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

var policyPrompt = testkit.Prompt("Exemplo sintetico")

const policyConfig = `timezone: UTC
providers:
  anthropic:
    api_format: anthropic
    base_url: %[1]s
    api_key: ${COMPORTA_PROXY_TEST_KEY}
    models: [fake-claude, fake-haiku]
  openai:
    api_format: openai
    base_url: %[1]s
    api_key: ${COMPORTA_PROXY_TEST_KEY}
    models: [fake-gpt, fake-gpt-mini]
apps:
  bot:
    allowed_models: [fake-claude, fake-haiku, fake-gpt, fake-gpt-mini]
    on_limit: downgrade
    downgrade_to: TARGET
    budget: {usd_per_day: 1.00}
pricing:
  fake-claude: {input: 3, output: 15, cache_write: 3.75, cache_read: 0.3}
  fake-haiku: {input: 1, output: 5, cache_write: 1.25, cache_read: 0.1}
  fake-gpt: {input: 2, output: 8, cache_write: 2, cache_read: 0.5}
  fake-gpt-mini: {input: 0.4, output: 1.6, cache_write: 0.4, cache_read: 0.1}
`

// policyAccounting stands in for metering: it records what it was offered and
// authorizes the candidate the test chose.
type policyAccounting struct {
	mu         sync.Mutex
	selected   CandidateID
	policy     PolicyOutcome
	offers     [][]Candidate
	envelopes  []Envelope
	completion []AttemptResult
}

func (a *policyAccounting) Admit(context.Context, *config.Snapshot, *Envelope, string, config.Provider) (Admission, *Rejection) {
	panic("a policy-aware accounting must be offered candidates")
}

func (a *policyAccounting) AdmitCandidates(_ context.Context, _ *config.Snapshot, e *Envelope, offer []Candidate) (Admission, *Rejection) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.offers = append(a.offers, offer)
	a.envelopes = append(a.envelopes, *e)
	if a.selected == CandidateDowngrade {
		if len(offer) < 2 {
			return Admission{}, asRejection(internalFailure())
		}
		if offer[1].Unavailable != nil {
			return Admission{}, offer[1].Unavailable
		}
	}
	complete := func(r AttemptResult) {
		a.mu.Lock()
		defer a.mu.Unlock()
		a.completion = append(a.completion, r)
	}
	return Admission{Complete: complete, Selected: a.selected, Policy: a.policy}, nil
}

func (a *policyAccounting) lastOffer() []Candidate {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.offers[len(a.offers)-1]
}

func policyFixture(t *testing.T, target string, accounting *policyAccounting, reply http.HandlerFunc) (*fixture, *recordingUpstream) {
	t.Helper()
	upstream, server := startUpstream(t, reply)
	fx := newFixture(t, server.URL, fixtureOptions{accounting: accounting, config: strings.Replace(policyConfig, "TARGET", target, 1)})
	return fx, upstream
}

func downgrading() *policyAccounting {
	return &policyAccounting{selected: CandidateDowngrade, policy: PolicyDowngraded}
}

func decode(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()
	var out map[string]json.RawMessage
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("%s: %v", body, err)
	}
	return out
}

func TestDowngradePatchesOnlyModelAndDocumentedFields(t *testing.T) {
	accounting := downgrading()
	fx, upstream := policyFixture(t, "fake-haiku", accounting, nil)
	body := `{"model":"fake-claude","max_tokens":9000,"messages":[{"role":"user","content":"` + policyPrompt + `"}],"metadata":{"user_id":"cliente-1"},"future":{"nested":[1,2]},"temperature":0.5}`
	response, answer := fx.post("/v1/messages", "anthropic", body)
	if response.StatusCode != 200 || response.Header.Get(downgradedFromHeader) != "fake-claude" {
		t.Fatalf("status %d header %q: %s", response.StatusCode, response.Header.Get(downgradedFromHeader), answer)
	}
	sent := decode(t, upstream.body)
	if string(sent["model"]) != `"fake-haiku"` || string(sent["max_tokens"]) != "4096" {
		t.Fatalf("target body: %s", upstream.body)
	}
	asked := decode(t, []byte(body))
	for _, key := range []string{"messages", "future", "temperature"} {
		if string(sent[key]) != string(asked[key]) {
			t.Fatalf("%s changed: %s", key, sent[key])
		}
	}
	if strings.Contains(string(upstream.body), "cliente-1") {
		t.Fatal("the target lost the identity patch")
	}
	result := fx.sink.wait(t)
	if result.ModelRequested != "fake-claude" || result.ModelUsed != "fake-haiku" || result.Provider != "anthropic" {
		t.Fatalf("result: %+v", result)
	}
}

func TestSelectedBodyMatchesQuote(t *testing.T) {
	accounting := downgrading()
	fx, upstream := policyFixture(t, "fake-haiku", accounting, nil)
	body := `{"model":"fake-claude","max_tokens":10,"messages":[{"role":"user","content":"` + policyPrompt + `"}]}`
	if response, answer := fx.post("/v1/messages", "anthropic", body); response.StatusCode != 200 {
		t.Fatalf("status %d: %s", response.StatusCode, answer)
	}
	offer := accounting.lastOffer()
	if len(offer) != 2 || offer[1].Model != "fake-haiku" || offer[1].ProviderName != "anthropic" || offer[1].Unavailable != nil {
		t.Fatalf("offer: %+v", offer)
	}
	// The target is priced on the bytes that are actually sent.
	if offer[1].BodySizeEffective != int64(len(upstream.body)) || offer[0].BodySizeEffective-offer[1].BodySizeEffective != int64(len("fake-claude")-len("fake-haiku")) {
		t.Fatalf("quoted %d/%d bytes, sent %d", offer[0].BodySizeEffective, offer[1].BodySizeEffective, len(upstream.body))
	}
	if accounting.envelopes[0].ModelEffective != "fake-claude" {
		t.Fatal("accounting saw the target as the original")
	}
}

func TestMixedFormatDowngradeRejectedWithoutSend(t *testing.T) {
	accounting := downgrading()
	fx, upstream := policyFixture(t, "fake-haiku", accounting, nil)
	body := `{"model":"fake-gpt","messages":[{"role":"user","content":"` + policyPrompt + `"}]}`
	response, answer := fx.post("/v1/chat/completions", "openai", body)
	if response.StatusCode != 400 || comportaCode(t, answer) != string(CodeProviderFormatMismatch) || upstream.count() != 0 {
		t.Fatalf("status %d upstream %d: %s", response.StatusCode, upstream.count(), answer)
	}
	if offer := accounting.lastOffer(); len(offer) != 2 || offer[1].Unavailable == nil {
		t.Fatalf("the mixed-format target was offered as sendable: %+v", offer)
	}

	// Without the policy needing it, the original still goes out.
	accounting.selected, accounting.policy = CandidateOriginal, PolicyNone
	if response, answer := fx.post("/v1/chat/completions", "openai", body); response.StatusCode != 200 || upstream.count() != 1 {
		t.Fatalf("status %d: %s", response.StatusCode, answer)
	}
}

func TestDowngradeHeaderBeforeSSE(t *testing.T) {
	accounting := downgrading()
	fx, _ := policyFixture(t, "fake-gpt-mini", accounting, sseReply(deltaChunk+usageChunk+doneChunk))
	body := `{"model":"fake-gpt","stream":true,"messages":[{"role":"user","content":"` + policyPrompt + `"}]}`
	response, err := http.DefaultClient.Do(fx.request(http.MethodPost, "/v1/chat/completions", "openai", body))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	// The header arrives with the status line, before the first event is read.
	if response.Header.Get(downgradedFromHeader) != "fake-gpt" || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		t.Fatalf("headers: %v", response.Header)
	}
	first, err := bufio.NewReader(response.Body).ReadString('\n')
	if err != nil || !strings.HasPrefix(first, "data: ") {
		t.Fatalf("first event %q %v", first, err)
	}
}

func TestUpstreamCannotSpoofPolicyHeader(t *testing.T) {
	spoof := func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Comporta-Downgraded-From", "forged-model")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"usage":{"input_tokens":1,"output_tokens":1}}`))
	}
	body := `{"model":"fake-claude","max_tokens":10,"messages":[{"role":"user","content":"` + policyPrompt + `"}]}`
	original := &policyAccounting{selected: CandidateOriginal}
	fx, _ := policyFixture(t, "fake-haiku", original, spoof)
	if response, _ := fx.post("/v1/messages", "anthropic", body); response.Header.Values(downgradedFromHeader) != nil {
		t.Fatalf("upstream header reached the client: %v", response.Header)
	}
	fx, _ = policyFixture(t, "fake-haiku", downgrading(), spoof)
	if response, _ := fx.post("/v1/messages", "anthropic", body); strings.Join(response.Header.Values(downgradedFromHeader), ",") != "fake-claude" {
		t.Fatalf("local header replaced: %v", response.Header.Values(downgradedFromHeader))
	}
}

func TestRequestedTargetOmitsFromHeader(t *testing.T) {
	accounting := &policyAccounting{selected: CandidateOriginal, policy: PolicyDowngraded}
	fx, upstream := policyFixture(t, "fake-haiku", accounting, nil)
	body := `{"model":"fake-haiku","max_tokens":10,"messages":[{"role":"user","content":"` + policyPrompt + `"}]}`
	response, answer := fx.post("/v1/messages", "anthropic", body)
	if response.StatusCode != 200 || response.Header.Get(downgradedFromHeader) != "" {
		t.Fatalf("status %d header %q: %s", response.StatusCode, response.Header.Get(downgradedFromHeader), answer)
	}
	if offer := accounting.lastOffer(); len(offer) != 1 {
		t.Fatalf("a request already on the target was offered a second target: %+v", offer)
	}
	if string(decode(t, upstream.body)["model"]) != `"fake-haiku"` {
		t.Fatalf("sent: %s", upstream.body)
	}
}

// staleAccounting reports that a reload overtook every admission.
type staleAccounting struct {
	mu         sync.Mutex
	requestIDs []string
}

func (a *staleAccounting) Admit(context.Context, *config.Snapshot, *Envelope, string, config.Provider) (Admission, *Rejection) {
	panic("a policy-aware accounting must be offered candidates")
}

func (a *staleAccounting) AdmitCandidates(_ context.Context, _ *config.Snapshot, e *Envelope, _ []Candidate) (Admission, *Rejection) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.requestIDs = append(a.requestIDs, e.RequestID)
	return Admission{}, &Rejection{Status: http.StatusServiceUnavailable, Code: CodeInternalError, Class: ClassInternal, Message: "configuration changed during admission; retry the request", Stale: true}
}

func TestPolicyReloadRetriesAreBounded(t *testing.T) {
	accounting := &staleAccounting{}
	upstream, server := startUpstream(t, nil)
	fx := newFixture(t, server.URL, fixtureOptions{accounting: accounting, reloads: true, config: strings.Replace(policyConfig, "TARGET", "fake-haiku", 1)})
	body := `{"model":"fake-claude","max_tokens":10,"messages":[{"role":"user","content":"` + policyPrompt + `"}]}`
	response, answer := fx.post("/v1/messages", "anthropic", body)
	if response.StatusCode != http.StatusServiceUnavailable || upstream.count() != 0 {
		t.Fatalf("status %d upstream %d: %s", response.StatusCode, upstream.count(), answer)
	}
	// The first admission and three new preparations, all under one request ID.
	if len(accounting.requestIDs) != 1+staleRetries {
		t.Fatalf("%d admissions", len(accounting.requestIDs))
	}
	for _, id := range accounting.requestIDs {
		if id != accounting.requestIDs[0] {
			t.Fatal("a new preparation changed the request ID")
		}
	}
	if len(fx.sink.all()) != 0 {
		t.Fatal("a request never admitted produced an attempt")
	}
}

func TestForbiddenModelIsRefusedBeforeAccounting(t *testing.T) {
	accounting := downgrading()
	upstream, server := startUpstream(t, nil)
	config := strings.Replace(strings.Replace(policyConfig, "TARGET", "fake-haiku", 1), "allowed_models: [fake-claude, fake-haiku, fake-gpt, fake-gpt-mini]", "allowed_models: [fake-haiku, fake-gpt]", 1)
	fx := newFixture(t, server.URL, fixtureOptions{accounting: accounting, config: config})
	body := `{"model":"fake-claude","max_tokens":10,"messages":[{"role":"user","content":"` + policyPrompt + `"}]}`
	response, answer := fx.post("/v1/messages", "anthropic", body)
	want := `{"type":"error","error":{"type":"permission_error","message":"model \"fake-claude\" is not allowed for app bot","comporta_code":"model_not_allowed"}}`
	if response.StatusCode != http.StatusForbidden || answer != want || upstream.count() != 0 || len(accounting.offers) != 0 {
		t.Fatalf("status %d upstream %d: %s", response.StatusCode, upstream.count(), answer)
	}
}
