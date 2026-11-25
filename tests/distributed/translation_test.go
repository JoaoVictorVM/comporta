//go:build distributed

package distributed

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/JoaoVictorVM/comporta/internal/testkit"
)

// callTranslated sends an Anthropic request naming the OpenAI model, which
// the replica translates, optionally with a beta that no OpenAI model honors.
func (c *cluster) callTranslated(r *replica, beta bool) (int, http.Header) {
	content, _ := json.Marshal(testkit.Prompt("hello"))
	body := `{"model":"fake-gpt","max_tokens":16,"messages":[{"role":"user","content":` + string(content) + `}]}`
	request, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/v1/messages", r.proxyPort), strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", c.key)
	request.Header.Set("Anthropic-Version", "2023-06-01")
	if beta {
		request.Header.Set("Anthropic-Beta", "fake-beta-2026-01-01")
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		c.t.Fatal(err)
	}
	defer response.Body.Close()
	_, _ = io.ReadAll(response.Body)
	return response.StatusCode, response.Header
}

func (c *cluster) count(query string) int {
	c.t.Helper()
	var n int
	if err := c.db().QueryRow(query).Scan(&n); err != nil {
		c.t.Fatal(err)
	}
	return n
}

func TestDistributedTranslationSkipExactlyOnce(t *testing.T) {
	c := newCluster(t, 2, "apps:\n  bot:\n    allowed_models: [fake-claude, fake-gpt]\n    fallback: [fake-claude]")
	for i, r := range c.replicas {
		status, header := c.callTranslated(r, true)
		if status != 200 || header.Get("X-Comporta-Translated") != "" {
			t.Fatalf("replica %d: %d %v", i, status, header)
		}
	}
	// One skip per request, each written once by the replica that served it,
	// with no attempt, reservation or record of its own.
	if n := c.count(`SELECT count(*) FROM events WHERE type='translation_skipped' AND payload_json->>'feature'='provider_beta' AND attempt_index IS NULL`); n != 2 {
		t.Fatalf("skip events %d", n)
	}
	if n := c.count(`SELECT count(*) FROM requests WHERE model_used='fake-gpt'`); n != 0 {
		t.Fatalf("the skipped model has %d records", n)
	}
}

func TestDistributedTranslatedAttemptSettles(t *testing.T) {
	c := newCluster(t, 2, "apps:\n  bot:\n    allowed_models: [fake-claude, fake-gpt]")
	status, header := c.callTranslated(c.replicas[1], false)
	if status != 200 || header.Get("X-Comporta-Translated") != "openai->anthropic" {
		t.Fatalf("%d %v", status, header)
	}
	// The translated attempt is admitted and settled through the shared
	// session like any other: one record with the provider's own usage.
	deadline := 0
	for c.count(`SELECT count(*) FROM requests WHERE model_used='fake-gpt' AND request_count=1 AND output_tokens IS NOT NULL`) != 1 {
		if deadline++; deadline > 100 {
			t.Fatal("the translated attempt was not settled")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if n := c.count(`SELECT count(*) FROM limit_reservations WHERE state='reserved'`); n != 0 {
		t.Fatalf("%d reservations left open", n)
	}
}
