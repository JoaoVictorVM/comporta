package privacy

import (
	"database/sql"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

// TestTranslationPrivacyCanary sends marked prompts, sensitive values and user
// identifiers through a chain whose Anthropic model is overloaded, so every
// request is translated to the OpenAI model, plus one whose beta makes the
// next model ineligible. It then scans every answer, log, file and database
// artifact. Translated attempts and skips carry model names, formats and
// closed feature codes only.
func TestTranslationPrivacyCanary(t *testing.T) {
	run.markCase("process/translation")
	g := startLiveFake(t, []string{"--scenario", "success", "--model-scenario", "fake-claude=overload"}, "", func(text string) string {
		text = detectionBlock(text)
		return strings.Replace(text, "  bot:\n    allowed_models: [fake-claude, fake-gpt]\n",
			"  bot:\n    allowed_models: [fake-claude, fake-gpt]\n    fallback: [fake-gpt]\n", 1)
	})
	defer g.stop()
	user := testkit.UserMarker()
	prompt := testkit.Prompt("Ola")
	for name, request := range map[string]struct{ path, body string }{
		"translation-json":      {"/v1/messages", `{"model":"fake-claude","max_tokens":32,"messages":[{"role":"user","content":"` + prompt + `"}]}`},
		"translation-stream":    {"/v1/messages", `{"model":"fake-claude","max_tokens":32,"stream":true,"messages":[{"role":"user","content":"` + prompt + `"}]}`},
		"translation-responses": {"/v1/responses", `{"model":"fake-claude","input":"` + prompt + `","stream":true}`},
		"translation-sensitive": {"/v1/messages", sensitiveAnthropic(sensitiveContent())},
	} {
		if status := g.send(name, request.path, g.key, user, request.body); status != http.StatusOK {
			t.Fatalf("%s: status %d", name, status)
		}
	}
	beta := map[string]string{"Anthropic-Beta": "fake-beta-2026-01-01", "X-Comporta-User": user}
	if status := g.sendWith("translation-skip", "/v1/messages", g.key, `{"model":"fake-claude","max_tokens":32,"messages":[{"role":"user","content":"`+prompt+`"}]}`, beta); status != 529 {
		t.Fatalf("skip: status %d", status)
	}
	waitForRecords(t, g, 9)
	g.stop()
	path := filepath.Join(g.dir, "data", "usage.sqlite")
	local := sensitiveScanner()
	local.ScanFile("database", path)
	requireClean(t, local, "database")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for query, want := range map[string]int{
		`SELECT count(*) FROM requests WHERE model_used='fake-gpt' AND request_count=1`:                                                                 4,
		`SELECT count(*) FROM events WHERE type='fallback' AND json_extract(payload_json,'$.to_model')='fake-gpt'`:                                      4,
		`SELECT count(*) FROM events WHERE type='translation_skipped' AND json_extract(payload_json,'$.feature')='provider_beta' AND user_hash IS NULL`: 1,
	} {
		var n int
		if err = db.QueryRow(query).Scan(&n); err != nil || n != want {
			t.Fatalf("positive control: %s found %d, want %d (%v)", query, n, want, err)
		}
	}
}
