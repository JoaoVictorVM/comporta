package proxy

import (
	"encoding/json"
	"errors"

	"github.com/JoaoVictorVM/comporta/internal/auth"
)

// errInvalidUsage reports counters that are malformed or contradict each other.
var errInvalidUsage = errors.New("invalid usage")

// Usage holds token counters normalized across provider formats. A counter a
// provider did not report stays unknown: turning it into a known zero would make a
// missing report look like free consumption.
type Usage struct {
	Input         Optional[int64]
	Output        Optional[int64]
	CacheWrite    Optional[int64]
	CacheRead     Optional[int64]
	RawInputTotal Optional[int64]
}

type anthropicUsage struct {
	Input      *int64 `json:"input_tokens"`
	Output     *int64 `json:"output_tokens"`
	CacheWrite *int64 `json:"cache_creation_input_tokens"`
	CacheRead  *int64 `json:"cache_read_input_tokens"`
}

type openAIUsage struct {
	Prompt     *int64 `json:"prompt_tokens"`
	Completion *int64 `json:"completion_tokens"`
	Details    *struct {
		Cached *int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func nonNegative(values ...*int64) error {
	for _, value := range values {
		if value != nil && *value < 0 {
			return errInvalidUsage
		}
	}
	return nil
}

func assign(target *Optional[int64], value *int64) {
	if value != nil {
		*target = known(*value)
	}
}

// mergeAnthropic overwrites the counters present. Anthropic reports totals, so a
// later event replaces an earlier value instead of adding to it.
func (u *Usage) mergeAnthropic(raw json.RawMessage) error {
	var parsed anthropicUsage
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return errInvalidUsage
	}
	if err := nonNegative(parsed.Input, parsed.Output, parsed.CacheWrite, parsed.CacheRead); err != nil {
		return err
	}
	assign(&u.Input, parsed.Input)
	assign(&u.Output, parsed.Output)
	assign(&u.CacheWrite, parsed.CacheWrite)
	assign(&u.CacheRead, parsed.CacheRead)
	return nil
}

// mergeOpenAI normalizes prompt_tokens, which already includes cached tokens, into
// the same disjoint dimensions Anthropic reports. The raw total is kept for audit.
func (u *Usage) mergeOpenAI(raw json.RawMessage) error {
	var parsed openAIUsage
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return errInvalidUsage
	}
	var cached *int64
	if parsed.Details != nil {
		cached = parsed.Details.Cached
	}
	if err := nonNegative(parsed.Prompt, parsed.Completion, cached); err != nil {
		return err
	}
	if parsed.Prompt != nil && cached != nil && *cached > *parsed.Prompt {
		return errInvalidUsage
	}
	assign(&u.Output, parsed.Completion)
	assign(&u.CacheRead, cached)
	if parsed.Prompt != nil {
		u.RawInputTotal = known(*parsed.Prompt)
		input := *parsed.Prompt
		if cached != nil {
			input -= *cached
		}
		u.Input = known(input)
	}
	return nil
}

func (u *Usage) merge(format auth.Format, raw json.RawMessage) error {
	if format == auth.FormatAnthropic {
		return u.mergeAnthropic(raw)
	}
	return u.mergeOpenAI(raw)
}

func (u *Usage) primaryKnown() bool { return u.Input.Known && u.Output.Known }

func isJSONNull(raw json.RawMessage) bool { return string(raw) == "null" }

// extractResponseUsage reads the counters of a whole JSON response without touching
// the bytes that go to the client. A response with no usage is still valid: it is
// delivered, and its result says the usage is incomplete.
func extractResponseUsage(format auth.Format, operation Operation, body []byte) (Usage, Optional[int64], error) {
	var usage Usage
	if operation == OperationCountTokens {
		var parsed struct {
			Input *int64 `json:"input_tokens"`
		}
		if err := json.Unmarshal(body, &parsed); err != nil {
			return usage, Optional[int64]{}, errInvalidUsage
		}
		if parsed.Input == nil {
			return usage, Optional[int64]{}, nil
		}
		if *parsed.Input < 0 {
			return usage, Optional[int64]{}, errInvalidUsage
		}
		return usage, known(*parsed.Input), nil
	}
	var parsed struct {
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return usage, Optional[int64]{}, errInvalidUsage
	}
	if len(parsed.Usage) == 0 || isJSONNull(parsed.Usage) {
		return usage, Optional[int64]{}, nil
	}
	return usage, Optional[int64]{}, usage.merge(format, parsed.Usage)
}
