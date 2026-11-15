package proxy

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/config"
	"github.com/JoaoVictorVM/comporta/internal/sensitivedata"
	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

// envelopeAccounting admits everything and keeps a copy of each envelope it saw.
type envelopeAccounting struct {
	mu        sync.Mutex
	envelopes []Envelope
	order     *[]string
}

func (a *envelopeAccounting) Admit(_ context.Context, _ *config.Snapshot, e *Envelope, _ string, _ config.Provider) (Admission, *Rejection) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.order != nil {
		*a.order = append(*a.order, "accounting")
	}
	a.envelopes = append(a.envelopes, *e)
	return Admission{Complete: func(AttemptResult) {}}, nil
}

func TestSensitiveStageBeforeAccounting(t *testing.T) {
	var order []string
	recorder := &sensitiveRecorder{order: &order}
	accounting := &envelopeAccounting{order: &order}
	fx, upstream := sensitiveFixture(t, sensitiveSetup{settings: detectionSettings("email: redact", "secret: block"), recorder: recorder, accounting: accounting})

	ok := `{"model":"fake-claude","max_tokens":10,"messages":[{"role":"user","content":"` + testkit.Prompt("para ana@example.org") + `"}]}`
	if response, answer := fx.post("/v1/messages", "anthropic", ok); response.StatusCode != 200 {
		t.Fatalf("status %d: %s", response.StatusCode, answer)
	}
	blocked := `{"model":"fake-claude","max_tokens":10,"messages":[{"role":"user","content":"use ` + syntheticAnthropicKey() + ` e ana@example.org"}]}`
	response, answer := fx.post("/v1/messages", "anthropic", blocked)
	want := `{"type":"error","error":{"type":"invalid_request_error","message":"request contains a secret (anthropic_api_key); blocked by policy","comporta_code":"sensitive_data_blocked"}}`
	if response.StatusCode != 400 || answer != want || response.Header.Get("Retry-After") != "" {
		t.Fatalf("status %d: %s", response.StatusCode, answer)
	}
	openAI := `{"model":"fake-gpt","messages":[{"role":"user","content":"use ` + syntheticAnthropicKey() + `"}]}`
	response, answer = fx.post("/v1/chat/completions", "openai", openAI)
	want = `{"error":{"type":"invalid_request_error","message":"request contains a secret (anthropic_api_key); blocked by policy","comporta_code":"sensitive_data_blocked"}}`
	if response.StatusCode != 400 || answer != want {
		t.Fatalf("openai status %d: %s", response.StatusCode, answer)
	}
	if upstream.count() != 1 || strings.Join(order, ",") != "sensitive-event,accounting,sensitive-cut,sensitive-cut" {
		t.Fatalf("upstream=%d order=%v", upstream.count(), order)
	}
	cut := recorder.cuts[0]
	if cut.Code != CodeSensitiveDataBlocked || cut.Status != 400 || len(cut.Summary) != 2 || cut.Summary[0].Action != sensitivedata.ModeRedact || cut.Summary[1].Action != sensitivedata.ModeBlock {
		t.Fatalf("cut = %+v", cut)
	}
}

func TestBeforeSendThenSensitive(t *testing.T) {
	var order []string
	recorder := &sensitiveRecorder{order: &order}
	sawOriginal := false
	hooks := Hooks{BeforeSend: func(_ context.Context, _ *Envelope, content RequestContent) *Rejection {
		order = append(order, "before-send")
		sawOriginal = strings.Contains(string(content.Body), "ana@example.org")
		return nil
	}}
	fx, upstream := sensitiveFixture(t, sensitiveSetup{settings: detectionSettings("email: redact"), recorder: recorder, hooks: hooks})
	body := `{"model":"fake-claude","max_tokens":10,"messages":[{"role":"user","content":"` + testkit.Prompt("para ana@example.org") + `"}]}`
	if response, answer := fx.post("/v1/messages", "anthropic", body); response.StatusCode != 200 {
		t.Fatalf("status %d: %s", response.StatusCode, answer)
	}
	if !sawOriginal || strings.Join(order, ",") != "before-send,sensitive-event" || strings.Contains(upstreamBody(t, upstream), "ana@") {
		t.Fatalf("order=%v sawOriginal=%t", order, sawOriginal)
	}
}

func TestScanRunsWithoutAccounting(t *testing.T) {
	fx, upstream := sensitiveFixture(t, sensitiveSetup{settings: detectionSettings("email: redact")})
	body := `{"model":"fake-claude","max_tokens":10,"messages":[{"role":"user","content":"` + testkit.Prompt("para ana@example.org") + `"}]}`
	if response, answer := fx.post("/v1/messages", "anthropic", body); response.StatusCode != 200 {
		t.Fatalf("status %d: %s", response.StatusCode, answer)
	}
	if sent := upstreamBody(t, upstream); strings.Contains(sent, "ana@") || !tokenPattern.MatchString(sent) {
		t.Fatalf("sent = %s", sent)
	}
}

func TestSensitiveEnvelopeAfterRedaction(t *testing.T) {
	accounting := &envelopeAccounting{}
	fx, upstream := sensitiveFixture(t, sensitiveSetup{settings: detectionSettings("email: redact"), recorder: &sensitiveRecorder{}, accounting: accounting})
	body := `{"model":"fake-claude","max_tokens":10,"metadata":{"user_id":"u-1"},"messages":[{"role":"user","content":"para a@b.io"}]}`
	var sent []string
	for range 2 {
		if response, answer := fx.post("/v1/messages", "anthropic", body); response.StatusCode != 200 {
			t.Fatalf("status %d: %s", response.StatusCode, answer)
		}
		sent = append(sent, upstreamBody(t, upstream))
	}
	first, second := accounting.envelopes[0], accounting.envelopes[1]
	if first.BodySizeOriginal != int64(len(body)) || first.BodySizeEffective != int64(len(sent[0])) {
		t.Fatalf("sizes original=%d effective=%d sent=%d", first.BodySizeOriginal, first.BodySizeEffective, len(sent[0]))
	}
	// Placeholders carry a new nonce on every request: the request hash differs,
	// while the loop signature, taken before redaction, does not.
	if first.RequestHash == second.RequestHash || !first.LastMessagesHash.Known || first.LastMessagesHash != second.LastMessagesHash {
		t.Fatal("hashes were not recomputed as expected")
	}
	if len(first.Sensitive) != 1 || first.Sensitive[0].DataType != sensitivedata.Email {
		t.Fatalf("summary = %+v", first.Sensitive)
	}
}

func TestSensitiveFailureModes(t *testing.T) {
	tight := sensitivedata.DefaultLimits()
	tight.MaxCandidates = 1
	two := `{"model":"fake-claude","max_tokens":10,"messages":[{"role":"user","content":"a@example.org b@example.org"}]}`
	disk := errors.New("disk")
	cases := []struct {
		name     string
		settings string
		limits   bool
		publish  error
		cut      error
		status   int
		code     string
		upstream int
	}{
		{"protective scan failure", detectionSettings("email: redact"), true, nil, nil, 503, "sensitive_data_scan_failed", 0},
		{"detect scan failure forwards", detectionSettings("email: detect"), true, nil, nil, 200, "", 1},
		{"protective publish failure", detectionSettings("email: redact"), false, disk, nil, 503, "usage_storage_unavailable", 0},
		{"detect publish failure forwards", detectionSettings("email: detect"), false, disk, nil, 200, "", 1},
		{"cut record failure", detectionSettings("email: block"), false, nil, disk, 503, "usage_storage_unavailable", 0},
		{"scan failure record failure", detectionSettings("email: redact"), true, nil, disk, 503, "usage_storage_unavailable", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &sensitiveRecorder{publishErr: tc.publish, cutErr: tc.cut}
			setup := sensitiveSetup{settings: tc.settings, recorder: recorder}
			if tc.limits {
				setup.scanner = sensitivedata.NewScanner(sensitivedata.WithLimits(tight))
			}
			fx, upstream := sensitiveFixture(t, setup)
			response, answer := fx.post("/v1/messages", "anthropic", two)
			if response.StatusCode != tc.status || upstream.count() != tc.upstream {
				t.Fatalf("status %d upstream %d: %s", response.StatusCode, upstream.count(), answer)
			}
			if tc.code != "" && comportaCode(t, answer) != tc.code {
				t.Fatalf("code = %s", comportaCode(t, answer))
			}
			if !tc.limits {
				return
			}
			if len(recorder.published) != 0 {
				t.Fatal("partial counts were published")
			}
			if tc.status == 503 && (len(recorder.cuts) != 1 || recorder.cuts[0].Summary != nil || recorder.cuts[0].Code != CodeSensitiveScanFailed) {
				t.Fatalf("cuts = %+v", recorder.cuts)
			}
			if tc.status == 200 && !strings.Contains(upstreamBody(t, upstream), "a@example.org b@example.org") {
				t.Fatal("a failed detect scan changed the body")
			}
		})
	}
}
