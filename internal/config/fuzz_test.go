package config

import (
	"strings"
	"testing"
)

func FuzzLoadYAML(f *testing.F) {
	for _, seed := range []string{
		baseYAML,
		"a: &a [*a]\n",
		"a: !!binary x\n",
		"<<: {a: 1}\n",
		"---\n---\n",
		"\"\\x00\": 1\n",
		"[[[[[[",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		_, _ = LoadBytes([]byte(text), testConfigPath, structuralOptions())
	})
}

func FuzzDiagnosticRedaction(f *testing.F) {
	const canary = "CANARYSECRET"
	f.Add("api_key: %s\n")
	f.Add("providers:\n  p:\n    api_key: %s\n")
	f.Add("providers:\n  p:\n    api_key_file: ${%s}\n    api_key: x\n")
	f.Add("timezone: %s\nfail_mode: [%s\n")
	f.Fuzz(func(t *testing.T, template string) {
		if strings.Contains(template, canary) {
			t.Skip()
		}
		_, err := LoadBytes([]byte(strings.ReplaceAll(template, "%s", canary)), testConfigPath, structuralOptions())
		if err == nil {
			return
		}
		failure := err.(*Error)
		for _, diagnostic := range failure.Diagnostics {
			// Key names are data that legitimately appears in paths, and quoted
			// references name a model. Only the message of a secret field is
			// guaranteed never to carry the value.
			if diagnostic.Code == CodeLiteralSecret && strings.Contains(diagnostic.Message, canary) {
				t.Fatalf("literal secret exposed: %s", diagnostic)
			}
		}
	})
}

func FuzzMoney(f *testing.F) {
	for _, seed := range []string{"0", "2.40", "0.000001", "9223372036854.775807", "9223372036854.775808", "1e3", ""} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, lexeme string) {
		value, err := ParseMoney(lexeme)
		if err != nil {
			return
		}
		if value < 0 {
			t.Fatalf("ParseMoney(%q) overflowed to %d", lexeme, value)
		}
		again, err := ParseMoney(value.String())
		if err != nil || again != value {
			t.Fatalf("value does not round trip: %q -> %s -> %d (%v)", lexeme, value, again, err)
		}
	})
}
