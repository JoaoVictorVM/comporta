package sensitivedata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func isReason(err error, reason Reason) bool {
	var scanErr *ScanError
	return errors.As(err, &scanErr) && scanErr.Reason == reason
}

func TestAllModes(t *testing.T) {
	cpf := cpfFrom("529982247")
	text := "cliente " + cpf + " escreveu de ana@example.org"
	for _, mode := range []Mode{ModeDetect, ModeRedact, ModeBlock} {
		t.Run(mode.String(), func(t *testing.T) {
			policy := policyOnly(CPF, mode)
			policy.Set(Email, ModeDetect)
			outcome, err := scanTexts(t, NewScanner(), policy, text)
			if err != nil {
				t.Fatal(err)
			}
			want := Summary{{DataType: CPF, Count: 1, Action: mode}, {DataType: Email, Count: 1, Action: ModeDetect}}
			if fmt.Sprint(outcome.Summary) != fmt.Sprint(want) {
				t.Fatalf("summary = %+v, want %+v", outcome.Summary, want)
			}
			rewritten, changed := outcome.Rewritten(0)
			switch mode {
			case ModeDetect:
				if changed || outcome.Blocked != nil {
					t.Fatal("detect must not change or block")
				}
			case ModeRedact:
				if !changed || strings.Contains(rewritten, cpf) || !strings.Contains(rewritten, "[CPF_1_") || !strings.Contains(rewritten, "ana@example.org") {
					t.Fatal("redact must replace only the redacted category")
				}
			case ModeBlock:
				if changed || outcome.Blocked == nil || outcome.Blocked.Message() != "request contains sensitive data (cpf); blocked by policy" {
					t.Fatalf("block outcome = %v", outcome)
				}
			}
		})
	}
}

func TestBlockPrecedence(t *testing.T) {
	policy := policyAll(ModeBlock)
	outcome, err := scanTexts(t, NewScanner(), policy, "ana@example.org "+cpfFrom("529982247"), githubClassic())
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Blocked.Category != Secret || outcome.Blocked.Kind != KindGitHubToken {
		t.Fatalf("blocked = %+v", outcome.Blocked)
	}
	if outcome.Redacted() {
		t.Fatal("a refusal must not build replacements")
	}
}

func TestMixedPolicyFailClosed(t *testing.T) {
	failing := func(emit Emit) error { panic("boom") }
	protective := policyOnly(CPF, ModeRedact)
	protective.Set(Email, ModeDetect)
	if _, err := NewScanner().Scan(context.Background(), protective, failing); !errors.Is(err, ErrScanFailed) {
		t.Fatalf("err = %v", err)
	}
	if !protective.Protective() || policyOnly(Email, ModeDetect).Protective() {
		t.Fatal("protective classification is wrong")
	}
}

func TestScanTimeout(t *testing.T) {
	var calls atomic.Int64
	start := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return start.Add(time.Duration(calls.Add(1)) * 20 * time.Millisecond) }
	scanner := NewScanner(WithClock(clock))
	_, err := scanTexts(t, scanner, policyAll(ModeDetect), "a", "b", "c", "d")
	if !isReason(err, ReasonTimeout) {
		t.Fatalf("err = %v, want timeout", err)
	}

	// A scan whose checks all passed is still discarded when it ends late.
	calls.Store(0)
	late := func() time.Time {
		if calls.Add(1) > 3 {
			return start.Add(time.Second)
		}
		return start
	}
	_, err = NewScanner(WithClock(late)).Scan(context.Background(), policyAll(ModeDetect), func(emit Emit) error { return emit("x") })
	if !isReason(err, ReasonTimeout) {
		t.Fatalf("late err = %v, want timeout", err)
	}

	deadline, cancel := context.WithTimeout(context.Background(), -time.Second)
	defer cancel()
	if _, err := NewScanner().Scan(deadline, policyAll(ModeDetect), func(Emit) error { return nil }); !isReason(err, ReasonTimeout) {
		t.Fatalf("client deadline err = %v", err)
	}
}

func TestScanTimeoutInsideLongField(t *testing.T) {
	var calls atomic.Int64
	start := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return start.Add(time.Duration(calls.Add(1)) * time.Millisecond) }
	scanner := NewScanner(WithClock(clock))
	_, err := scanTexts(t, scanner, policyOnly(Email, ModeDetect), strings.Repeat("a@b ", 100_000))
	if !isReason(err, ReasonTimeout) {
		t.Fatalf("err = %v, want timeout checked inside the field", err)
	}
}

func TestScanPanicSanitized(t *testing.T) {
	marker := "cliente " + cpfFrom("529982247")
	_, err := NewScanner().Scan(context.Background(), policyAll(ModeRedact), func(emit Emit) error {
		_ = emit(marker)
		panic(marker)
	})
	if !isReason(err, ReasonPanic) || strings.Contains(err.Error(), marker) || err.Error() != "sensitive data scan failed" {
		t.Fatalf("err = %v", err)
	}
	_, err = NewScanner().Scan(context.Background(), policyAll(ModeRedact), func(Emit) error { return errors.New(marker) })
	if !errors.Is(err, ErrScanFailed) || strings.Contains(err.Error(), marker) {
		t.Fatalf("selection err = %v", err)
	}
}

func TestCandidateAndMapLimits(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxCandidates = 3
	scanner := NewScanner(WithLimits(limits))
	if _, err := scanTexts(t, scanner, policyOnly(Email, ModeDetect), "a@x.org b@x.org c@x.org d@x.org"); !isReason(err, ReasonCapacity) {
		t.Fatalf("candidates err = %v", err)
	}
	if _, err := scanTexts(t, scanner, policyOnly(Email, ModeDetect), "a@x.org b@x.org c@x.org"); err != nil {
		t.Fatalf("at the limit err = %v", err)
	}

	limits = DefaultLimits()
	limits.MaxMapBytes = 20
	scanner = NewScanner(WithLimits(limits))
	if _, err := scanTexts(t, scanner, policyOnly(Email, ModeRedact), "first@example.org second@example.org"); !isReason(err, ReasonCapacity) {
		t.Fatalf("map err = %v", err)
	}
	// Repeating one value does not grow the retained bytes.
	if _, err := scanTexts(t, scanner, policyOnly(Email, ModeRedact), "first@example.org first@example.org"); err != nil {
		t.Fatalf("repeated value err = %v", err)
	}
}

func TestCanceledScan(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	_, err := NewScanner().Scan(ctx, policyAll(ModeDetect), func(emit Emit) error {
		if err := emit("x"); err != nil {
			return err
		}
		cancel()
		return emit("y")
	})
	if !errors.Is(err, ErrCanceled) || !errors.Is(err, ErrScanFailed) {
		t.Fatalf("err = %v", err)
	}
}

func TestOffDoesNotScan(t *testing.T) {
	visited := false
	outcome, err := NewScanner().Scan(context.Background(), Policy{Rehydrate: true}, func(Emit) error {
		visited = true
		return nil
	})
	if visited || outcome != nil || err != nil {
		t.Fatalf("visited=%t outcome=%v err=%v", visited, outcome, err)
	}
}

func TestOnlyActiveCategoriesAreCounted(t *testing.T) {
	outcome, err := scanTexts(t, NewScanner(), policyOnly(Email, ModeDetect), cpfFrom("529982247")+" "+anthropicKey())
	if err != nil {
		t.Fatal(err)
	}
	if len(outcome.Summary) != 0 {
		t.Fatalf("summary = %+v", outcome.Summary)
	}
}

func TestSummaryValidation(t *testing.T) {
	valid := Summary{{DataType: CPF, Count: 1, Action: ModeRedact}, {DataType: Secret, Count: 2, Action: ModeBlock, SecretKinds: []string{KindAnthropicAPIKey, KindJWT}}}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	invalid := []Summary{
		{{DataType: CPF, Count: 0, Action: ModeDetect}},
		{{DataType: CPF, Count: MaxCount + 1, Action: ModeDetect}},
		{{DataType: CPF, Count: 1, Action: ModeOff}},
		{{DataType: Email, Count: 1, Action: ModeDetect}, {DataType: CPF, Count: 1, Action: ModeDetect}},
		{{DataType: CPF, Count: 1, Action: ModeDetect}, {DataType: CPF, Count: 1, Action: ModeDetect}},
		{{DataType: CPF, Count: 1, Action: ModeDetect, SecretKinds: []string{KindJWT}}},
		{{DataType: Secret, Count: 1, Action: ModeDetect, SecretKinds: []string{"password"}}},
		{{DataType: Secret, Count: 1, Action: ModeDetect, SecretKinds: []string{KindJWT, KindAnthropicAPIKey}}},
		{{DataType: categoryCount, Count: 1, Action: ModeDetect}},
	}
	for i, s := range invalid {
		if s.Validate() == nil {
			t.Errorf("case %d accepted", i)
		}
	}
	data, err := json.Marshal(valid)
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"data_type":"cpf","count":1,"action":"redact"},{"data_type":"secret","count":2,"action":"block","secret_kinds":["anthropic_api_key","jwt"]}]`
	if string(data) != want {
		t.Fatalf("json = %s", data)
	}
	var back Summary
	if err := json.Unmarshal(data, &back); err != nil || back.Validate() != nil {
		t.Fatalf("round trip: %v", err)
	}
	if json.Unmarshal([]byte(`[{"data_type":"name","count":1,"action":"detect"}]`), &back) == nil {
		t.Fatal("unknown data type accepted")
	}
	clone := valid.Clone()
	clone[1].SecretKinds[0] = "x"
	if valid[1].SecretKinds[0] != KindAnthropicAPIKey {
		t.Fatal("clone shares secret kinds")
	}
}
