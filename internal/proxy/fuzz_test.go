package proxy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/auth"
	"github.com/JoaoVictorVM/comporta/internal/sensitivedata"
)

func FuzzJSONSpansAndPatchPreservation(f *testing.F) {
	for _, seed := range []string{
		`{}`, `{"a":1}`, ` { "a" : [ 1 , { "b" : null } ] , "c":"\"}" } `, `{"a":"é","ab":-1.5e3}`,
		`{"stream_options":{"include_usage":false}}`, `[`, `{"a":1,"a":2}`, "{\"a\":\"\xff\"}", `{"a":1}{`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<16 {
			return
		}
		document, err := ParseDocument(data)
		if err != nil {
			if strings.Contains(err.Error(), "invalid JSON") && !strings.HasPrefix(err.Error(), "invalid JSON at byte ") {
				t.Fatalf("error format: %v", err)
			}
			return
		}
		if !json.Valid(data) {
			t.Fatal("accepted invalid JSON")
		}
		if string(document.Apply(nil)) != string(data) {
			t.Fatal("no-op changed the bytes")
		}
		patch, err := document.SetMember(document.Root(), "comporta_probe", []byte("true"))
		if err != nil {
			t.Fatal(err)
		}
		patched := document.Apply([]Patch{patch})
		if !json.Valid(patched) {
			t.Fatalf("patch produced invalid JSON: %q", patched)
		}
		if len(patched) > len(data)+len(`,"comporta_probe":true`) {
			t.Fatal("patch expanded beyond its text")
		}
		rebuilt := string(patched[:patch.At.Start]) + string(patched[patch.At.Start+len(patch.Text):])
		original := string(data[:patch.At.Start]) + string(data[patch.At.End:])
		if rebuilt != original {
			t.Fatal("bytes outside the patch changed")
		}
		var check struct {
			Probe bool `json:"comporta_probe"`
		}
		if err := json.Unmarshal(patched, &check); err != nil || !check.Probe {
			t.Fatalf("patch not readable: %v", err)
		}
	})
}

func FuzzSSEFraming(f *testing.F) {
	for _, seed := range []string{
		"data: x\n\n", "event: a\r\ndata: b\r\n\r\n", ": c\n\ndata: d\r\re", "data: [DONE]\n\n",
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1}}\n\n", "\xef\xbb\xbfdata: x\n\n", "\r\n\r\n\n\r",
	} {
		f.Add([]byte(seed), uint8(3))
	}
	f.Fuzz(func(t *testing.T, data []byte, size uint8) {
		step := int(size)%64 + 1
		parser := newSSEParser()
		var rebuilt strings.Builder
		state := &streamState{format: auth.FormatOpenAI}
		other := &streamState{format: auth.FormatAnthropic}
		for i := 0; i < len(data); i += step {
			parser.Feed(data[i:min(i+step, len(data))])
			for {
				event, ok, err := parser.Next()
				if err != nil {
					return
				}
				if !ok {
					break
				}
				rebuilt.Write(event.Raw)
				_, _ = state.inspect(event, "p")
				_, _ = other.inspect(event, "p")
			}
		}
		if !strings.HasPrefix(string(data), rebuilt.String()) {
			t.Fatalf("framing changed bytes: %q vs %q", rebuilt.String(), data)
		}
	})
}

func FuzzSafeErrorsAndHost(f *testing.F) {
	for _, seed := range []string{"canary", `"quoted"`, "\x00", "rate_limit_error", "é"} {
		f.Add(seed, 400)
	}
	f.Fuzz(func(t *testing.T, text string, status int) {
		marked := "zqxcanary" + text
		for _, format := range []auth.Format{auth.FormatAnthropic, auth.FormatOpenAI} {
			failures := []Failure{providerStreamFailure(marked, "p"), upstreamStatusFailure(status, "p"), HostNotAllowed("")}
			for _, failure := range failures {
				payload := errorPayload(format, failure)
				if !json.Valid(payload) {
					t.Fatalf("invalid payload: %s", payload)
				}
				if strings.Contains(string(payload), "zqxcanary") || strings.Contains(string(sseErrorEvent(format, failure)), "zqxcanary") {
					t.Fatalf("input echoed: %s", payload)
				}
			}
		}
	})
}

func redactAll() sensitivedata.Policy {
	policy := sensitivedata.Policy{Rehydrate: true}
	for _, c := range sensitivedata.Categories() {
		policy.Set(c, sensitivedata.ModeRedact)
	}
	return policy
}

// FuzzSensitivePatchesJSON checks that redaction always yields valid JSON whose
// controls are unchanged, and that restoring the placeholders gives back exactly
// the decoded text of every inspected field.
func FuzzSensitivePatchesJSON(f *testing.F) {
	for _, seed := range []string{
		`{"model":"m","messages":[{"role":"user","content":"a@example.org"}]}`,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"x b@example.org"},{"type":"tool_use","id":"i","name":"n","input":{"k":["c@example.org",1]}}]}]}`,
		`{"model":"m","messages":[{"role":"assistant","tool_calls":[{"function":{"name":"f","arguments":"{\"to\":\"d@example.org\"}"}}]}]}`,
		`{"model":"m","system":"e@example.org","tools":[{"description":"f@example.org","input_schema":{"enum":["g@example.org"]}}],"messages":[]}`,
	} {
		f.Add([]byte(seed), true)
		f.Add([]byte(seed), false)
	}
	scanner := sensitivedata.NewScanner()
	f.Fuzz(func(t *testing.T, data []byte, anthropic bool) {
		if len(data) > 1<<14 {
			return
		}
		document, err := ParseDocument(data)
		if err != nil {
			return
		}
		format := auth.FormatOpenAI
		if anthropic {
			format = auth.FormatAnthropic
		}
		var originals []string
		selection := &contentSelection{document: document, format: format}
		outcome, err := scanner.Scan(context.Background(), redactAll(), func(emit sensitivedata.Emit) error {
			return selection.run(func(text string) error {
				originals = append(originals, text)
				return emit(text)
			})
		})
		if err != nil {
			return
		}
		patches, err := selection.patches(outcome)
		if err != nil {
			t.Fatalf("patches: %v", err)
		}
		out := document.Apply(patches)
		if !json.Valid(out) {
			t.Fatalf("invalid JSON after redaction: %s", out)
		}
		after, err := ParseDocument(out)
		if err != nil {
			t.Fatalf("redacted body does not parse: %v", err)
		}
		for _, key := range []string{"model", "stream", "max_tokens"} {
			a, okA := document.Path(key)
			b, okB := after.Path(key)
			if okA != okB || (okA && string(document.Bytes(a)) != string(after.Bytes(b))) {
				t.Fatalf("control %s changed", key)
			}
		}
		for i, original := range originals {
			rewritten, changed := outcome.Rewritten(i)
			if !changed {
				continue
			}
			if restored, _ := outcome.Map().Restore(rewritten); restored != original {
				t.Fatalf("field %d does not round trip", i)
			}
		}
	})
}

// FuzzRehydrationJSON checks that restoration never produces invalid JSON and
// never changes a response with no placeholder of this request.
func FuzzRehydrationJSON(f *testing.F) {
	scanner := sensitivedata.NewScanner()
	outcome, err := scanner.Scan(context.Background(), redactAll(), func(emit sensitivedata.Emit) error { return emit("a\"b@example.org") })
	if err != nil {
		f.Fatal(err)
	}
	token, _ := outcome.Rewritten(0)
	token = strings.TrimPrefix(token, "a\"")
	for _, seed := range []string{
		`{"content":[{"type":"text","text":"` + token + `"}]}`,
		`{"choices":[{"message":{"content":"` + token + `","tool_calls":[{"function":{"arguments":"{\"x\":\"` + token + `\"}"}}]}}]}`,
		`{"content":"x"}`, `{"choices":[1,{"message":[]}]}`,
	} {
		f.Add([]byte(seed), true)
		f.Add([]byte(seed), false)
	}
	f.Fuzz(func(t *testing.T, data []byte, anthropic bool) {
		if len(data) > 1<<14 || !json.Valid(data) {
			return
		}
		format := auth.FormatOpenAI
		if anthropic {
			format = auth.FormatAnthropic
		}
		got, err := rehydrateResponse(format, data, outcome.Map(), maxResponseBytes)
		if err != nil {
			return
		}
		if !json.Valid(got) {
			t.Fatalf("invalid JSON after restoration: %s", got)
		}
		if !strings.Contains(string(data), token[len(token)-25:]) && string(got) != string(data) {
			t.Fatal("a response without placeholders changed")
		}
	})
}
