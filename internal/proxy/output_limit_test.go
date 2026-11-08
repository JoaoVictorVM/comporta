package proxy

import (
	"encoding/json"
	"testing"
)

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
