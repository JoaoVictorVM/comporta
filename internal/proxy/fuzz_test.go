package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/auth"
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
