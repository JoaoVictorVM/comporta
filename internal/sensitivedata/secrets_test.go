package sensitivedata

import (
	"encoding/base64"
	"slices"
	"strings"
	"testing"
)

func secretKindsOf(t *testing.T, text string) []string {
	t.Helper()
	outcome, err := scanTexts(t, NewScanner(), policyOnly(Secret, ModeDetect), text)
	if err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	for _, d := range outcome.Summary {
		if d.DataType == Secret {
			return d.SecretKinds
		}
	}
	return nil
}

func TestSecretFamilies(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []string
	}{
		{"anthropic", "key=" + anthropicKey(), []string{KindAnthropicAPIKey}},
		{"openai", "OPENAI_API_KEY=" + openAIKey(), []string{KindOpenAIAPIKey}},
		{"aws", "aws " + awsKeyID() + " ok", []string{KindAWSAccessKeyID}},
		{"github classic", githubClassic(), []string{KindGitHubToken}},
		{"github fine grained", githubFineGrained(), []string{KindGitHubToken}},
		{"jwt", "Bearer " + jwtToken() + ".", []string{KindJWT}},
		{"private key", privateKey("PRIVATE KEY"), []string{KindPrivateKey}},
		{"rsa private key", privateKey("RSA PRIVATE KEY"), []string{KindPrivateKey}},
		{"ec private key", privateKey("EC PRIVATE KEY"), []string{KindPrivateKey}},
		{"encrypted private key", privateKey("ENCRYPTED PRIVATE KEY"), []string{KindPrivateKey}},
		{"several kinds", awsKeyID() + " " + anthropicKey(), []string{KindAnthropicAPIKey, KindAWSAccessKeyID}},
		{"truncated anthropic", "sk-" + "ant-" + keyBody(10), nil},
		{"truncated openai", "sk-" + keyBody(15), nil},
		{"prefix inside a word", "task-" + keyBody(30), nil},
		{"aws too short", "AK" + "IA" + "QZ7RT5WX3YV2MN4", nil},
		{"aws lower case", "AK" + "IA" + strings.ToLower("QZ7RT5WX3YV2MN4P"), nil},
		{"github classic too short", "gh" + "p_" + strings.Repeat("a", 35), nil},
		{"github pat too short", "github" + "_pat_" + strings.Repeat("a", 19), nil},
		{"certificate", privateKey("CERTIFICATE"), nil},
		{"public key", privateKey("PUBLIC KEY"), nil},
		{"incomplete pem", "-----BEGIN " + "PRIVATE KEY-----\nAAAA", nil},
		{"pem with bad body", "-----BEGIN " + "PRIVATE KEY-----\n!!!!\n-----END PRIVATE KEY-----", nil},
		{"mismatched pem labels", "-----BEGIN " + "RSA PRIVATE KEY-----\nAAAA\n-----END EC PRIVATE KEY-----", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := secretKindsOf(t, tc.text); !slices.Equal(got, tc.want) {
				t.Fatalf("kinds = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAnthropicBeforeOpenAI(t *testing.T) {
	outcome, err := scanTexts(t, NewScanner(), policyOnly(Secret, ModeBlock), anthropicKey())
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Blocked == nil || outcome.Blocked.Kind != KindAnthropicAPIKey {
		t.Fatalf("blocked = %+v", outcome.Blocked)
	}
	if got := outcome.Blocked.Message(); got != "request contains a secret (anthropic_api_key); blocked by policy" {
		t.Fatalf("message = %q", got)
	}
	if outcome.Summary[0].Count != 1 || !slices.Equal(outcome.Summary[0].SecretKinds, []string{KindAnthropicAPIKey}) {
		t.Fatalf("summary = %+v", outcome.Summary)
	}
}

func TestJWTStructure(t *testing.T) {
	segment := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	cases := []struct {
		name  string
		token string
		want  bool
	}{
		{"valid", jwtToken(), true},
		{"header without alg", segment(`{"typ":"JWT"}`) + "." + segment(`{}`) + ".c2ln", false},
		{"alg not a string", segment(`{"alg":1}`) + "." + segment(`{}`) + ".c2ln", false},
		{"payload not an object", segment(`{"alg":"HS256"}`) + "." + segment(`[1]`) + ".c2ln", false},
		{"empty signature", segment(`{"alg":"HS256"}`) + "." + segment(`{}`) + ".", false},
		{"two segments", segment(`{"alg":"HS256"}`) + "." + segment(`{}`), false},
		{"four segments", jwtToken() + ".x", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := len(secretKindsOf(t, "token "+tc.token+" end")) > 0
			if got != tc.want {
				t.Fatalf("detected = %t, want %t", got, tc.want)
			}
		})
	}
}

func TestPrivatePEM(t *testing.T) {
	text := "config:\n" + privateKey("PRIVATE KEY") + "\nfim " + privateKey("EC PRIVATE KEY")
	outcome, err := scanTexts(t, NewScanner(), policyOnly(Secret, ModeDetect), text)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Summary[0].Count != 2 {
		t.Fatalf("count = %d, want 2", outcome.Summary[0].Count)
	}

	legacy := "-----BEGIN " + "RSA PRIVATE KEY-----\nProc-Type: 4,ENCRYPTED\nDEK-Info: AES-128-CBC,00\n\nQUJDRA==\n-----END RSA PRIVATE KEY-----"
	if got := secretKindsOf(t, legacy); !slices.Equal(got, []string{KindPrivateKey}) {
		t.Fatalf("legacy encrypted kinds = %v", got)
	}
}

func TestSecretCandidateLimit(t *testing.T) {
	scanner := NewScanner(WithLimits(Limits{Deadline: DefaultLimits().Deadline, MaxCandidates: 10, MaxMapBytes: 1 << 10, MaxSecretBytes: 64}))
	_, err := scanTexts(t, scanner, policyOnly(Secret, ModeDetect), "sk-"+strings.Repeat("a", 100))
	if !isReason(err, ReasonCapacity) {
		t.Fatalf("err = %v, want capacity failure", err)
	}
}
