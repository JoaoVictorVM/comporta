package sensitivedata

import (
	"context"
	"strings"
	"testing"
)

const benchPromptBytes = 400_000

// benchText repeats a paragraph to the size of the reference prompt. dense mixes a
// value of every category into each paragraph.
func benchText(paragraph string) string {
	return strings.Repeat(paragraph, benchPromptBytes/len(paragraph)+1)[:benchPromptBytes]
}

var benchInputs = map[string]string{
	"small/clean": "Resuma o atendimento de hoje e proponha os próximos passos.",
	"small/match": "Cliente " + cpfFrom("529982247") + " escreveu de ana@example.org pedindo retorno.",
	"400KB/clean": benchText("O relatório trimestral mostra crescimento estável nas regiões sul e sudeste, com margem preservada. "),
	"400KB/dense": benchText("Cliente " + maskCPF(cpfFrom("529982247")) + " email ana@example.org fone (11) 98765-4321 cartão " +
		cardFrom("4", 16) + " pix 123e4567-e89b-42d3-a456-426614174000. "),
	"400KB/unicode":   benchText("Atenção: ação, coração, órgão, pão — 日本語のテキスト، النص العربي ✓ "),
	"400KB/malformed": benchText("1234567890 12345 (99) 1234-56 a@b @@ sk- AKIA ghp_ eyJ.eyJ. -----BEGIN 123e4567-e89b- 4111 1111 "),
	"400KB/digits":    benchText("0123456789"),
}

func benchPolicy(mode Mode) Policy {
	p := policyAll(mode)
	p.Rehydrate = mode == ModeRedact
	return p
}

func BenchmarkScan(b *testing.B) {
	scanner := NewScanner()
	for _, mode := range []Mode{ModeOff, ModeDetect, ModeRedact} {
		for _, name := range []string{"small/clean", "small/match", "400KB/clean", "400KB/dense", "400KB/unicode", "400KB/malformed", "400KB/digits"} {
			text := benchInputs[name]
			b.Run(mode.String()+"/"+name, func(b *testing.B) {
				limits := DefaultLimits()
				// The benchmark measures the work; a dense 400 KB text exceeds the
				// production candidate limit by design.
				limits.MaxCandidates = 1 << 20
				limits.MaxMapBytes = 1 << 30
				limits.Deadline = 1 << 40
				scanner = NewScanner(WithLimits(limits))
				b.SetBytes(int64(len(text)))
				b.ReportAllocs()
				for b.Loop() {
					outcome, err := scanner.Scan(context.Background(), benchPolicy(mode), func(emit Emit) error { return emit(text) })
					if err != nil {
						b.Fatal(err)
					}
					outcome.Release()
				}
			})
		}
	}
}

// BenchmarkScanCanceled measures how fast a canceled context stops a scan of the
// largest prompt.
func BenchmarkScanCanceled(b *testing.B) {
	scanner := NewScanner()
	text := benchInputs["400KB/clean"]
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := scanner.Scan(ctx, policyAll(ModeDetect), func(emit Emit) error { return emit(text) }); err == nil {
			b.Fatal("a canceled scan succeeded")
		}
	}
}

// BenchmarkScanAdversarial feeds prefix storms close to the body limit, which a
// quadratic detector would not survive.
func BenchmarkScanAdversarial(b *testing.B) {
	scanner := NewScanner()
	for name, text := range map[string]string{
		"sk-storm":    strings.Repeat("sk-", benchPromptBytes/3),
		"at-storm":    strings.Repeat("a@", benchPromptBytes/2),
		"pem-headers": strings.Repeat("-----BEGIN "+"PRIVATE KEY-----", benchPromptBytes/27),
		"dash-storm":  strings.Repeat("-", benchPromptBytes),
		"jwt-dots":    strings.Repeat("eyJ.", benchPromptBytes/4),
	} {
		b.Run(name, func(b *testing.B) {
			b.SetBytes(int64(len(text)))
			b.ReportAllocs()
			for b.Loop() {
				outcome, err := scanner.Scan(context.Background(), policyAll(ModeDetect), func(emit Emit) error { return emit(text) })
				if err != nil && !isReason(err, ReasonCapacity) {
					b.Fatal(err)
				}
				outcome.Release()
			}
		})
	}
}
