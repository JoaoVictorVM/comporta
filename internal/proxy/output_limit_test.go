package proxy

import (
	"encoding/json"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

var capPrompt = testkit.Prompt("Exemplo de limite")

func TestOutputCapBoundsQuoteBeforePolicyFeature(t *testing.T) {
	for _, tc := range []struct {
		body, field string
		known       bool
		value       int64
		want        string
	}{
		{`{"future":{"enabled":true}}`, "", false, 0, "4096"},
		{`{"max_tokens":99999,"future":{"enabled":true}}`, "max_tokens", true, 99999, "4096"},
		{`{"max_completion_tokens":99999,"future":{"enabled":true}}`, "max_completion_tokens", true, 99999, "4096"},
		{`{ "max_tokens" : 5, "future":{"enabled":true} }`, "max_tokens", true, 5, "5"},
	} {
		document, err := ParseDocument([]byte(tc.body))
		if err != nil {
			t.Fatal(err)
		}
		p := &prepared{document: document, envelope: &Envelope{Operation: OperationGeneration, OutputLimitField: tc.field, MaxTokens: Optional[int64]{Known: tc.known, Value: tc.value}}}
		if err = p.capOutput(4096); err != nil {
			t.Fatal(err)
		}
		var got map[string]json.RawMessage
		if err = json.Unmarshal(p.effectiveBody(), &got); err != nil {
			t.Fatal(err)
		}
		field := tc.field
		if field == "" {
			field = "max_tokens"
		}
		if string(got[field]) != tc.want || string(got["future"]) != `{"enabled":true}` {
			t.Fatalf("cap changed unrelated data: %s", p.effectiveBody())
		}
		if tc.field == "max_completion_tokens" && got["max_tokens"] != nil {
			t.Fatal("both output fields forwarded")
		}
		if tc.value == 5 && string(p.effectiveBody()) != tc.body {
			t.Fatal("lower cap changed bytes")
		}
	}
}

func TestPolicyCapsBothCandidates(t *testing.T) {
	for _, selected := range []CandidateID{CandidateOriginal, CandidateDowngrade} {
		accounting := &policyAccounting{selected: selected}
		fx, upstream := policyFixture(t, "fake-haiku", accounting, nil)
		body := `{"model":"fake-claude","max_tokens":9000,"messages":[{"role":"user","content":"` + capPrompt + `"}]}`
		if response, answer := fx.post("/v1/messages", "anthropic", body); response.StatusCode != 200 {
			t.Fatalf("status %d: %s", response.StatusCode, answer)
		}
		if got := string(decode(t, upstream.body)["max_tokens"]); got != "4096" {
			t.Fatalf("%s sent max_tokens %s", selected, got)
		}
		// Both candidates are estimated with the capped output.
		if e := accounting.envelopes[0]; !e.MaxTokens.Known || e.MaxTokens.Value != 4096 {
			t.Fatalf("estimated with %+v", e.MaxTokens)
		}
	}
}

func TestMissingOpenAILimitInjected(t *testing.T) {
	accounting := downgrading()
	fx, upstream := policyFixture(t, "fake-gpt-mini", accounting, nil)
	body := `{"model":"fake-gpt","messages":[{"role":"user","content":"` + capPrompt + `"}]}`
	if response, answer := fx.post("/v1/chat/completions", "openai", body); response.StatusCode != 200 {
		t.Fatalf("status %d: %s", response.StatusCode, answer)
	}
	sent := decode(t, upstream.body)
	if string(sent["max_tokens"]) != "4096" || string(sent["model"]) != `"fake-gpt-mini"` || sent["max_completion_tokens"] != nil {
		t.Fatalf("sent: %s", upstream.body)
	}
	if e := accounting.envelopes[0]; e.OutputLimitField != "max_tokens" || e.MaxTokens.Value != 4096 {
		t.Fatalf("envelope: %+v", e.MaxTokens)
	}
}

func TestOutputAliasIsNotDuplicated(t *testing.T) {
	accounting := downgrading()
	fx, upstream := policyFixture(t, "fake-gpt-mini", accounting, nil)
	body := `{"model":"fake-gpt","max_completion_tokens":9000,"messages":[{"role":"user","content":"` + capPrompt + `"}]}`
	if response, answer := fx.post("/v1/chat/completions", "openai", body); response.StatusCode != 200 {
		t.Fatalf("status %d: %s", response.StatusCode, answer)
	}
	sent := decode(t, upstream.body)
	if string(sent["max_completion_tokens"]) != "4096" || sent["max_tokens"] != nil || string(sent["model"]) != `"fake-gpt-mini"` {
		t.Fatalf("sent: %s", upstream.body)
	}
	// The parser's rejection of both fields together holds for the policy path.
	both := `{"model":"fake-gpt","max_tokens":1,"max_completion_tokens":1,"messages":[]}`
	if response, answer := fx.post("/v1/chat/completions", "openai", both); response.StatusCode != 400 || upstream.count() != 1 {
		t.Fatalf("status %d: %s", response.StatusCode, answer)
	}
}
