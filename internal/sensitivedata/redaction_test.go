package sensitivedata

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"testing"
)

var placeholder = regexp.MustCompile(`\[([A-Z]+)_(\d+)_([0-9a-f]{24})\]`)

func redactWithMap(t *testing.T, policy Policy, texts ...string) *Outcome {
	t.Helper()
	policy.Rehydrate = true
	outcome, err := scanTexts(t, NewScanner(), policy, texts...)
	if err != nil {
		t.Fatal(err)
	}
	return outcome
}

func TestOverlapResolution(t *testing.T) {
	cpf := cpfFrom("529982247")
	// The CPF is the local part of the address: the longer span wins and protects both.
	text := "contato " + cpf + "@example.org"
	outcome := redactWithMap(t, policyAll(ModeRedact), text)
	rewritten, _ := outcome.Rewritten(0)
	if strings.Contains(rewritten, cpf) || len(placeholder.FindAllString(rewritten, -1)) != 1 || !strings.Contains(rewritten, "[EMAIL_1_") {
		t.Fatalf("rewritten = %q", rewritten)
	}
	if fmt.Sprint(outcome.Summary) != fmt.Sprint(Summary{{DataType: CPF, Count: 1, Action: ModeRedact}, {DataType: Email, Count: 1, Action: ModeRedact}}) {
		t.Fatalf("both candidates must be counted: %+v", outcome.Summary)
	}

	// Same span, different categories: precedence decides (CNPJ before card).
	cnpj := cnpjFrom("112223330001")
	if validLuhn(cnpj) {
		outcome = redactWithMap(t, policyAll(ModeRedact), cnpj)
		rewritten, _ = outcome.Rewritten(0)
		if !strings.HasPrefix(rewritten, "[CNPJ_1_") {
			t.Fatalf("tie = %q", rewritten)
		}
	}

	// Detect never wins over redact and does not change bytes.
	policy := policyOnly(CPF, ModeRedact)
	policy.Set(Email, ModeDetect)
	outcome = redactWithMap(t, policy, text)
	rewritten, _ = outcome.Rewritten(0)
	if !strings.Contains(rewritten, "[CPF_1_") || !strings.HasSuffix(rewritten, "@example.org") {
		t.Fatalf("detect overlap = %q", rewritten)
	}
}

func TestRepeatedValueCounts(t *testing.T) {
	a, b := "ana@example.org", "bia@example.org"
	outcome := redactWithMap(t, policyOnly(Email, ModeRedact), a+" "+b+" "+a, "de novo "+a)
	if outcome.Summary[0].Count != 4 {
		t.Fatalf("count = %d, want 4", outcome.Summary[0].Count)
	}
	first, _ := outcome.Rewritten(0)
	second, _ := outcome.Rewritten(1)
	tokens := placeholder.FindAllStringSubmatch(first+" "+second, -1)
	indexes := []string{}
	for _, token := range tokens {
		indexes = append(indexes, token[2])
	}
	if strings.Join(indexes, ",") != "1,2,1,1" {
		t.Fatalf("indexes = %v", indexes)
	}
	if outcome.Map().Len() != 2 {
		t.Fatalf("map size = %d", outcome.Map().Len())
	}
	restored, changed := outcome.Map().Restore(second)
	if !changed || restored != "de novo "+a {
		t.Fatalf("restored = %q", restored)
	}
}

// fixedEntropy repeats the same bytes, so every nonce it yields is the same.
type fixedEntropy struct{ b byte }

func (f fixedEntropy) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = f.b
	}
	return len(p), nil
}

type failingEntropy struct{}

func (failingEntropy) Read([]byte) (int, error) { return 0, errors.New("no entropy") }

func TestPlaceholderCollision(t *testing.T) {
	nonce := strings.Repeat("ab", nonceBytes)
	scanner := NewScanner(WithEntropy(fixedEntropy{0xab}))
	text := "ana@example.org e o texto já tem " + nonce
	if _, err := scanTexts(t, scanner, policyOnly(Email, ModeRedact), text); !isReason(err, ReasonCollision) {
		t.Fatalf("err = %v, want collision", err)
	}
	outcome, err := scanTexts(t, scanner, policyOnly(Email, ModeRedact), "ana@example.org")
	if err != nil {
		t.Fatal(err)
	}
	rewritten, _ := outcome.Rewritten(0)
	if rewritten != "[EMAIL_1_"+nonce+"]" {
		t.Fatalf("rewritten = %q", rewritten)
	}
}

func TestEntropyFailure(t *testing.T) {
	scanner := NewScanner(WithEntropy(failingEntropy{}))
	if _, err := scanTexts(t, scanner, policyOnly(Email, ModeRedact), "ana@example.org"); !isReason(err, ReasonEntropy) {
		t.Fatalf("err = %v", err)
	}
	// Detect needs no nonce.
	if _, err := scanTexts(t, scanner, policyOnly(Email, ModeDetect), "ana@example.org"); err != nil {
		t.Fatalf("detect err = %v", err)
	}
}

func TestPrivateMapRepresentations(t *testing.T) {
	value := "ana@example.org"
	outcome := redactWithMap(t, policyOnly(Email, ModeRedact), "contato "+value)
	m := outcome.Map()
	var logs bytes.Buffer
	slog.New(slog.NewJSONHandler(&logs, nil)).Info("x", "map", m, "outcome", outcome)
	data, _ := json.Marshal(struct {
		M *ReverseMap
		O *Outcome
	}{m, outcome})
	for _, rendered := range []string{fmt.Sprint(m), fmt.Sprintf("%v %+v %#v %s", m, m, m, m), fmt.Sprintf("%v %+v %#v", outcome, outcome, outcome), logs.String(), string(data)} {
		if strings.Contains(rendered, value) || strings.Contains(rendered, "example") {
			t.Fatalf("representation leaks: %s", rendered)
		}
	}

	// Without rehydration there is no map, and detect or block keep none either.
	outcome, err := scanTexts(t, NewScanner(), policyOnly(Email, ModeRedact), value)
	if err != nil || outcome.Map() != nil {
		t.Fatalf("map without rehydrate: %v %v", outcome.Map(), err)
	}
	for _, mode := range []Mode{ModeDetect, ModeBlock} {
		p := policyOnly(Email, mode)
		p.Rehydrate = true
		outcome, _ := scanTexts(t, NewScanner(), p, value)
		if outcome.Map() != nil || outcome.Redacted() {
			t.Fatalf("%s kept a map", mode)
		}
	}

	outcome = redactWithMap(t, policyOnly(Email, ModeRedact), value)
	outcome.Release()
	outcome.Release()
	if outcome.Map() != nil || outcome.Redacted() {
		t.Fatal("release kept state")
	}
}

func TestRestoreOnlyExactTokens(t *testing.T) {
	value := "ana@example.org"
	outcome := redactWithMap(t, policyOnly(Email, ModeRedact), value)
	token, _ := outcome.Rewritten(0)
	nonce := placeholder.FindStringSubmatch(token)[3]
	foreign := "[EMAIL_1_" + strings.Repeat("0", 24) + "]"
	cases := map[string]string{
		"Contato: " + token:                   "Contato: " + value,
		token + token:                         value + value,
		foreign:                               foreign,
		"[EMAIL_2_" + nonce + "]":             "[EMAIL_2_" + nonce + "]",
		"EMAIL_1_" + nonce + "]":              "EMAIL_1_" + nonce + "]",
		"[EMAIL_1_" + nonce:                   "[EMAIL_1_" + nonce,
		"[email_1_" + nonce + "]":             "[email_1_" + nonce + "]",
		"[[EMAIL_1_" + nonce + "]]":           "[" + value + "]",
		"[x " + strings.Repeat("y", 40) + "]": "[x " + strings.Repeat("y", 40) + "]",
	}
	for input, want := range cases {
		if got, _ := outcome.Map().Restore(input); got != want {
			t.Errorf("Restore(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestRestoreIsOnePass(t *testing.T) {
	// The original value itself looks like a placeholder of this request; it must
	// come back literally, never expanded again.
	outcome := redactWithMap(t, policyAll(ModeRedact), "a@example.org", "b@example.org")
	first, _ := outcome.Rewritten(0)
	second, _ := outcome.Rewritten(1)
	m := outcome.Map()
	m.values[second] = first
	got, _ := m.Restore(second)
	if got != first {
		t.Fatalf("restore expanded twice: %q", got)
	}
}
