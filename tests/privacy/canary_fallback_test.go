package privacy

import (
	"database/sql"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

// TestFallbackPrivacyCanary sends marked prompts, sensitive values and user
// identifiers through a chain whose first model is overloaded, then scans every
// answer, log, file and database artifact the processes left. Each attempt and
// the fallback event carry configured model names and causes only.
func TestFallbackPrivacyCanary(t *testing.T) {
	run.markCase("process/fallback")
	g := startLiveFake(t, []string{"--scenario", "success", "--model-scenario", "fake-claude=overload"}, "", func(text string) string {
		text = detectionBlock(text)
		text = strings.Replace(text, "models: [fake-claude]", "models: [fake-claude, fake-haiku]", 1)
		text = strings.Replace(text, "pricing:\n", "pricing:\n  fake-haiku: {input: 1, output: 5, cache_write: 1.25, cache_read: 0.1}\n", 1)
		return strings.Replace(text, "  bot:\n    allowed_models: [fake-claude, fake-gpt]\n",
			"  bot:\n    allowed_models: [fake-claude, fake-gpt, fake-haiku]\n    fallback: [fake-haiku]\n", 1)
	})
	defer g.stop()
	run.markConstructor("AnthropicBody")
	run.markConstructor("AnthropicStreamBody")
	user := testkit.UserMarker()
	for name, body := range map[string]string{
		"fallback-json":      testkit.AnthropicBody(),
		"fallback-stream":    testkit.AnthropicStreamBody(),
		"fallback-sensitive": sensitiveAnthropic(sensitiveContent()),
	} {
		if status := g.send(name, "/v1/messages", g.key, user, body); status != http.StatusOK {
			t.Fatalf("%s: status %d", name, status)
		}
	}
	waitForRecords(t, g, 6)
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
		`SELECT count(*) FROM events WHERE type='fallback' AND json_extract(payload_json,'$.from_model')='fake-claude' AND json_extract(payload_json,'$.cause')='529' AND user_hash IS NULL`: 3,
		`SELECT count(*) FROM requests WHERE request_count=0 AND model_used='fake-claude'`:                                                                                                   3,
		`SELECT count(*) FROM requests WHERE request_count=1 AND json_extract(record_json,'$.FallbackFrom')='fake-claude'`:                                                                   3,
		`SELECT count(*) FROM request_operations WHERE state='closed' AND materialized=1`:                                                                                                    3,
		// The inspection belongs to the request: one event, not one per attempt.
		`SELECT count(*) FROM events WHERE type='sensitive_data_detected'`: 1,
	} {
		var n int
		if err = db.QueryRow(query).Scan(&n); err != nil || n != want {
			t.Fatalf("positive control: %s found %d, want %d (%v)", query, n, want, err)
		}
	}
}
