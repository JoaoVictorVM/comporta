package sensitivedata

import (
	"context"
	"errors"
	"testing"
)

func FuzzScanner(f *testing.F) {
	for _, seed := range []string{
		"", "cliente " + cpfFrom("529982247"), maskCNPJ(cnpjFrom("112223330001")), cardFrom("4", 16),
		"ana@example.org", "+55 (11) 98765-4321", "123e4567-e89b-42d3-a456-426614174000",
		anthropicKey(), jwtToken(), privateKey("PRIVATE KEY"), "@@@--((++55", "é\u0000[EMAIL_1_]",
	} {
		f.Add(seed, seed)
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		policy := policyAll(ModeRedact)
		policy.Rehydrate = true
		outcome, err := scanTexts(t, NewScanner(), policy, a, b)
		if err != nil {
			if !errors.Is(err, ErrScanFailed) || err.Error() != "sensitive data scan failed" {
				t.Fatalf("unexpected error %v", err)
			}
			return
		}
		if outcome == nil {
			t.Fatal("active policy returned no outcome")
		}
		if err := outcome.Summary.Validate(); err != nil {
			t.Fatalf("invalid summary %+v", outcome.Summary)
		}
		// Redaction followed by restoration is the identity on every field.
		for i, original := range []string{a, b} {
			rewritten, changed := outcome.Rewritten(i)
			if !changed {
				continue
			}
			if restored, _ := outcome.Map().Restore(rewritten); restored != original {
				t.Fatalf("field %d did not round trip", i)
			}
		}
	})
}

func FuzzSecretCandidates(f *testing.F) {
	for _, seed := range []string{anthropicKey(), openAIKey(), awsKeyID(), githubClassic(), githubFineGrained(), jwtToken(), privateKey("RSA PRIVATE KEY"), "sk-sk-sk-", "eyJ.eyJ.", "-----BEGIN -----END"} {
		f.Add(seed)
	}
	limits := DefaultLimits()
	limits.MaxSecretBytes = 128
	f.Fuzz(func(t *testing.T, text string) {
		outcome, err := NewScanner(WithLimits(limits)).Scan(context.Background(), policyOnly(Secret, ModeBlock), func(emit Emit) error { return emit(text) })
		if err != nil {
			if !isReason(err, ReasonCapacity) && !isReason(err, ReasonTimeout) {
				t.Fatalf("unexpected error %v", err)
			}
			return
		}
		if outcome.Blocked != nil && !ValidSecretKind(outcome.Blocked.Kind) {
			t.Fatalf("kind %q outside the allowlist", outcome.Blocked.Kind)
		}
		if err := outcome.Summary.Validate(); err != nil {
			t.Fatalf("invalid summary %+v", outcome.Summary)
		}
	})
}
