package proxy

import (
	"net/http"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/auth"
)

func TestCacheWriteTTLBreakdown(t *testing.T) {
	var u Usage
	if err := u.mergeAnthropic([]byte(`{"input_tokens":10,"output_tokens":3,"cache_creation_input_tokens":4,"cache_creation":{"ephemeral_5m_input_tokens":2,"ephemeral_1h_input_tokens":2}}`)); err != nil {
		t.Fatal(err)
	}
	if !u.CacheWrite5m.Known || u.CacheWrite5m.Value != 2 || !u.CacheWrite1h.Known || u.CacheWrite1h.Value != 2 || u.CacheWrite.Value != 4 {
		t.Fatal("TTL breakdown lost")
	}
	before := u
	if err := u.mergeAnthropic([]byte(`{"cache_creation_input_tokens":4,"cache_creation":{"ephemeral_5m_input_tokens":5,"ephemeral_1h_input_tokens":2}}`)); err == nil {
		t.Fatal("invalid subtotal accepted")
	}
	if u != before {
		t.Fatal("invalid usage changed known counters")
	}
}

func TestReasoningNotChargedTwice(t *testing.T) {
	var u Usage
	if err := u.mergeOpenAI([]byte(`{"prompt_tokens":10,"completion_tokens":8,"completion_tokens_details":{"reasoning_tokens":5},"prompt_tokens_details":{"cached_tokens":3}}`)); err != nil {
		t.Fatal(err)
	}
	if u.Input.Value != 7 || u.CacheRead.Value != 3 || u.Output.Value != 8 {
		t.Fatal("usage dimensions counted twice")
	}
}

func TestAnthropicCumulativeUsageNotSummed(t *testing.T) {
	var usage Usage
	for _, raw := range []string{
		`{"input_tokens":10,"output_tokens":1,"cache_creation_input_tokens":5}`,
		`{"output_tokens":4}`,
		`{"output_tokens":9,"input_tokens":12}`,
	} {
		if err := usage.mergeAnthropic([]byte(raw)); err != nil {
			t.Fatal(err)
		}
	}
	if usage.Input.Value != 12 || usage.Output.Value != 9 || usage.CacheWrite.Value != 5 || usage.CacheRead.Known {
		t.Fatalf("usage %+v", usage)
	}
}

func TestOpenAICacheSubsetNormalized(t *testing.T) {
	var usage Usage
	if err := usage.mergeOpenAI([]byte(`{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":30},"completion_tokens_details":{"reasoning_tokens":5}}`)); err != nil {
		t.Fatal(err)
	}
	if usage.Input.Value != 70 || usage.CacheRead.Value != 30 || usage.RawInputTotal.Value != 100 {
		t.Fatalf("input not normalized: %+v", usage)
	}
	if usage.Output.Value != 20 {
		t.Fatalf("reasoning must not be added twice: %+v", usage)
	}
	if usage.CacheWrite.Known {
		t.Fatal("OpenAI reports no cache writes; the dimension stays unknown")
	}
	var uncached Usage
	if err := uncached.mergeOpenAI([]byte(`{"prompt_tokens":100,"completion_tokens":20}`)); err != nil {
		t.Fatal(err)
	}
	if uncached.Input.Value != 100 || uncached.CacheRead.Known {
		t.Fatalf("usage %+v", uncached)
	}
}

func TestMissingUsageIsNotZero(t *testing.T) {
	var usage Usage
	if err := usage.mergeAnthropic([]byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	if usage.Input.Known || usage.Output.Known || usage.CacheRead.Known || usage.CacheWrite.Known || usage.primaryKnown() {
		t.Fatalf("usage %+v", usage)
	}
	zero := Usage{}
	if err := zero.mergeAnthropic([]byte(`{"input_tokens":0,"output_tokens":0}`)); err != nil {
		t.Fatal(err)
	}
	if !zero.Input.Known || zero.Input.Value != 0 || !zero.primaryKnown() {
		t.Fatalf("a reported zero is known: %+v", zero)
	}
}

func TestInvalidCountersAreRefused(t *testing.T) {
	cases := []struct {
		format auth.Format
		raw    string
	}{
		{auth.FormatAnthropic, `{"input_tokens":-1}`},
		{auth.FormatAnthropic, `{"output_tokens":1.5}`},
		{auth.FormatAnthropic, `{"input_tokens":"3"}`},
		{auth.FormatAnthropic, `{"input_tokens":9223372036854775808}`},
		{auth.FormatOpenAI, `{"prompt_tokens":-1}`},
		{auth.FormatOpenAI, `{"prompt_tokens":1,"prompt_tokens_details":{"cached_tokens":2}}`},
		{auth.FormatOpenAI, `{"prompt_tokens":2,"prompt_tokens_details":{"cached_tokens":-1}}`},
	}
	for _, tc := range cases {
		var usage Usage
		if err := usage.merge(tc.format, []byte(tc.raw)); err == nil {
			t.Errorf("%s accepted: %+v", tc.raw, usage)
		}
	}
}

func TestInterruptedStreamPreservesObservedUsage(t *testing.T) {
	text := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n"
	_, server := startUpstream(t, sseReply(text))
	fx := newFixture(t, server.URL, fixtureOptions{})
	fx.post("/v1/messages", "anthropic", streamAnthropicBody)
	result := fx.sink.wait(t)
	if result.UsageComplete || result.TerminalSeen {
		t.Fatalf("an interrupted stream is incomplete: %+v", result)
	}
	if result.Usage.Input.Value != 10 || result.Usage.Output.Value != 1 {
		t.Fatalf("observed counters lost: %+v", result.Usage)
	}
}

func TestStreamWithoutFinalCountsIsIncomplete(t *testing.T) {
	cases := map[string]struct {
		path, format, body, text string
	}{
		"anthropic stop with only the initial output": {
			"/v1/messages", "anthropic", streamAnthropicBody,
			"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
		},
		"openai done without usage": {
			"/v1/chat/completions", "openai", streamOpenAIBody, deltaChunk + doneChunk,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, server := startUpstream(t, sseReply(tc.text))
			fx := newFixture(t, server.URL, fixtureOptions{})
			response, _ := fx.post(tc.path, tc.format, tc.body)
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status %d", response.StatusCode)
			}
			result := fx.sink.wait(t)
			if !result.TerminalSeen || result.UsageComplete || result.ErrorCode != "" {
				t.Fatalf("result %+v", result)
			}
		})
	}
}

func TestCountTokensIsNotGenerationUsage(t *testing.T) {
	_, server := startUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":42}`))
	})
	fx := newFixture(t, server.URL, fixtureOptions{})
	fx.post("/v1/messages/count_tokens", "anthropic", countBody)
	result := fx.sink.wait(t)
	if result.Operation != OperationCountTokens || !result.CountedInputTokens.Known || result.CountedInputTokens.Value != 42 {
		t.Fatalf("result %+v", result)
	}
	if result.Usage.Input.Known || result.Usage.Output.Known || result.UsageComplete {
		t.Fatalf("a count must never read as consumption: %+v", result)
	}
}
