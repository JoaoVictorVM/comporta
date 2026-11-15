package sensitivedata

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
)

// Fixtures are built at run time from synthetic parts. Writing complete credential
// shapes as literals would make the repository look like it leaks secrets.

func cpfFrom(base string) string {
	d1 := checkDigit(base, cpfWeight)
	d2 := checkDigit(base+string(d1), cpfWeight)
	return base + string(d1) + string(d2)
}

func cnpjFrom(base string) string {
	d1 := checkDigit(base, cnpjWeight)
	d2 := checkDigit(base+string(d1), cnpjWeight)
	return base + string(d1) + string(d2)
}

func maskCPF(d string) string { return d[:3] + "." + d[3:6] + "." + d[6:9] + "-" + d[9:] }
func maskCNPJ(d string) string {
	return d[:2] + "." + d[2:5] + "." + d[5:8] + "/" + d[8:12] + "-" + d[12:]
}

// cardFrom completes a prefix to the length with a Luhn check digit.
func cardFrom(prefix string, length int) string {
	body := prefix + strings.Repeat("3", length-1-len(prefix))
	for d := byte('0'); d <= '9'; d++ {
		if candidate := body + string(d); validLuhn(candidate) {
			return candidate
		}
	}
	panic("unreachable")
}

func breakLastDigit(s string) string {
	last := s[len(s)-1]
	return s[:len(s)-1] + string('0'+(last-'0'+1)%10)
}

func keyBody(n int) string { return strings.Repeat("Zq7_", n/4+1)[:n] }

func anthropicKey() string { return "sk-" + "ant-" + "api03-" + keyBody(40) }
func openAIKey() string    { return "sk-" + "proj-" + keyBody(40) }
func awsKeyID() string     { return "AK" + "IA" + "QZ7RT5WX3YV2MN4P" }
func githubClassic() string {
	return "gh" + "p_" + strings.Repeat("aB3", 12)
}
func githubFineGrained() string { return "github" + "_pat_" + strings.Repeat("aB3_", 10) }

func jwtToken() string {
	segment := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	return segment(`{"alg":"none","typ":"JWT"}`) + "." + segment(`{"sub":"synthetic"}`) + "." + "c2lnbmF0dXJl"
}

func privateKey(label string) string {
	body := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("synthetic key material ", 6)))
	return "-----BEGIN " + label + "-----\n" + body[:64] + "\n" + body[64:] + "\n-----END " + label + "-----"
}

func policyAll(mode Mode) Policy {
	var p Policy
	for _, c := range Categories() {
		p.Set(c, mode)
	}
	return p
}

func policyOnly(c Category, mode Mode) Policy {
	var p Policy
	p.Set(c, mode)
	return p
}

func scanTexts(t *testing.T, scanner *Scanner, policy Policy, texts ...string) (*Outcome, error) {
	t.Helper()
	return scanner.Scan(context.Background(), policy, func(emit Emit) error {
		for _, text := range texts {
			if err := emit(text); err != nil {
				return err
			}
		}
		return nil
	})
}

// countOf runs one category in detect mode and returns its count.
func countOf(t *testing.T, c Category, text string) int64 {
	t.Helper()
	outcome, err := scanTexts(t, NewScanner(), policyOnly(c, ModeDetect), text)
	if err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	for _, d := range outcome.Summary {
		if d.DataType == c {
			return d.Count
		}
	}
	return 0
}
